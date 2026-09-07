// Package mirror: watcher polls the DB for new sessions and pairs them with pending markers.
package mirror

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/devpass/devpass-discord-relay/internal/inject"
	"github.com/devpass/devpass-discord-relay/internal/transcript"
)

// Config holds relay watcher configuration.
type Config struct {
	// Discord
	DiscordToken  string
	DiscordAPIURL string // override for testing
	ChannelID     string
	AllowedUserID string // only honour messages from this Discord user ID (DISCORD_USER_ID)

	// DB
	DBPath string

	// State
	StateDir string

	// Binaries
	// DpmuxBin is the path to the dpmux binary (default: "dpmux" on PATH).
	// Used when spawning a new session from a Discord channel message.
	DpmuxBin string

	// Timing
	PollInterval time.Duration // default 1s

	// SpawnBindTimeout is how long to wait for the DB session row after dpmux spawn.
	// Default 60s.
	SpawnBindTimeout time.Duration
}

// DiscordClient is the interface the watcher uses to talk to Discord.
type DiscordClient interface {
	SendMessage(channelID, content string) (MsgRef, error)
	EditMessage(channelID, messageID, content string) error
	CreateThread(channelID, messageID, name string) (ThreadRef, error)
	SendTyping(channelID string) error
	RenameThread(threadID, name string) error
	// OnMessage registers a callback invoked for every MESSAGE_CREATE gateway event.
	// Multiple handlers may be registered; each receives a copy of the event.
	OnMessage(fn MessageHandler)
}

// MessageEvent carries the fields of a Discord MESSAGE_CREATE event that the
// inject path needs.
type MessageEvent struct {
	ChannelID   string
	AuthorID    string
	AuthorIsBot bool
	Content     string
	Attachments []MessageAttachment
}

// MessageAttachment is a single file attachment on a Discord message.
type MessageAttachment struct {
	Filename string
	URL      string
}

// MessageHandler is called for every incoming Discord message.
type MessageHandler func(ev MessageEvent)

// MsgRef is a minimal message reference.
type MsgRef struct {
	ID        string
	ChannelID string
}

// ThreadRef is a minimal thread reference.
type ThreadRef struct {
	ID string
}

// Watcher polls the DB and mirrors sessions to Discord.
type Watcher struct {
	cfg      Config
	db       *sql.DB
	discord  DiscordClient
	sessions *Sessions
	throttle *Throttle
	injector *inject.Injector
	// runCtx is cancelled when the relay is shutting down.  Discord's REST
	// interface has no context parameter, so the relay-owned 429 wait must use
	// this context rather than an unconditional Sleep.
	runCtx context.Context
	// claimed guards against double-binding a session from two concurrent paths
	// (scanSessions and handleSpawnFromDiscord). A session ID is inserted here
	// the first time it is claimed; subsequent claimants are rejected.
	claimed claimSet
	// pendingResume holds per-thread pending !resume <n> candidate lists.
	pendingResume pendingResumeStore
	// suppressedEchoes tracks (sessionID, promptText) pairs whose Discord echo
	// has already been posted once. A second appearance of the same prompt in
	// the DB (due to inject retry) must not be posted again.
	suppressedEchoes suppressedEchoStore
}

// suppressedEchoStore tracks prompt texts that have already been echoed to
// Discord for a given session, so the retry path can suppress duplicates.
type suppressedEchoStore struct {
	mu      sync.Mutex
	entries map[string]map[string]bool // sessionID -> set of prompt texts
}

func (s *suppressedEchoStore) add(sessionID, prompt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[string]map[string]bool)
	}
	if s.entries[sessionID] == nil {
		s.entries[sessionID] = make(map[string]bool)
	}
	s.entries[sessionID][prompt] = true
}

func (s *suppressedEchoStore) has(sessionID, prompt string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		return false
	}
	return s.entries[sessionID][prompt]
}

// claimSet is a thread-safe set of claimed session IDs.
type claimSet struct {
	mu  sync.Mutex
	ids map[string]bool
}

func (c *claimSet) init() {
	c.ids = make(map[string]bool)
}

// tryAdd returns true and inserts id if it was not already present.
// Returns false (no-op) if already claimed.
func (c *claimSet) tryAdd(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ids == nil {
		c.ids = make(map[string]bool)
	}
	if c.ids[id] {
		return false
	}
	c.ids[id] = true
	return true
}

// copyInto copies all claimed IDs into dst (non-destructive read).
func (c *claimSet) copyInto(dst map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.ids {
		dst[id] = true
	}
}

// NewWatcher creates a watcher. db must already be open read-only.
func NewWatcher(cfg Config, db *sql.DB, discord DiscordClient) (*Watcher, error) {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 1 * time.Second
	}
	if cfg.SpawnBindTimeout == 0 {
		cfg.SpawnBindTimeout = 60 * time.Second
	}
	sessPath := filepath.Join(cfg.StateDir, "sessions.json")
	sessions, err := LoadSessions(sessPath)
	if err != nil {
		return nil, fmt.Errorf("mirror: load sessions: %w", err)
	}
	w := &Watcher{
		cfg:      cfg,
		db:       db,
		discord:  discord,
		sessions: sessions,
		throttle: NewThrottle(5, 5*time.Second),
		injector: inject.New(cfg.StateDir),
		runCtx:   context.Background(),
	}

	// Register incoming message handler for thread replies.
	discord.OnMessage(w.handleDiscordMessage)

	return w, nil
}

// NewWatcherForTest builds a minimal Watcher around a real DiscordClient
// (e.g. wrapping the production discord.New() client) for tests that need to
// exercise the relay's own 429 retry/throttle path (discordSend) without a
// full DB/tmux/dpmux fixture. It uses an in-memory sessions store and default
// throttle settings, matching what NewWatcher configures.
func NewWatcherForTest(discord DiscordClient) (*Watcher, error) {
	return &Watcher{
		discord:  discord,
		sessions: &Sessions{records: make(map[string]*SessionRecord)},
		throttle: NewThrottle(5, 5*time.Second),
		runCtx:   context.Background(),
	}, nil
}

// DiscordSendForTest is the exported form of discordSend used by tests that
// need to exercise the relay's bounded, context-cancellable 429 retry path
// against a real DiscordClient (see discordSend for the actual logic: on a
// rate-limit error it extracts RetryAfter, sleeps, and retries exactly once).
func (w *Watcher) DiscordSendForTest(channelID, content string) (MsgRef, error) {
	return w.discordSend(channelID, content)
}

// Run starts the watcher loop until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	w.runCtx = ctx
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()
	// Track known session IDs to avoid re-pairing
	seen := make(map[string]bool)
	// Pre-seed with already-tracked sessions
	for _, r := range w.sessions.All() {
		seen[r.SessionID] = true
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Merge any sessions claimed by handleSpawnFromDiscord into seen.
			w.claimed.copyInto(seen)
			w.scanSessions(ctx, seen)
		}
	}
}

// scanSessions checks for new non-subagent sessions and pairs them with pending markers.
func (w *Watcher) scanSessions(ctx context.Context, seen map[string]bool) {
	rows, err := w.db.QueryContext(ctx,
		`SELECT id, time_created, title FROM session WHERE parent_id IS NULL ORDER BY time_created ASC`)
	if err != nil {
		return
	}
	defer rows.Close()

	type sessionRow struct {
		id      string
		created int64
		title   string
	}

	var sessions []sessionRow
	for rows.Next() {
		var r sessionRow
		if err := rows.Scan(&r.id, &r.created, &r.title); err != nil {
			continue
		}
		sessions = append(sessions, r)
	}
	_ = rows.Close()

	for _, s := range sessions {
		if seen[s.id] {
			continue
		}
		// Try to pair with a pending marker
		target, pendingPath, ok := findPendingMarker(w.cfg.StateDir, s.id, s.created)
		if !ok {
			// Unmatched session – ignore per spec
			continue
		}
		// Atomically claim this session to prevent double-binding with handleSpawnFromDiscord.
		if !w.claimed.tryAdd(s.id) {
			// Already claimed by the spawn path; mark as seen so we stop trying.
			seen[s.id] = true
			continue
		}
		seen[s.id] = true

		// Claim this session
		go func(sessionID, tmuxTarget, pendingPath string, createdAt int64) {
			defer os.Remove(pendingPath) // clean up marker

			if err := w.mirrorSession(ctx, sessionID, tmuxTarget, createdAt); err != nil {
				log.Printf("mirror: session %s error: %v", sessionID, err)
			}
		}(s.id, target, pendingPath, s.created)
	}
}

// spawnMarkerJSON is the JSON format of a pending marker written by dpmux.
//
// StderrLog is the phase-16-verified production identity evidence: the exact
// tmux pane's devpass-code process (launched with --print-logs --log-level
// INFO) has its stderr redirected exclusively to this file. When it creates a
// new session row, it logs "message=created id=ses_<ID>" there — a line no
// other process can write, because the file belongs to this one launch only.
// See docs/BINDING-PROTOCOL.md.
type spawnMarkerJSON struct {
	Target       string `json:"target"`
	Nonce        string `json:"nonce"` // legacy; not used to prove identity
	SpawnInstant int64  `json:"spawnInstant"`
	StderrLog    string `json:"stderrLog"`
	// SessionID is set only for an exact-id resume.  A resume has no new
	// session-creation event to log, so its requested historical id is the
	// identity evidence; it must never be matched to a newly discovered row.
	SessionID string `json:"sessionID,omitempty"`
}

// findPendingMarker looks for a pending-<name> marker file in stateDir that
// can be proven to own sessionID.
//
// JSON markers with a StderrLog field are matched using the phase-16-verified
// production identity protocol: the marker's StderrLog file must contain
// "message=created id=<sessionID>" (see extractCreatedSessionID), and the
// session must have been created at or after the marker's spawnInstant.
// Neither the legacy nonce/title field nor any other heuristic is accepted as
// proof for these markers; a JSON marker with no StderrLog cannot be matched.
// Exact-id resume markers bind only their requested historical SessionID. New
// spawn markers bind only the id in their exclusive stderr log. Plain-text
// markers retain only the legacy pre-existing-session attachment behavior;
// they cannot bind a row created after the marker (the unsafe fallback).
func findPendingMarker(stateDir string, sessionID string, createdAt int64) (target, pendingPath string, ok bool) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return "", "", false
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "pending-") {
			continue
		}
		fp := filepath.Join(stateDir, e.Name())
		data, err := os.ReadFile(fp)
		if err != nil {
			continue
		}

		// Try parsing as JSON.
		var m spawnMarkerJSON
		if jsonErr := json.Unmarshal(data, &m); jsonErr != nil || m.Target == "" {
			legacyTarget := strings.TrimSpace(string(data))
			info, infoErr := e.Info()
			// Compatibility only for sessions that predate a plain marker (the
			// historic attachment fixture). This deliberately excludes every
			// post-marker/new-spawn row, so a timestamp is never used to claim a
			// newly created foreign transcript.
			if legacyTarget != "" && infoErr == nil && createdAt <= info.ModTime().UnixMilli() && isTmuxPaneAlive(legacyTarget) {
				return legacyTarget, fp, true
			}
			continue
		}
		if m.SessionID != "" {
			// A resumed CLI is invoked with this exact id and does not create a
			// new row, so timestamps and pane liveness are deliberately irrelevant
			// here. This preserves a known historical session whose old pane died:
			// it is attached by exact identity and then resumed, never inferred
			// from any arbitrary post-spawn row.
			if m.SessionID == sessionID {
				// A pending historical session may intentionally point at a
				// dead pane so the inbound path can resume it. Markers written
				// by dpmux carry a creation-time StderrLog; ad-hoc legacy
				// markers do not, so retain their dead-target resume behavior.
				if m.StderrLog == "" || isTmuxPaneAlive(m.Target) {
					return m.Target, fp, true
				}
			}
			continue
		}
		// Liveness is a constraint on verified new-spawn identity, not a
		// substitute for it. Check it immediately before returning the marker
		// for a claim so a pane that died while the DB was polled cannot attach
		// a foreign row.
		if !isTmuxPaneAlive(m.Target) {
			continue
		}
		if createdAt < m.SpawnInstant || m.StderrLog == "" {
			continue
		}
		loggedID, logOK := extractCreatedSessionID(m.StderrLog)
		if logOK && loggedID == sessionID {
			return m.Target, fp, true
		}
	}
	return "", "", false
}

// mirrorSession mirrors a single session to Discord.
func (w *Watcher) mirrorSession(ctx context.Context, sessionID, tmuxTarget string, createdAt int64) error {
	log.Printf("mirror: attaching session %s target=%s", sessionID, tmuxTarget)

	// Create thread root message
	rootContent := fmt.Sprintf("🟢 **Session** `%s`\nTarget: `%s`", sessionID, tmuxTarget)
	rootMsg, err := w.discord.SendMessage(w.cfg.ChannelID, rootContent)
	if err != nil {
		return fmt.Errorf("create root message: %w", err)
	}

	// Create thread
	thread, err := w.discord.CreateThread(w.cfg.ChannelID, rootMsg.ID, sessionID)
	if err != nil {
		return fmt.Errorf("create thread: %w", err)
	}

	threadID := thread.ID

	// Persist the session record
	rec := &SessionRecord{
		SessionID:    sessionID,
		TmuxTarget:   tmuxTarget,
		ThreadID:     threadID,
		ThreadRootID: rootMsg.ID,
		ChannelID:    w.cfg.ChannelID,
		Status:       "active",
	}
	if err := w.sessions.Upsert(rec); err != nil {
		log.Printf("mirror: sessions.json upsert: %v", err)
	}

	// Mirror the session content
	if err := w.mirrorContent(ctx, sessionID, threadID, tmuxTarget); err != nil {
		log.Printf("mirror: mirrorContent error: %v", err)
	}

	// Post session-ended marker
	endContent := "⏹ Session ended"
	if _, postErr := w.discord.SendMessage(threadID, endContent); postErr != nil {
		log.Printf("mirror: post session-ended: %v", postErr)
	}

	// Flip thread-root emoji
	updatedRoot := fmt.Sprintf("⚫ **Session** `%s`\nTarget: `%s`", sessionID, tmuxTarget)
	if editErr := w.discord.EditMessage(w.cfg.ChannelID, rootMsg.ID, updatedRoot); editErr != nil {
		log.Printf("mirror: flip root emoji: %v", editErr)
	}

	// Keep sessions.json claim (status=ended)
	rec.Status = "ended"
	_ = w.sessions.Upsert(rec)

	return nil
}

// msgState tracks per-message relay state.
type msgState struct {
	role          string
	discordMsgID  string   // current edit-target Discord message ID for assistant text
	discordMsgIDs []string // all Discord message IDs posted for this assistant text (for softening)
	text          string   // accumulated text so far
	toolCalls     map[string]*toolState
	isPreamble    bool
	finishSeen    bool
	promptEchoed  bool
	hadOutput     bool
}

// toolState tracks per-tool-call state.
type toolState struct {
	callID   string
	toolName string
	msgID    string // Discord message ID for the embed
	done     bool
	failed   bool
}

// mirrorContent tails the DB and mirrors content to the Discord thread.
func (w *Watcher) mirrorContent(ctx context.Context, sessionID, threadID, tmuxTarget string) error {
	tailer := transcript.NewTailer(w.db, sessionID, 250*time.Millisecond)

	// First-sight replay: start from 0 to pick up any existing content
	ch, err := tailer.Tail(ctx, 0)
	if err != nil {
		return err
	}

	// Typing ticker
	typingTicker := time.NewTicker(8 * time.Second)
	defer typingTicker.Stop()

	// Liveness check for the tmux target
	livenessTicker := time.NewTicker(5 * time.Second)
	defer livenessTicker.Stop()

	// Map from DB message_id → msgState
	msgs := make(map[string]*msgState)
	// Track last assistant message ID for turn-no-output detection
	var lastAsstMsgID string

	sendTyping := func() {
		if err := w.discord.SendTyping(threadID); err != nil {
			log.Printf("mirror: typing: %v", err)
		}
	}
	sendTyping()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-typingTicker.C:
			sendTyping()

		case <-livenessTicker.C:
			if tmuxTarget != "" && !isTmuxPaneAlive(tmuxTarget) {
				log.Printf("mirror: tmux pane %s died", tmuxTarget)
				return nil
			}

		case part, ok := <-ch:
			if !ok {
				return nil
			}

			// Look up or create message state
			ms, exists := msgs[part.MessageID]
			if !exists {
				// Fetch the message role from DB
				role, isPreamble := w.fetchMessageRole(ctx, part.MessageID)
				ms = &msgState{
					role:       role,
					toolCalls:  make(map[string]*toolState),
					isPreamble: isPreamble,
				}
				msgs[part.MessageID] = ms
				if role == "assistant" {
					lastAsstMsgID = part.MessageID
				}
			}

			cp := ClassifyPart(part, ms.role, ms.isPreamble)

			switch cp.Kind {
			case PartKindPreamble:
				// Skip preamble parts – do NOT count as user prompts

			case PartKindText:
				if ms.role == "user" && !ms.isPreamble && !ms.promptEchoed {
					// Echo user prompt (truncated to 300 chars)
					prompt := cp.Text
					runes := []rune(prompt)
					if len(runes) > 300 {
						prompt = string(runes[:297]) + "..."
					}
					if prompt != "" {
						ms.promptEchoed = true
						// Suppress duplicate echo: if injectAndVerify already echoed this
						// prompt (because it had to retry the injection), skip the second
						// Discord post.  Use the raw cp.Text for the suppression key (before
						// truncation) so the check is length-invariant.
						if !w.suppressedEchoes.has(sessionID, cp.Text) {
							w.sendToThread(threadID, fmt.Sprintf("**You:** %s", prompt))
						} else {
							log.Printf("mirror: suppressed duplicate echo for session %s", sessionID)
						}
					}
				} else if ms.role == "assistant" {
					// Accumulate and stream assistant text
					ms.text += cp.Text
					ms.hadOutput = true
					w.updateTextMessage(threadID, ms)
				}

			case PartKindToolCall:
				if ms.role == "assistant" {
					w.handleToolPart(threadID, cp, ms)
				}

			case PartKindStepStart:
				// nothing to mirror

			case PartKindStepFinish:
				if ms.role == "assistant" {
					// Flush remaining text
					if ms.text != "" && ms.discordMsgID == "" {
						// Text was not yet posted
						w.sendToThread(threadID, ms.text)
						ms.text = ""
					}
					ms.finishSeen = true
					// "Turn finished without output"
					if !ms.hadOutput && lastAsstMsgID == part.MessageID {
						w.sendToThread(threadID, "_Turn finished without output_")
					}
					// Soften any failed tool embeds in-place: flip ❌ failed → ⚠️ warning
					for _, ts := range ms.toolCalls {
						if ts.failed && ts.msgID != "" {
							content := formatToolEmbedSoftened(ts.toolName, ts.callID)
							w.throttle.Wait(threadID)
							if err := w.discordEdit(threadID, ts.msgID, content); err != nil {
								log.Printf("mirror: soften tool embed: %v", err)
							}
						}
					}
					// Summary line
					toolCount := len(ms.toolCalls)
					w.sendToThread(threadID, fmt.Sprintf("_— %d tool(s)_", toolCount))
				}
			}
		}
	}
}

// fetchMessageRole queries the DB for the role and preamble status of a message.
func (w *Watcher) fetchMessageRole(ctx context.Context, messageID string) (role string, isPreamble bool) {
	var data string
	err := w.db.QueryRowContext(ctx, `SELECT data FROM message WHERE id = ?`, messageID).Scan(&data)
	if err != nil {
		return "unknown", false
	}
	var md struct {
		Role  string `json:"role"`
		Agent string `json:"agent,omitempty"`
	}
	if err := json.Unmarshal([]byte(data), &md); err != nil {
		return "unknown", false
	}
	// Preamble: user message with agent context (harness-injected)
	isPreamble = md.Role == "user" && md.Agent != ""
	return md.Role, isPreamble
}

// sendToThread sends a message to the thread.
func (w *Watcher) sendToThread(threadID, content string) {
	w.throttle.Wait(threadID)
	if _, err := w.discordSend(threadID, content); err != nil {
		log.Printf("mirror: sendToThread: %v", err)
	}
}

// updateTextMessage creates or edits assistant text Discord messages.
//
// Invariant: ms.discordMsgIDs[i] is the Discord message ID for chunk i.
// ms.discordMsgID is the current streaming target (the last ID in discordMsgIDs).
//
// Streaming behavior:
//   - While text fits in one chunk, the same message is edited in place.
//   - When text overflows 2000 chars, the current message is sealed (edited with
//     the final split point) and a new follow-up message is created for the remainder.
//   - This repeats for each additional overflow, so K chunks → K Discord messages.
func (w *Watcher) updateTextMessage(threadID string, ms *msgState) {
	chunks := ChunkText(ms.text, 2000)
	if len(chunks) == 0 {
		return
	}

	N := len(ms.discordMsgIDs) // Discord messages already created
	K := len(chunks)           // chunks needed now

	if N == 0 {
		// First call: create the first Discord message.
		w.throttle.Wait(threadID)
		msg, err := w.discordSend(threadID, chunks[0])
		if err != nil {
			log.Printf("mirror: create text chunk 0: %v", err)
			return
		}
		ms.discordMsgIDs = append(ms.discordMsgIDs, msg.ID)
		ms.discordMsgID = msg.ID
		N = 1
		// Fall through: if K > 1, we need to create more messages below.
	}

	if K > N {
		// We've overflowed into more chunks. Seal all existing messages with their
		// final chunk content, then create messages for any new chunks.

		// Step 1: Seal all existing messages (they may have been written with partial text).
		for i := 0; i < N; i++ {
			w.throttle.Wait(threadID)
			if err := w.discordEdit(threadID, ms.discordMsgIDs[i], chunks[i]); err != nil {
				log.Printf("mirror: seal text chunk %d: %v", i, err)
			}
		}

		// Step 2: Create messages for any remaining chunks.
		for i := N; i < K; i++ {
			w.throttle.Wait(threadID)
			msg, err := w.discordSend(threadID, chunks[i])
			if err != nil {
				log.Printf("mirror: create text chunk %d: %v", i, err)
				return
			}
			ms.discordMsgIDs = append(ms.discordMsgIDs, msg.ID)
			ms.discordMsgID = msg.ID
		}
	} else {
		// Same number of chunks as before (K == N, since text only grows).
		// Just edit the live (last) message with the latest content.
		lastIdx := K - 1
		w.throttle.Wait(threadID)
		if err := w.discordEdit(threadID, ms.discordMsgIDs[lastIdx], chunks[lastIdx]); err != nil {
			log.Printf("mirror: edit text chunk %d: %v", lastIdx, err)
		}
		ms.discordMsgID = ms.discordMsgIDs[lastIdx]
	}
}

// discordSend sends a message with 429 retry logic using the relay lifecycle
// context.  A shutdown can therefore interrupt a RetryAfter wait promptly.
func (w *Watcher) discordSend(channelID, content string) (MsgRef, error) {
	return w.discordSendContext(w.runCtx, channelID, content)
}

func (w *Watcher) discordSendContext(ctx context.Context, channelID, content string) (MsgRef, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	msg, err := w.discord.SendMessage(channelID, content)
	if err == nil {
		return msg, nil
	}
	if retryAfter, ok := extractRetryAfter(err); ok {
		log.Printf("mirror: 429 rate-limit on send, retry after %v", retryAfter)
		w.throttle.SetBackoff(channelID, retryAfter)
		defer w.throttle.ClearBackoff(channelID)
		if err := waitForRetryAfter(ctx, retryAfter); err != nil {
			return MsgRef{}, err
		}
		return w.discord.SendMessage(channelID, content)
	}
	return MsgRef{}, err
}

// discordEdit edits a message with 429 retry logic.
func (w *Watcher) discordEdit(channelID, messageID, content string) error {
	return w.discordEditContext(w.runCtx, channelID, messageID, content)
}

func (w *Watcher) discordEditContext(ctx context.Context, channelID, messageID, content string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	err := w.discord.EditMessage(channelID, messageID, content)
	if err == nil {
		return nil
	}
	if retryAfter, ok := extractRetryAfter(err); ok {
		log.Printf("mirror: 429 rate-limit on edit, retry after %v", retryAfter)
		w.throttle.SetBackoff(channelID, retryAfter)
		defer w.throttle.ClearBackoff(channelID)
		if err := waitForRetryAfter(ctx, retryAfter); err != nil {
			return err
		}
		return w.discord.EditMessage(channelID, messageID, content)
	}
	return err
}

// waitForRetryAfter waits for a relay-owned rate-limit delay without making
// shutdown wait for the complete RetryAfter duration.
func waitForRetryAfter(ctx context.Context, retryAfter time.Duration) error {
	if retryAfter <= 0 {
		return nil
	}
	timer := time.NewTimer(retryAfter)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// extractRetryAfter extracts the retry_after duration from a rate-limit error.
// It checks for:
//  1. *discordgo.RateLimitError (real Discord 429 responses via discordgo)
//  2. retryAfterDurationGetter interface (test doubles)
//  3. String fallback: "retry after <duration>" in error message
func extractRetryAfter(err error) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}

	// Primary path: discordgo typed rate-limit error.
	var rle *discordgo.RateLimitError
	if errors.As(err, &rle) && rle.RateLimit != nil {
		return rle.RetryAfter, true
	}

	// Secondary path: test double that implements RetryAfterDuration().
	type retryHolder interface {
		RetryAfterDuration() time.Duration
	}
	var rh retryHolder
	if errors.As(err, &rh) {
		return rh.RetryAfterDuration(), true
	}

	// Fallback: parse the error string for "retry after <duration>".
	msg := err.Error()
	const prefix = "retry after "
	if idx := strings.Index(msg, prefix); idx >= 0 {
		rest := msg[idx+len(prefix):]
		// Trim at first space or end.
		end := len(rest)
		for i, c := range rest {
			if c == ' ' || c == ',' {
				end = i
				break
			}
		}
		if d, parseErr := time.ParseDuration(rest[:end]); parseErr == nil {
			return d, true
		}
	}
	return 0, false
}

// handleToolPart updates the tool embed for a tool-call part.
func (w *Watcher) handleToolPart(threadID string, cp ClassifiedPart, ms *msgState) {
	toolKey := cp.ToolID
	if toolKey == "" {
		toolKey = cp.ToolName
	}

	ts, exists := ms.toolCalls[toolKey]
	if !exists {
		// Post tool embed
		content := formatToolEmbed(cp.ToolName, cp.ToolID, false, false)
		w.throttle.Wait(threadID)
		toolMsg, sendErr := w.discordSend(threadID, content)
		if sendErr != nil {
			log.Printf("mirror: tool embed: %v", sendErr)
			return
		}
		ts = &toolState{
			callID:   cp.ToolID,
			toolName: cp.ToolName,
			msgID:    toolMsg.ID,
		}
		ms.toolCalls[toolKey] = ts
	}

	// Parse tool state to check completion
	var raw struct {
		State struct {
			Status string `json:"status"`
		} `json:"state"`
	}
	if err := json.Unmarshal(cp.Data, &raw); err != nil {
		return
	}

	status := raw.State.Status
	if status == "completed" && !ts.done {
		ts.done = true
		content := formatToolEmbed(ts.toolName, ts.callID, true, false)
		w.throttle.Wait(threadID)
		if err := w.discordEdit(threadID, ts.msgID, content); err != nil {
			log.Printf("mirror: update tool embed done: %v", err)
		}
	} else if status == "error" && !ts.failed {
		ts.failed = true
		content := formatToolEmbed(ts.toolName, ts.callID, false, true)
		w.throttle.Wait(threadID)
		if err := w.discordEdit(threadID, ts.msgID, content); err != nil {
			log.Printf("mirror: update tool embed error: %v", err)
		}
	}
}

// formatToolEmbed formats a tool call as a Discord message.
func formatToolEmbed(toolName, callID string, done, failed bool) string {
	status := "⏳ running"
	if done {
		status = "✅ done"
	}
	if failed {
		status = "❌ failed"
	}
	return fmt.Sprintf("🔧 **Tool:** `%s` | %s | ID: `%s`", toolName, status, callID)
}

// formatToolEmbedSoftened formats a previously-failed tool embed as a warning
// (used when step-finish arrives clean, softening the error in-place).
func formatToolEmbedSoftened(toolName, callID string) string {
	return fmt.Sprintf("🔧 **Tool:** `%s` | ⚠️ warning | ID: `%s` | turn completed", toolName, callID)
}

// isTmuxPaneAlive checks if a tmux pane target is still alive.
// Uses list-panes -t <target> (window-level check) rather than has-session,
// so a dead pane inside a live session is correctly detected.
func isTmuxPaneAlive(target string) bool {
	err := exec.Command("tmux", "list-panes", "-t", target).Run()
	return err == nil
}

// handleDiscordMessage is registered with the Discord client to receive
// MESSAGE_CREATE gateway events. It dispatches thread replies to the injector.
func (w *Watcher) handleDiscordMessage(ev MessageEvent) {
	// Drop bots and non-allowed authors immediately.
	if !inject.ShouldAcceptAuthor(ev.AuthorID, ev.AuthorIsBot, w.cfg.AllowedUserID) {
		return
	}

	// A message in the relay channel (not a thread) triggers a new session spawn.
	// Check this before checking known threads so the channel message gets spawned
	// even if we somehow also manage a thread with the same ID (impossible in practice,
	// but correctness requires the channel-message path to be checked first).
	if ev.ChannelID == w.cfg.ChannelID {
		log.Printf("mirror: channel message received – spawning new dpmux session")
		go func(content string, atts []MessageAttachment) {
			// Use a background context so the spawn survives past handleDiscordMessage.
			if err := w.handleSpawnFromDiscord(context.Background(), content, atts); err != nil {
				log.Printf("mirror: spawn from Discord: %v", err)
			}
		}(ev.Content, ev.Attachments)
		return
	}

	// Find the session that owns this thread.
	var rec *SessionRecord
	for _, r := range w.sessions.All() {
		if r.ThreadID == ev.ChannelID {
			rec = r
			break
		}
	}
	if rec == nil {
		// Not a thread we manage directly, but it may have a pending !resume <n>
		// entry (e.g., from handleAmbiguousResume on a now-ended session).
		// Route through the real handler so the choice is never silently dropped.
		w.handleResumeChoiceForThread(ev.ChannelID, ev.Content)
		return
	}

	log.Printf("mirror: handleDiscordMessage: channel=%s session=%s", ev.ChannelID, rec.SessionID)

	// Check for pending !resume <n> choice first (takes priority).
	if w.handleResumeChoiceForThread(ev.ChannelID, ev.Content) {
		return
	}

	// Handle !quit command: teardown pane but keep the claim.
	if strings.TrimSpace(ev.Content) == "!quit" {
		log.Printf("mirror: !quit received for session %s", rec.SessionID)
		go w.teardownPane(rec.TmuxTarget)
		return
	}

	// Handle bare `!resume` (no number): show candidate list.
	if strings.TrimSpace(ev.Content) == "!resume" {
		go w.handleAmbiguousResume(rec, ev.Content, nil)
		return
	}

	// Handle !title command.
	if title, ok := inject.HandleTitleCommand(ev.Content); ok {
		if err := w.discord.RenameThread(rec.ThreadID, title); err != nil {
			log.Printf("mirror: rename thread %s: %v", rec.ThreadID, err)
		}
		return
	}

	// Build attachment list from image attachments.
	var attachments []inject.Attachment
	for _, att := range ev.Attachments {
		attachments = append(attachments, inject.Attachment{
			Filename: att.Filename,
			URL:      att.URL,
		})
	}

	// If the pane is dead, attempt a resume before injecting.
	if rec.TmuxTarget != "" && !isTmuxPaneAlive(rec.TmuxTarget) {
		log.Printf("mirror: pane %s is dead for session %s – attempting resume", rec.TmuxTarget, rec.SessionID)
		go func(r *SessionRecord, content string, atts []inject.Attachment) {
			if err := w.resumeDeadSession(context.Background(), r, content, atts); err != nil {
				log.Printf("mirror: resumeDeadSession %s: %v", r.SessionID, err)
				w.sendToThread(r.ThreadID, fmt.Sprintf("⚠️ Could not resume session: %v", err))
			}
		}(rec, ev.Content, attachments)
		return
	}

	// Inject in a goroutine so we don't block the gateway event loop.
	go func(target, sessionID, threadID, content string, atts []inject.Attachment) {
		if err := w.injectAndVerify(context.Background(), target, sessionID, threadID, content, atts); err != nil {
			log.Printf("mirror: inject session %s: %v", sessionID, err)
		}
	}(rec.TmuxTarget, rec.SessionID, rec.ThreadID, ev.Content, attachments)
}

// ── Resume dead sessions ───────────────────────────────────────────────────────

// echoVerifyTimeout is how long to wait for the injected prompt to appear in the DB.
const echoVerifyTimeout = 30 * time.Second

// pendingResumeStore tracks pending !resume <n> candidate lists per Discord thread.
type pendingResumeStore struct {
	mu      sync.Mutex
	entries map[string]*pendingResumeEntry // keyed by threadID
}

type pendingResumeEntry struct {
	candidates []resumeCandidate
	content    string
	atts       []inject.Attachment
	rec        *SessionRecord
}

type resumeCandidate struct {
	SessionID  string
	ProviderID string // model provider info if available
	LastActive int64  // time_updated from DB (ms)
}

func (p *pendingResumeStore) init() {
	p.entries = make(map[string]*pendingResumeEntry)
}

func (p *pendingResumeStore) set(threadID string, e *pendingResumeEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries == nil {
		p.entries = make(map[string]*pendingResumeEntry)
	}
	p.entries[threadID] = e
}

func (p *pendingResumeStore) get(threadID string) (*pendingResumeEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[threadID]
	return e, ok
}

func (p *pendingResumeStore) delete(threadID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, threadID)
}

// teardownPane kills the process group of the tmux pane's process, then kill-window.
// This is the canonical teardown used by !quit and refusal handling.
// The claim in sessions.json is kept.
// Only the window is killed, not the whole session, to be surgical.
func (w *Watcher) teardownPane(tmuxTarget string) {
	teardownPaneTarget(tmuxTarget)
}

// TeardownPane is the exported variant used in tests.
func TeardownPane(tmuxTarget string) {
	teardownPaneTarget(tmuxTarget)
}

// teardownPaneTarget implements the actual teardown logic.
// Sends SIGTERM to the pane's process group, then kills the tmux window only.
func teardownPaneTarget(tmuxTarget string) {
	if tmuxTarget == "" {
		return
	}
	log.Printf("mirror: teardown pane %s", tmuxTarget)

	// Get the PID of the pane's foreground process group leader.
	out, err := exec.Command("tmux", "display-message", "-t", tmuxTarget, "-p", "#{pane_pid}").Output()
	if err == nil {
		pid := strings.TrimSpace(string(out))
		if pid != "" {
			// Send SIGTERM to the process group (negative pid).
			exec.Command("kill", "-TERM", "--", "-"+pid).Run() //nolint:errcheck
			time.Sleep(300 * time.Millisecond)
		}
	}

	// Kill only the window (never the whole session) — surgical teardown.
	windowTarget := tmuxTarget
	if idx := strings.LastIndex(tmuxTarget, "."); idx >= 0 {
		windowTarget = tmuxTarget[:idx]
	}
	exec.Command("tmux", "kill-window", "-t", windowTarget).Run() //nolint:errcheck
}

// resumeDeadSession is called when a thread reply arrives for a session whose
// tmux pane is no longer alive. It:
//  1. Spawns dpmux with --session <sessionID>.
//  2. Detects refusal via pane text or session id mismatch.
//  3. On refusal: teardown, retry once silently.
//  4. On second failure: announces a fresh session was started.
//  5. On success: updates rec.TmuxTarget and injects the queued message.
func (w *Watcher) resumeDeadSession(ctx context.Context, rec *SessionRecord, content string, atts []inject.Attachment) error {
	log.Printf("mirror: resumeDeadSession: session=%s", rec.SessionID)

	// Take DB baseline BEFORE spawning so any new session row created during
	// spawn is visible to detectRefusal (never miss a foreign-id creation).
	baselineMS := time.Now().UnixMilli()

	newTarget, err := w.spawnResume(ctx, rec.SessionID)
	if err != nil {
		return fmt.Errorf("spawn resume: %w", err)
	}

	// Detect refusal: check pane for "Error: Session not found:" within 5s,
	// or check if a new session row with a foreign id was created since baseline.
	refused, err := w.detectRefusal(ctx, rec.SessionID, newTarget, baselineMS)
	if err != nil {
		log.Printf("mirror: refusal detection error: %v", err)
	}

	if refused {
		log.Printf("mirror: resume refused for session %s (attempt 1), tearing down and retrying", rec.SessionID)
		w.teardownPane(newTarget)

		// Retry once silently — take a fresh baseline before the second spawn.
		baseline2MS := time.Now().UnixMilli()
		newTarget2, err2 := w.spawnResume(ctx, rec.SessionID)
		if err2 != nil {
			// Could not even spawn. Fall back to fresh session.
			return w.startFreshSessionAfterRefusal(ctx, rec, content, atts)
		}
		refused2, _ := w.detectRefusal(ctx, rec.SessionID, newTarget2, baseline2MS)
		if refused2 {
			log.Printf("mirror: resume refused for session %s (attempt 2), starting fresh session", rec.SessionID)
			w.teardownPane(newTarget2)
			return w.startFreshSessionAfterRefusal(ctx, rec, content, atts)
		}
		newTarget = newTarget2
	}

	// Resume succeeded: update the session record with the new tmux target.
	rec.TmuxTarget = newTarget
	if err := w.sessions.Upsert(rec); err != nil {
		log.Printf("mirror: update sessions.json after resume: %v", err)
	}

	// Inject the queued message into the resumed session.
	return w.injectAndVerify(ctx, newTarget, rec.SessionID, rec.ThreadID, content, atts)
}

// spawnResume launches dpmux with --session <sessionID> and returns the new tmux target.
func (w *Watcher) spawnResume(ctx context.Context, sessionID string) (string, error) {
	dpmuxBin := w.cfg.DpmuxBin
	if dpmuxBin == "" {
		p, err := exec.LookPath("dpmux")
		if err != nil {
			return "", fmt.Errorf("dpmux not found: %w", err)
		}
		dpmuxBin = p
	}

	cmd := exec.CommandContext(ctx, dpmuxBin, "--session", sessionID)
	env := os.Environ()
	if w.cfg.StateDir != "" {
		env = setEnvVar(env, "RELAY_STATE_DIR", w.cfg.StateDir)
	}
	if w.cfg.DBPath != "" {
		env = setEnvVar(env, "RELAY_DEVPASS_DB", w.cfg.DBPath)
	}
	env = setEnvVar(env, "TMUX", "")
	cmd.Env = env

	var stdoutBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = os.Stderr

	log.Printf("mirror: spawnResume: dpmux --session %s", sessionID)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("dpmux --session %s: %w", sessionID, err)
	}

	// dpmux prints the session name to stdout.
	sessionName := strings.TrimSpace(stdoutBuf.String())
	if sessionName == "" {
		return "", fmt.Errorf("dpmux produced no session name")
	}

	// Read the pending marker to get the tmux target.
	pendingPath := filepath.Join(w.cfg.StateDir, "pending-"+sessionName)
	data, err := os.ReadFile(pendingPath)
	if err != nil {
		// Marker may already be consumed; derive target from session name.
		return sessionName + ":0.0", nil
	}
	// Try parsing as JSON (nonce-bearing marker).
	var m spawnMarkerJSON
	if jsonErr := json.Unmarshal(data, &m); jsonErr == nil && m.Target != "" {
		return m.Target, nil
	}
	// Legacy plain-text marker.
	return strings.TrimSpace(string(data)), nil
}

// DetectRefusal is the exported variant of detectRefusal used in tests.
func (w *Watcher) DetectRefusal(ctx context.Context, sessionID, tmuxTarget string, baselineMS int64) (bool, error) {
	return w.detectRefusal(ctx, sessionID, tmuxTarget, baselineMS)
}

// detectRefusal returns true if the devpass-code process refused the --session resume.
// baselineMS is the Unix-millisecond timestamp taken BEFORE spawnResume was called;
// any new session row with time_created >= baselineMS and an id other than sessionID
// is evidence of refusal.
// Signals checked (per DISCOVERY.md):
//  1. Pane contains "Error: Session not found:" within 5s.
//  2. The first new session row created after baseline has an id other than sessionID.
//  3. Pane shows "Ask anything..." (success).
func (w *Watcher) detectRefusal(ctx context.Context, sessionID, tmuxTarget string, baselineMS int64) (bool, error) {
	deadline := time.Now().Add(5 * time.Second)

	// sawAlive tracks whether capturePaneTextRaw has ever succeeded for this
	// target. This is required to fail closed correctly: a target that never
	// becomes observable (e.g. its tmux session/server already exited because
	// the launched process refused and the wrapping shell's tail exited,
	// tearing down the only session on an isolated socket via tmux's
	// exit-empty behaviour) must NOT be silently treated as a successful
	// resume just because the poll loop ran out of time without seeing an
	// explicit signal. Without this, a fast pane teardown races the poll
	// loop: every capture-pane call fails with "no server running", no DB
	// row appears (correctly, since the resume was refused), and the old
	// code fell through to "assume success" — handing resumeDeadSession a
	// target that no longer exists, which then fails much later with a
	// bare "no server running" from send-keys instead of retrying.
	sawAlive := false

	for time.Now().Before(deadline) {
		// Signal 1: pane text.
		if text, err := capturePaneTextRaw(tmuxTarget); err == nil {
			sawAlive = true
			if strings.Contains(text, "Error: Session not found:") {
				log.Printf("mirror: refusal detected via pane text for %s", sessionID)
				return true, nil
			}
			// Signal 3: if pane shows "Ask anything..." it resumed successfully —
			// but only accept if no foreign session row exists since baseline.
			if strings.Contains(text, "Ask anything...") {
				// Still check for foreign-id (defence-in-depth).
				var newID string
				w.db.QueryRowContext(ctx,
					`SELECT id FROM session WHERE time_created >= ? AND parent_id IS NULL ORDER BY time_created ASC LIMIT 1`,
					baselineMS,
				).Scan(&newID)
				if newID != "" && newID != sessionID {
					log.Printf("mirror: refusal detected via session id mismatch (ready pane): want=%s got=%s", sessionID, newID)
					return true, nil
				}
				return false, nil
			}
		} else if sawAlive {
			// The pane WAS observable and is now gone without ever showing the
			// ready signature. A process that disappears before becoming ready
			// never successfully resumed; treat this as a refusal so the
			// caller retries/tears down instead of proceeding to inject into a
			// target that no longer exists.
			log.Printf("mirror: refusal detected via pane disappearance before ready for %s (target=%s)", sessionID, tmuxTarget)
			return true, nil
		}

		// Signal 2: new session row with wrong id.
		var newID string
		w.db.QueryRowContext(ctx,
			`SELECT id FROM session WHERE time_created >= ? AND parent_id IS NULL ORDER BY time_created ASC LIMIT 1`,
			baselineMS,
		).Scan(&newID)
		if newID != "" && newID != sessionID {
			log.Printf("mirror: refusal detected via session id mismatch: want=%s got=%s", sessionID, newID)
			return true, nil
		}

		time.Sleep(200 * time.Millisecond)
	}
	if !sawAlive {
		// Never once observed the target within the wait window at all. This
		// is a failure signal, not a silent success: fail closed by treating
		// it as a refusal so the caller tears down (a no-op, since nothing
		// exists) and retries with a fresh spawn, rather than handing back a
		// target that was never confirmed alive.
		log.Printf("mirror: refusal detection could never observe target %s for %s – treating as refused", tmuxTarget, sessionID)
		return true, nil
	}
	// Timeout without a definitive signal, but the target was observably
	// alive throughout – assume success (unchanged prior behaviour).
	return false, nil
}

// capturePaneTextRaw captures visible text from a tmux pane target.
func capturePaneTextRaw(target string) (string, error) {
	out, err := exec.Command("tmux", "capture-pane", "-t", target, "-p").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// startFreshSessionAfterRefusal starts a brand-new session (no --session flag)
// and posts ONE notice to the thread that a fresh session was started.
func (w *Watcher) startFreshSessionAfterRefusal(ctx context.Context, rec *SessionRecord, content string, atts []inject.Attachment) error {
	log.Printf("mirror: starting fresh session after double refusal for %s", rec.SessionID)
	w.sendToThread(rec.ThreadID, "⚠️ Could not resume previous session; a **fresh session** has been started.")

	// Spawn a fresh dpmux session (no --session).
	if err := w.handleSpawnFromDiscord(ctx, content, nil); err != nil {
		return fmt.Errorf("fresh spawn after refusal: %w", err)
	}
	return nil
}

// injectAndVerify injects content into the tmux pane and verifies the echo
// appears in the DB within echoVerifyTimeout. If the echo is absent, it
// re-sends once and suppresses duplicate echoes in the Discord thread.
func (w *Watcher) injectAndVerify(ctx context.Context, target, sessionID, threadID, content string, atts []inject.Attachment) error {
	// Perform the injection.
	if err := w.injector.Inject(target, sessionID, content, atts); err != nil {
		return fmt.Errorf("inject: %w", err)
	}

	// Verify the echo appears in the DB within echoVerifyTimeout.
	if w.waitForEcho(ctx, sessionID, content, echoVerifyTimeout) {
		// Echo confirmed — record as echoed so mirrorContent won't double-post.
		// (The first inject already triggered the "You: …" post in mirrorContent;
		//  this is a no-op if mirrorContent hasn't seen the row yet.)
		return nil
	}

	// Echo absent: re-send once.
	log.Printf("mirror: echo absent for session %s – re-sending once", sessionID)
	// Mark the prompt as already-echoed BEFORE the retry so mirrorContent suppresses
	// the second user-prompt row if the fake echoes both turns.
	w.suppressedEchoes.add(sessionID, content)
	if err := w.injector.Inject(target, sessionID, content, atts); err != nil {
		return fmt.Errorf("inject retry: %w", err)
	}

	// Suppress duplicate: the Discord echo for this prompt was already posted (or
	// will be posted) by mirrorContent for the first user-prompt row. Any second
	// user-prompt row with identical text must not be re-posted to the thread.
	// The suppressedEchoes entry ensures mirrorContent skips the duplicate.
	return nil
}

// waitForEcho polls the DB for the given content in the part table for the
// specified session, returning true if found within timeout.
func (w *Watcher) waitForEcho(ctx context.Context, sessionID, content string, timeout time.Duration) bool {
	if content == "" {
		return true
	}
	// Truncate very long content for the DB LIKE query.
	needle := content
	if len(needle) > 100 {
		needle = needle[:100]
	}
	needle = strings.ReplaceAll(needle, "%", "\\%")
	needle = strings.ReplaceAll(needle, "_", "\\_")

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		var count int
		w.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM part WHERE session_id = ? AND data LIKE ?`,
			sessionID, "%"+needle+"%",
		).Scan(&count)
		if count > 0 {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// handleAmbiguousResume posts a numbered candidate list of all known sessions
// so the user can pick one with `!resume <n>`.
func (w *Watcher) handleAmbiguousResume(rec *SessionRecord, content string, atts []inject.Attachment) {
	// Collect all known sessions from the DB, newest first.
	ctx := context.Background()
	rows, err := w.db.QueryContext(ctx,
		`SELECT id, time_updated FROM session WHERE parent_id IS NULL ORDER BY time_updated DESC LIMIT 20`)
	var candidates []resumeCandidate
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var c resumeCandidate
			if err := rows.Scan(&c.SessionID, &c.LastActive); err == nil {
				candidates = append(candidates, c)
			}
		}
	}

	// Enrich each candidate with the provider from its most recent assistant message.
	for i := range candidates {
		candidates[i].ProviderID = w.fetchSessionProvider(ctx, candidates[i].SessionID)
	}

	if len(candidates) == 0 {
		w.sendToThread(rec.ThreadID, "No sessions available to resume.")
		return
	}

	if len(candidates) == 1 {
		// Only one candidate: resume directly.
		rec2 := *rec
		rec2.SessionID = candidates[0].SessionID
		if err := w.resumeDeadSession(ctx, &rec2, content, atts); err != nil {
			log.Printf("mirror: single candidate resume: %v", err)
			w.sendToThread(rec.ThreadID, fmt.Sprintf("⚠️ Resume failed: %v", err))
		}
		return
	}

	// Build numbered list with provider and last-active age.
	var sb strings.Builder
	sb.WriteString("**Multiple sessions available — reply `!resume <n>` to choose:**\n")
	now := time.Now().UnixMilli()
	for i, c := range candidates {
		ageMS := now - c.LastActive
		ageStr := formatAge(ageMS)
		provider := c.ProviderID
		if provider == "" {
			provider = "unknown"
		}
		sb.WriteString(fmt.Sprintf("%d. `%s` | provider: `%s` | last active: %s\n", i+1, c.SessionID, provider, ageStr))
	}

	w.sendToThread(rec.ThreadID, strings.TrimRight(sb.String(), "\n"))

	// Store the pending entry so the next `!resume <n>` resolves it.
	w.pendingResume.set(rec.ThreadID, &pendingResumeEntry{
		candidates: candidates,
		content:    content,
		atts:       atts,
		rec:        rec,
	})
}

// fetchSessionProvider returns the provider ID from the most recent assistant
// message in the session. The provider is stored in message.data JSON as
// model.providerID (per TRANSCRIPT-SCHEMA.md).
func (w *Watcher) fetchSessionProvider(ctx context.Context, sessionID string) string {
	// Find the most recent assistant message for this session.
	var data string
	err := w.db.QueryRowContext(ctx,
		`SELECT data FROM message WHERE session_id = ? ORDER BY time_created DESC LIMIT 1`,
		sessionID,
	).Scan(&data)
	if err != nil || data == "" {
		return ""
	}
	var md struct {
		Model struct {
			ProviderID string `json:"providerID"`
		} `json:"model"`
	}
	if err := json.Unmarshal([]byte(data), &md); err != nil {
		return ""
	}
	return md.Model.ProviderID
}

// formatAge formats a millisecond duration as a human-readable age string.
func formatAge(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// handleResumeChoiceForThread processes a !resume <n> message in a thread that
// has a pending resume candidate list.
func (w *Watcher) handleResumeChoiceForThread(threadID, content string) bool {
	entry, ok := w.pendingResume.get(threadID)
	if !ok {
		return false
	}
	s := strings.TrimSpace(content)
	if !strings.HasPrefix(s, "!resume ") {
		return false
	}
	nStr := strings.TrimSpace(strings.TrimPrefix(s, "!resume "))
	n := 0
	fmt.Sscanf(nStr, "%d", &n)
	if n < 1 || n > len(entry.candidates) {
		w.sendToThread(threadID, fmt.Sprintf("⚠️ Invalid choice. Please reply `!resume 1`–`!resume %d`.", len(entry.candidates)))
		return true
	}
	chosen := entry.candidates[n-1]
	w.pendingResume.delete(threadID)

	go func() {
		// Re-use the rec from the entry, updating its SessionID to the chosen one.
		rec := entry.rec
		rec.SessionID = chosen.SessionID
		if err := w.resumeDeadSession(context.Background(), rec, entry.content, entry.atts); err != nil {
			log.Printf("mirror: resume choice %s: %v", chosen.SessionID, err)
			w.sendToThread(threadID, fmt.Sprintf("⚠️ Resume failed: %v", err))
		}
	}()
	return true
}

// ─── Stderr-log session identity binding (production mechanism) ────────────
//
// This is the phase-16-verified real protocol (docs/BINDING-PROTOCOL.md):
// devpass-code is launched with --print-logs --log-level INFO and its stderr
// is redirected exclusively to one file per launch. When (and only when) that
// exact process creates a new session row, it logs
// "message=created id=ses_<ID>" to that file. Because the file is exclusive
// to the one launched process, no foreign process can ever cause a wrong id
// to appear in it — this is what makes it sufficient to prove ownership,
// unlike /proc liveness, row timestamps, or a shared directory, none of which
// prove who wrote a given row.
//
// createdSessionLogRe matches devpass-code's structured log line for session
// creation, tolerating field order/spacing per logfmt conventions.
var createdSessionLogRe = regexp.MustCompile(`message=created\s+id=(ses_[A-Za-z0-9_]+)`)

// extractCreatedSessionID reads logPath (a devpass-code stderr redirect file)
// and returns its one unambiguous created-session ID. Repeated observations of
// the same ID are harmless, but two distinct IDs are ambiguous evidence and
// fail closed. Returns ("", false) if the file does not exist, is empty,
// contains no such line, or contains conflicting creation IDs.
func extractCreatedSessionID(logPath string) (string, bool) {
	if logPath == "" {
		return "", false
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		return "", false
	}
	matches := createdSessionLogRe.FindAllStringSubmatch(string(data), -1)
	if len(matches) == 0 {
		return "", false
	}
	id := matches[0][1]
	for _, match := range matches[1:] {
		if match[1] != id {
			return "", false
		}
	}
	return id, true
}

// paneAliveFn returns true if the tmux pane target is still alive (has a
// live foreground process). Overridable in tests.
type paneAliveFn func(target string) bool

// bindSessionByStderrLog is the production identity binder. It polls logPath
// for a "message=created id=ses_<ID>" line, and for every candidate id it
// finds, re-verifies BOTH that the tmux pane is alive AT THAT MOMENT (not just
// at entry) and that the corresponding DB row exists with
// time_created >= spawnInstant, before returning it as bound. If the pane
// dies during the wait, or the log never appears, or the DB row is missing or
// stale, it fails closed: ("", false). It never falls back to nonce, title,
// row-time-only, or first-row heuristics.
func bindSessionByStderrLog(
	ctx context.Context,
	db *sql.DB,
	logPath string,
	tmuxTarget string,
	spawnInstant int64,
	timeout time.Duration,
	paneAlive paneAliveFn,
) (string, bool) {
	if logPath == "" {
		return "", false
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", false
		default:
		}

		// Check liveness at THIS moment, not just at entry: a pane that died
		// during the wait must never be treated as having produced a row.
		if !paneAlive(tmuxTarget) {
			return "", false
		}

		sid, logOK := extractCreatedSessionID(logPath)
		if logOK && sid != "" {
			// Cross-check the DB: the row must exist and be at or after
			// spawnInstant. A missing or stale row is not evidence of
			// ownership even if the log line is present (e.g. clock skew or
			// a corrupted/foreign log path).
			var createdAt int64
			err := db.QueryRowContext(ctx,
				`SELECT time_created FROM session WHERE id = ? AND parent_id IS NULL`,
				sid,
			).Scan(&createdAt)
			if err == nil && createdAt >= spawnInstant {
				return sid, true
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return "", false
}

// BindSessionByStderrLog is the exported, injectable form used by tests.
func BindSessionByStderrLog(
	ctx context.Context,
	db *sql.DB,
	logPath string,
	tmuxTarget string,
	spawnInstant int64,
	timeout time.Duration,
	paneAlive func(target string) bool,
) (string, bool) {
	return bindSessionByStderrLog(ctx, db, logPath, tmuxTarget, spawnInstant, timeout, paneAlive)
}

// handleSpawnFromDiscord spawns a new dpmux session in response to a Discord
// channel message (outside any thread), waits for the new session row to appear
// in the DB, binds the session (via mirrorSession), and injects the Discord
// message as the first prompt.
//
// Binding contract (phase-16-verified protocol; see docs/BINDING-PROTOCOL.md):
//   - spawnStart is recorded BEFORE dpmux is invoked.
//   - dpmux writes a JSON pending marker containing the spawn instant, tmux
//     target, and the path to this launch's exclusive stderr log
//     (StderrLog), and launches devpass-code with --print-logs --log-level
//     INFO redirected to that file.
//   - The ONLY accepted evidence is bindSessionByStderrLog: the process's own
//     stderr log must contain "message=created id=ses_<ID>", the pane must
//     be alive at the moment that evidence is checked (re-verified on every
//     poll, not just at entry), and the corresponding DB row must exist with
//     time_created >= spawnInstant.
//   - Neither /proc existence alone, process parentage, row timestamps alone,
//     a title nonce, nor a shared directory is ever treated as sufficient
//     proof; those may only narrow candidates, never substitute for the log
//     evidence.
//   - A missing or ambiguous identity (no log line, pane dies during the
//     wait, or the row is missing/stale) fails closed: no session is
//     claimed, no thread is created, and nothing is injected.
//   - Rows created before the marker's spawnInstant are never accepted.
//   - Lexicographic ordering of session IDs is never used.
//
// TODO(phase9): wrap dpmux invocation with systemd-run for process isolation.
func (w *Watcher) handleSpawnFromDiscord(ctx context.Context, content string, evAtts []MessageAttachment) error {
	// Record the spawn start instant BEFORE calling dpmux.
	spawnStartMS := time.Now().UnixMilli()

	// Resolve the dpmux binary.
	dpmuxBin := w.cfg.DpmuxBin
	if dpmuxBin == "" {
		p, err := exec.LookPath("dpmux")
		if err != nil {
			return fmt.Errorf("dpmux not found on PATH (set DpmuxBin in Config): %w", err)
		}
		dpmuxBin = p
	}

	// Build the dpmux command.  Inherit the relay's RELAY_STATE_DIR and
	// RELAY_DEVPASS_DB so dpmux writes the pending marker into the same
	// directory the relay is watching.
	cmd := exec.CommandContext(ctx, dpmuxBin)
	env := os.Environ()
	if w.cfg.StateDir != "" {
		env = setEnvVar(env, "RELAY_STATE_DIR", w.cfg.StateDir)
	}
	if w.cfg.DBPath != "" {
		env = setEnvVar(env, "RELAY_DEVPASS_DB", w.cfg.DBPath)
	}
	// Unset TMUX so dpmux always creates a fresh tmux session (never new-window
	// in the relay's own session if the relay runs inside tmux).
	env = setEnvVar(env, "TMUX", "")
	cmd.Env = env

	var stdoutBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = os.Stderr

	log.Printf("mirror: spawning dpmux (spawnStart=%d)", spawnStartMS)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("dpmux run: %w", err)
	}

	// dpmux prints the session name (dp-<unixtime>-<pid>) to stdout.
	sessionName := strings.TrimSpace(stdoutBuf.String())
	if sessionName == "" {
		return fmt.Errorf("dpmux produced no session name on stdout")
	}
	log.Printf("mirror: dpmux spawned session %q", sessionName)

	// Read and parse the pending marker written by dpmux.
	pendingPath := filepath.Join(w.cfg.StateDir, "pending-"+sessionName)
	markerData, readErr := os.ReadFile(pendingPath)
	if readErr != nil {
		return fmt.Errorf("read pending marker for %q: %w", sessionName, readErr)
	}
	var marker spawnMarkerJSON
	if jsonErr := json.Unmarshal(markerData, &marker); jsonErr != nil {
		// Legacy plain-text marker: treat content as tmux target.
		marker.Target = strings.TrimSpace(string(markerData))
	}
	if marker.Target == "" {
		marker.Target = sessionName + ":0.0"
	}
	spawnInstant := marker.SpawnInstant
	if spawnInstant < spawnStartMS {
		spawnInstant = spawnStartMS
	}

	bindTimeout := w.cfg.SpawnBindTimeout
	tmuxTarget := marker.Target

	if marker.StderrLog == "" {
		// No identity evidence source at all: fail closed. Never claim or
		// inject into any row without the phase-16-verified protocol.
		return fmt.Errorf("pending marker for %q has no stderrLog; cannot verify session identity (spawnStart=%d)", sessionName, spawnStartMS)
	}

	boundSessionID, bound := bindSessionByStderrLog(
		ctx, w.db, marker.StderrLog, tmuxTarget, spawnInstant, bindTimeout, isTmuxPaneAlive,
	)
	if !bound || boundSessionID == "" {
		return fmt.Errorf("timed out or failed to verify session identity for dpmux session %q (spawnStart=%d, stderrLog=%s)", sessionName, spawnStartMS, marker.StderrLog)
	}

	log.Printf("mirror: bound new session %s to dpmux %s (target=%s)", boundSessionID, sessionName, tmuxTarget)

	// Atomically claim the session.  If scanSessions won the race and already
	// claimed it, mirrorSession is running; we only need to inject the prompt.
	needsMirror := w.claimed.tryAdd(boundSessionID)

	// Remove the pending marker so scanSessions won't try to bind it again.
	os.Remove(pendingPath)

	if needsMirror {
		// Register the session into sessions.json so that scanSessions' `seen` map
		// picks it up on the next tick and skips it.
		if err := w.sessions.Upsert(&SessionRecord{
			SessionID:  boundSessionID,
			TmuxTarget: tmuxTarget,
			Status:     "active",
		}); err != nil {
			log.Printf("mirror: sessions.json pre-upsert for %s: %v", boundSessionID, err)
		}

		// Mirror the session concurrently (same as scanSessions does).
		var createdAt int64
		w.db.QueryRowContext(ctx, `SELECT time_created FROM session WHERE id = ?`, boundSessionID).Scan(&createdAt)
		go func(sid, target string, ca int64) {
			if err := w.mirrorSession(ctx, sid, target, ca); err != nil {
				log.Printf("mirror: mirrorSession(spawn) %s: %v", sid, err)
			}
		}(boundSessionID, tmuxTarget, createdAt)
	}

	// Inject the original Discord message as the first prompt.
	// Build attachment list.
	var atts []inject.Attachment
	for _, a := range evAtts {
		atts = append(atts, inject.Attachment{Filename: a.Filename, URL: a.URL})
	}

	// Inject in a separate goroutine; the readiness probe inside Inject will
	// wait for "Ask anything..." before sending the text.
	go func(target, sid, text string, attachments []inject.Attachment) {
		if err := w.injector.Inject(target, sid, text, attachments); err != nil {
			log.Printf("mirror: inject first prompt into %s: %v", sid, err)
		}
	}(tmuxTarget, boundSessionID, content, atts)

	return nil
}

// setEnvVar sets (or replaces) a KEY=VALUE pair in the given env slice.
func setEnvVar(env []string, key, value string) []string {
	prefix := key + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
