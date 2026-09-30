// Package telemetry sends the relay's deliberately minimal, best-effort usage heartbeat.
package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// defaultPoll is how often a long-running relay re-checks the UTC day.
const defaultPoll = 10 * time.Minute

// Send posts {username, version} at most once each UTC day. Failures are quiet
// and do not create a stamp, so a later attempt may retry. It is asynchronous;
// long-running processes should use Run so a service that stays up across a
// UTC day boundary keeps reporting.
func Send(stateDir, version, endpoint string) {
	if endpoint == "" {
		return
	}
	go sendIfDue(stateDir, version, endpoint, time.Now())
}

// Run sends once per UTC day for as long as ctx lives: immediately if today's
// stamp is absent, then again after each UTC day boundary. It does nothing when
// endpoint is empty.
func Run(ctx context.Context, stateDir, version, endpoint string) {
	RunWithClock(ctx, stateDir, version, endpoint, time.Now, defaultPoll)
}

// RunWithClock is Run with an injectable clock and poll interval.
func RunWithClock(ctx context.Context, stateDir, version, endpoint string, now func() time.Time, poll time.Duration) {
	if endpoint == "" {
		return
	}
	for {
		sendIfDue(stateDir, version, endpoint, now())
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
	}
}

// sendIfDue posts synchronously unless the stamp already records t's UTC day.
func sendIfDue(stateDir, version, endpoint string, t time.Time) {
	day := t.UTC().Format("2006-01-02")
	stamp := filepath.Join(stateDir, "telemetry-last-date")
	if data, err := os.ReadFile(stamp); err == nil && strings.TrimSpace(string(data)) == day {
		return
	}
	username := os.Getenv("USER")
	if username == "" {
		username = os.Getenv("USERNAME")
	}
	digest := sha256.Sum256([]byte(username))
	body, _ := json.Marshal(struct {
		Username string `json:"username"`
		Version  string `json:"version"`
	}{
		Username: hex.EncodeToString(digest[:])[:12], Version: version,
	})
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Post(endpoint, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_ = os.WriteFile(stamp, []byte(day+"\n"), 0600)
	}
}
