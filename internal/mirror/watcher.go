// Package mirror: watcher polls the DB for new sessions and pairs them with pending markers.
package mirror

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/devpass/devpass-discord-relay/internal/transcript"
)

// Config holds relay watcher configuration.
type Config struct {
	// Discord
	DiscordToken  string
	DiscordAPIURL string // override for testing
	ChannelID     string

	// DB
	DBPath string

	// State
	StateDir string

	// Timing
	PollInterval time.Duration // default 1s
}

// DiscordClient is the interface the watcher uses to talk to Discord.
type DiscordClient interface {
	SendMessage(channelID, content string) (MsgRef, error)
	EditMessage(channelID, messageID, content string) error
	CreateThread(channelID, messageID, name string) (ThreadRef, error)
	SendTyping(channelID string) error
}

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
}

// NewWatcher creates a watcher. db must already be open read-only.
func NewWatcher(cfg Config, db *sql.DB, discord DiscordClient) (*Watcher, error) {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 1 * time.Second
	}
	sessPath := filepath.Join(cfg.StateDir, "sessions.json")
	sessions, err := LoadSessions(sessPath)
	if err != nil {
		return nil, fmt.Errorf("mirror: load sessions: %w", err)
	}
	return &Watcher{
		cfg:      cfg,
		db:       db,
		discord:  discord,
		sessions: sessions,
		throttle: NewThrottle(5, 5*time.Second),
	}, nil
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
	role         string
	discordMsgID string // for assistant text accumulation
	text         string // accumulated text so far
	toolCalls    map[string]*toolState
	isPreamble   bool
	finishSeen   bool
	promptEchoed bool
	hadOutput    bool
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
	if _, err := w.discord.SendMessage(threadID, content); err != nil {
		log.Printf("mirror: sendToThread: %v", err)
	}
}

// updateTextMessage creates or edits the current assistant text Discord message.
func (w *Watcher) updateTextMessage(threadID string, ms *msgState) {
	text := truncateForDiscord(ms.text)
	if ms.discordMsgID == "" {
		w.throttle.Wait(threadID)
		msg, err := w.discord.SendMessage(threadID, text)
		if err != nil {
			log.Printf("mirror: create text message: %v", err)
			return
		}
		ms.discordMsgID = msg.ID
	} else {
		w.throttle.Wait(threadID)
		if err := w.discord.EditMessage(threadID, ms.discordMsgID, text); err != nil {
			log.Printf("mirror: edit text message: %v", err)
		}
	}
}

// truncateForDiscord returns text truncated to 2000 chars for Discord.
func truncateForDiscord(text string) string {
	chunks := ChunkText(text, 2000)
	if len(chunks) == 0 {
		return text
	}
	return chunks[0]
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
		toolMsg, sendErr := w.discord.SendMessage(threadID, content)
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
		if err := w.discord.EditMessage(threadID, ts.msgID, content); err != nil {
			log.Printf("mirror: update tool embed done: %v", err)
		}
	} else if status == "error" && !ts.failed {
		ts.failed = true
		content := formatToolEmbed(ts.toolName, ts.callID, false, true)
		w.throttle.Wait(threadID)
		if err := w.discord.EditMessage(threadID, ts.msgID, content); err != nil {
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

// isTmuxPaneAlive checks if a tmux pane target is still alive.
func isTmuxPaneAlive(target string) bool {
	// Use has-session for session-level check; for window/pane use list-panes
	err := exec.Command("tmux", "has-session", "-t", target).Run()
	return err == nil
}
