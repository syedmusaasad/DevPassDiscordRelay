// Package mirror: sessions.json persistence.
package mirror

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// SessionRecord represents a relay session claim in sessions.json.
type SessionRecord struct {
	SessionID    string `json:"session_id"`
	TmuxTarget   string `json:"tmux_target"`
	ThreadID     string `json:"thread_id"`
	ThreadRootID string `json:"thread_root_id"`
	ChannelID    string `json:"channel_id"`
	Status       string `json:"status"` // "active", "ended"
	UpdatedAt    int64  `json:"updated_at"`
}

// Sessions is a thread-safe sessions.json store.
type Sessions struct {
	mu      sync.Mutex
	path    string
	records map[string]*SessionRecord
}

// LoadSessions opens (or creates) a sessions.json at the given path.
func LoadSessions(path string) (*Sessions, error) {
	s := &Sessions{path: path, records: make(map[string]*SessionRecord)}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*SessionRecord
	if err := json.Unmarshal(data, &list); err != nil {
		// Corrupted - start fresh
		return s, nil
	}
	for _, r := range list {
		s.records[r.SessionID] = r
	}
	return s, nil
}

// Upsert creates or updates a session record.
func (s *Sessions) Upsert(r *SessionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.UpdatedAt = time.Now().UnixMilli()
	s.records[r.SessionID] = r
	return s.save()
}

// SweepExpired removes records older than ttl only after confirming their tmux
// window is gone. Active windows survive regardless of record age.
func (s *Sessions) SweepExpired(ttl time.Duration, alive func(string) bool, now time.Time) error {
	if ttl <= 0 {
		return nil
	}
	cutoff := now.Add(-ttl).UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for id, record := range s.records {
		if record.UpdatedAt == 0 || record.UpdatedAt > cutoff {
			continue
		}
		if record.TmuxTarget != "" && alive(record.TmuxTarget) {
			continue
		}
		delete(s.records, id)
		changed = true
	}
	if !changed {
		return nil
	}
	return s.save()
}

// Get returns the record for the given session ID, or nil.
func (s *Sessions) Get(sessionID string) *SessionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.records[sessionID]
}

// All returns a snapshot of all records.
func (s *Sessions) All() []*SessionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*SessionRecord, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, r)
	}
	return out
}

func (s *Sessions) save() error {
	list := make([]*SessionRecord, 0, len(s.records))
	for _, r := range s.records {
		list = append(list, r)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0644)
}
