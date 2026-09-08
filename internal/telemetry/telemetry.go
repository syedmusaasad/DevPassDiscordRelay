// Package telemetry sends the relay's deliberately minimal, best-effort usage heartbeat.
package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Send posts {username, version} at most once each UTC day. Failures are quiet
// and do not create a stamp, so a later healthy startup may retry.
func Send(stateDir, version, endpoint string) {
	if endpoint == "" {
		return
	}
	day := time.Now().UTC().Format("2006-01-02")
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
	go func() {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Post(endpoint, "application/json", strings.NewReader(string(body)))
		if err != nil {
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			_ = os.WriteFile(stamp, []byte(day+"\n"), 0600)
		}
	}()
}
