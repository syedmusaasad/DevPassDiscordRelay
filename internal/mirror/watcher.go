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
	"strings"
	"sync"
	"time"

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
	// claimed guards against double-binding a session from two concurrent paths
	// (scanSessions and handleSpawnFromDiscord). A session ID is inserted here
	// the first time it is claimed; subsequent claimants are rejected.
	claimed claimSet
}

// claimSet is a thread-safe set of claimed session IDs.
type claimSet struct {
	mu   sync.Mutex
	ids  map[string]bool
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
	}

	// Register incoming message handler for thread replies.
	discord.OnMessage(w.handleDiscordMessage)

	return w, nil
}

// Run starts the watcher loop until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
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
		`SELECT id, time_created FROM session WHERE parent_id IS NULL ORDER BY time_created ASC`)
	if err != nil {
		return
	}
	defer rows.Close()

	type sessionRow struct {
		id      string
		created int64
	}

	var sessions []sessionRow
	for rows.Next() {
		var r sessionRow
		if err := rows.Scan(&r.id, &r.created); err != nil {
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

// findPendingMarker looks for a pending-<name> marker file in stateDir.
// It matches any marker whose modification time is within 30s before the session creation.
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
		// Check file mod time: session must have been created within 30s after the marker
		info, err := e.Info()
		if err != nil {
			continue
		}
		markerTimeMS := info.ModTime().UnixMilli()
		// Accept if session was created within [-5s, +30s] of the marker being written
		diff := createdAt - markerTimeMS
		if diff >= -5000 && diff <= 30000 {
			return strings.TrimSpace(string(data)), fp, true
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
	chunkedBytes  int      // byte offset into text that has been committed to discordMsgIDs[:-1]
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
						w.sendToThread(threadID, fmt.Sprintf("**You:** %s", prompt))
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
	K := len(chunks)            // chunks needed now

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

// discordSend sends a message with 429 retry logic.
func (w *Watcher) discordSend(channelID, content string) (MsgRef, error) {
	msg, err := w.discord.SendMessage(channelID, content)
	if err == nil {
		return msg, nil
	}
	if retryAfter, ok := extractRetryAfter(err); ok {
		log.Printf("mirror: 429 rate-limit on send, retry after %v", retryAfter)
		w.throttle.SetBackoff(channelID, retryAfter)
		time.Sleep(retryAfter)
		w.throttle.ClearBackoff(channelID)
		return w.discord.SendMessage(channelID, content)
	}
	return MsgRef{}, err
}

// discordEdit edits a message with 429 retry logic.
func (w *Watcher) discordEdit(channelID, messageID, content string) error {
	err := w.discord.EditMessage(channelID, messageID, content)
	if err == nil {
		return nil
	}
	if retryAfter, ok := extractRetryAfter(err); ok {
		log.Printf("mirror: 429 rate-limit on edit, retry after %v", retryAfter)
		w.throttle.SetBackoff(channelID, retryAfter)
		time.Sleep(retryAfter)
		w.throttle.ClearBackoff(channelID)
		return w.discord.EditMessage(channelID, messageID, content)
	}
	return err
}

// rateLimitErr is the interface satisfied by discordgo.RateLimitError.
type rateLimitErr interface {
	error
	// RetryAfter is embedded via *RateLimit → *TooManyRequests.
}

// ExtractRetryAfter extracts the retry_after duration from a discordgo rate-limit error.
// It checks for the discordgo RateLimitError interface (has RetryAfter field via
// reflection-unsafe casting) or parses the error string as a fallback.
// Exported for use in unit tests.
func ExtractRetryAfter(err error) (time.Duration, bool) {
	return extractRetryAfter(err)
}

// extractRetryAfter extracts the retry_after duration from a discordgo rate-limit error.
// It checks for the discordgo RateLimitError interface (has RetryAfter field via
// reflection-unsafe casting) or parses the error string as a fallback.
func extractRetryAfter(err error) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	// discordgo.RateLimitError embeds *RateLimit which embeds *TooManyRequests.
	// We can't import discordgo here, so use an interface with a method.
	type retryAfterGetter interface {
		GetRetryAfter() time.Duration
	}
	// Try the interface directly.
	var rg retryAfterGetter
	if errors.As(err, &rg) {
		return rg.GetRetryAfter(), true
	}

	// Try unwrapping to find a *retryAfterHolder (our own wrapper, used in tests).
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
		// Not a thread we manage.
		return
	}

	log.Printf("mirror: handleDiscordMessage: channel=%s session=%s", ev.ChannelID, rec.SessionID)

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

	// Inject in a goroutine so we don't block the gateway event loop.
	go func(target, sessionID, content string, atts []inject.Attachment) {
		if err := w.injector.Inject(target, sessionID, content, atts); err != nil {
			log.Printf("mirror: inject session %s: %v", sessionID, err)
		}
	}(rec.TmuxTarget, rec.SessionID, ev.Content, attachments)
}

// handleSpawnFromDiscord spawns a new dpmux session in response to a Discord
// channel message (outside any thread), waits for the new session row to appear
// in the DB, binds the session (via mirrorSession), and injects the Discord
// message as the first prompt.
//
// Binding contract:
//   - spawnStart is recorded BEFORE dpmux is invoked.
//   - Only session rows with time_created >= spawnStart are candidates.
//   - Among those candidates, only the row whose pending marker name matches
//     the session name printed by dpmux is accepted.
//   - Lexicographic ordering of session IDs is never used; the pending marker
//     (written by dpmux before spawning devpass-code) is the sole pairing key.
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

	// The pending marker written by dpmux is pending-<sessionName>.
	pendingMarkerName := "pending-" + sessionName
	pendingPath := filepath.Join(w.cfg.StateDir, pendingMarkerName)

	// Poll the DB for the new session row.  The row must have
	// time_created >= spawnStartMS and its pending marker must exist.
	// Never use lexicographic ordering; never use a process-start baseline
	// snapshot of existing session IDs.
	bindTimeout := w.cfg.SpawnBindTimeout
	deadline := time.Now().Add(bindTimeout)

	var boundSessionID string
	var tmuxTarget string
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		rows, err := w.db.QueryContext(ctx,
			`SELECT id, time_created FROM session WHERE parent_id IS NULL AND time_created >= ? ORDER BY time_created ASC`,
			spawnStartMS)
		if err != nil {
			time.Sleep(300 * time.Millisecond)
			continue
		}

		found := false
		for rows.Next() {
			var sid string
			var tc int64
			if err := rows.Scan(&sid, &tc); err != nil {
				continue
			}
			// Verify the pending marker for our specific dpmux session still exists.
			// We check for this exact marker (not any marker) so that a concurrent
			// stale spawn's marker cannot steal the bind.
			data, err := os.ReadFile(pendingPath)
			if err != nil {
				// Marker consumed (already removed by scanSessions), or not yet written.
				// If scanSessions already consumed it (raced us), the session will also
				// be in w.sessions already; check there.
				if existing := w.sessions.Get(sid); existing != nil {
					// Already bound by scanSessions; grab its target.
					boundSessionID = sid
					tmuxTarget = existing.TmuxTarget
					found = true
					break
				}
				continue
			}
			// Marker exists; verify it matches this session name.
			target := strings.TrimSpace(string(data))
			boundSessionID = sid
			tmuxTarget = target
			found = true
			break
		}
		rows.Close()

		if found {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	if boundSessionID == "" {
		return fmt.Errorf("timed out waiting for session row for dpmux session %q (spawnStart=%d)", sessionName, spawnStartMS)
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
