// Package transcript provides read-only access to devpass-code's SQLite store
// (~/.local/share/devpass-code/opencode.db, WAL mode). It polls for new rows
// for a given session ID and emits Part events on a channel.
//
// Schema reference: docs/TRANSCRIPT-SCHEMA.md
// devpass-code version: 1.18.11
package transcript

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, CGO_ENABLED=0 compatible
)

// Part mirrors a row from the `part` table.
type Part struct {
	ID        string
	MessageID string
	SessionID string
	TimeCreated int64
	TimeUpdated int64
	Data      json.RawMessage
}

// PartData is the parsed JSON from part.data.
type PartData struct {
	Type   string          `json:"type"`
	Text   string          `json:"text,omitempty"`
	Reason string          `json:"reason,omitempty"`
	Tool   string          `json:"tool,omitempty"`
	CallID string          `json:"callID,omitempty"`
	State  json.RawMessage `json:"state,omitempty"`
}

// Message mirrors a row from the `message` table.
type Message struct {
	ID          string
	SessionID   string
	TimeCreated int64
	TimeUpdated int64
	Data        json.RawMessage
}

// MessageData is the parsed JSON from message.data.
type MessageData struct {
	Role   string `json:"role"`
	Finish string `json:"finish,omitempty"`
}

// Session mirrors a row from the `session` table.
type Session struct {
	ID          string
	ProjectID   string
	WorkspaceID string
	Slug        string
	Directory   string
	Title       string
	Version     string
	TimeCreated int64
	TimeUpdated int64
}

// Tailer tails a single session's parts from the SQLite DB.
type Tailer struct {
	db        *sql.DB
	sessionID string
	pollInterval time.Duration
}

// OpenDB opens the devpass-code SQLite database read-only.
// path should be the path to opencode.db.
func OpenDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("transcript: open db: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("transcript: ping db: %w", err)
	}
	return db, nil
}

// NewTailer creates a Tailer for the given session.
func NewTailer(db *sql.DB, sessionID string, pollInterval time.Duration) *Tailer {
	if pollInterval == 0 {
		pollInterval = 250 * time.Millisecond
	}
	return &Tailer{
		db:           db,
		sessionID:    sessionID,
		pollInterval: pollInterval,
	}
}

// Tail emits Part events on the returned channel until ctx is cancelled.
// It only emits parts with time_created > afterMS (exclusive).
func (t *Tailer) Tail(ctx context.Context, afterMS int64) (<-chan Part, error) {
	ch := make(chan Part, 32)
	go func() {
		defer close(ch)
		cursor := afterMS
		ticker := time.NewTicker(t.pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				parts, err := t.fetchParts(ctx, cursor)
				if err != nil {
					continue
				}
				for _, p := range parts {
					if p.TimeCreated > cursor {
						cursor = p.TimeCreated
					}
					select {
					case ch <- p:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	return ch, nil
}

func (t *Tailer) fetchParts(ctx context.Context, afterMS int64) ([]Part, error) {
	rows, err := t.db.QueryContext(ctx,
		`SELECT id, message_id, session_id, time_created, time_updated, data
		 FROM part
		 WHERE session_id = ? AND time_created > ?
		 ORDER BY time_created ASC`,
		t.sessionID, afterMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var parts []Part
	for rows.Next() {
		var p Part
		var data string
		if err := rows.Scan(&p.ID, &p.MessageID, &p.SessionID, &p.TimeCreated, &p.TimeUpdated, &data); err != nil {
			return nil, err
		}
		p.Data = json.RawMessage(data)
		parts = append(parts, p)
	}
	return parts, rows.Err()
}

// GetSession fetches a session row by ID. Returns sql.ErrNoRows if not found.
func GetSession(ctx context.Context, db *sql.DB, sessionID string) (*Session, error) {
	row := db.QueryRowContext(ctx,
		`SELECT id, project_id, COALESCE(workspace_id,''), slug, directory, title, version, time_created, time_updated
		 FROM session WHERE id = ?`, sessionID)
	var s Session
	err := row.Scan(&s.ID, &s.ProjectID, &s.WorkspaceID, &s.Slug, &s.Directory, &s.Title, &s.Version, &s.TimeCreated, &s.TimeUpdated)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// WaitForSession polls until a session row with the given ID appears or ctx is cancelled.
func WaitForSession(ctx context.Context, db *sql.DB, sessionID string, poll time.Duration) (*Session, error) {
	if poll == 0 {
		poll = 250 * time.Millisecond
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			s, err := GetSession(ctx, db, sessionID)
			if err == nil {
				return s, nil
			}
			if err != sql.ErrNoRows {
				return nil, err
			}
		}
	}
}
