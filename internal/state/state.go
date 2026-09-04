// Package state tracks the relay's in-memory state: active sessions, message IDs, etc.
package state

import (
	"sync"
)

// SessionState holds per-session relay state.
type SessionState struct {
	SessionID       string
	DiscordThreadID string
	LastPartID      string
	LastMessageID   string
}

// Store is a thread-safe map of session states keyed by session ID.
type Store struct {
	mu       sync.RWMutex
	sessions map[string]*SessionState
}

// New creates an empty Store.
func New() *Store {
	return &Store{sessions: make(map[string]*SessionState)}
}

// Get returns the session state for the given ID, or nil if not found.
func (s *Store) Get(sessionID string) *SessionState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[sessionID]
}

// Set stores session state.
func (s *Store) Set(state *SessionState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[state.SessionID] = state
}

// Delete removes session state.
func (s *Store) Delete(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
}

// All returns a snapshot of all session states.
func (s *Store) All() []*SessionState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*SessionState, 0, len(s.sessions))
	for _, v := range s.sessions {
		out = append(out, v)
	}
	return out
}
