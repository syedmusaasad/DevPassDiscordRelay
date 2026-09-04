package mirror

import (
	"sync"
	"time"
)

// Throttle enforces ≤maxEdits edits per window duration per channel.
// If the limit is hit, Wait returns the delay needed before the next edit is allowed.
// It also handles 429 backoffs.
type Throttle struct {
	mu       sync.Mutex
	window   time.Duration // e.g. 5s
	maxEdits int           // e.g. 5
	// per-channel edit timestamps
	edits   map[string][]time.Time
	backoff map[string]time.Time // channel -> backoff-until
}

// NewThrottle creates a Throttle with the given window and max edits.
// Zero values use defaults (5 edits / 5 seconds).
func NewThrottle(maxEdits int, window time.Duration) *Throttle {
	if maxEdits <= 0 {
		maxEdits = 5
	}
	if window <= 0 {
		window = 5 * time.Second
	}
	return &Throttle{
		window:   window,
		maxEdits: maxEdits,
		edits:    make(map[string][]time.Time),
		backoff:  make(map[string]time.Time),
	}
}

// Wait blocks until an edit for channelID is allowed, then records the edit.
func (t *Throttle) Wait(channelID string) {
	for {
		delay := t.allowed(channelID)
		if delay <= 0 {
			return
		}
		time.Sleep(delay)
	}
}

// allowed returns 0 if an edit is allowed now, else the duration to wait.
// If allowed, it records the edit.
func (t *Throttle) allowed(channelID string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()

	// Check backoff
	if until, ok := t.backoff[channelID]; ok && now.Before(until) {
		return until.Sub(now)
	}

	// Prune old edits outside the window
	cutoff := now.Add(-t.window)
	var recent []time.Time
	for _, ts := range t.edits[channelID] {
		if ts.After(cutoff) {
			recent = append(recent, ts)
		}
	}
	t.edits[channelID] = recent

	if len(recent) < t.maxEdits {
		t.edits[channelID] = append(t.edits[channelID], now)
		return 0
	}

	// Must wait until oldest edit falls out of window
	oldest := recent[0]
	return oldest.Add(t.window).Sub(now)
}

// SetBackoff registers a 429 backoff for the given channel.
func (t *Throttle) SetBackoff(channelID string, retryAfter time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.backoff[channelID] = time.Now().Add(retryAfter)
}

// ClearBackoff removes a backoff for the given channel.
func (t *Throttle) ClearBackoff(channelID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.backoff, channelID)
}
