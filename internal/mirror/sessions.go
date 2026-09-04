// Package mirror: sessions.json persistence.
package mirror

import (
	"encoding/json"
	"os"
	"sync"
)

// SessionRecord represents a relay session claim in sessions.json.
type SessionRecord struct {
	SessionID    string `json:"session_id"`
	TmuxTarget   string `json:"tmux_target"`
	ThreadID     string `json:"thread_id"`
	ThreadRootID string `json:"thread_root_id"`
	ChannelID    string `json:"channel_id"`
	Status       string `json:"status"` // "active", "ended"
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
	s.records[r.SessionID] = r
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
