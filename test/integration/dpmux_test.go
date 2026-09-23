//go:build integration

// dpmux_test.go: integration tests for cmd/dpmux.
package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDpmux_MarkerBeforeSpawn verifies that dpmux writes the pending-<name> marker
// file BEFORE spawning the devpass-code process. This ensures the relay can pair the
// session even if the process starts and creates a session row before dpmux writes
// the marker.
//
// Approach: point RELAY_DEVPASS_BIN at a stub that records whether the marker
// existed at the child's own startup.
func TestDpmux_MarkerBeforeSpawn(t *testing.T) {
	mustBin(t, "tmux")
	dpmuxBin := mustBin(t, "dpmux")

	stateDir := dpmuxStateDir(t)
	logFile := filepath.Join(t.TempDir(), "marker-check.log")

	// The pending marker is the durable pre-spawn contract. The child observes
	// it before doing anything else.
	stubScript := filepath.Join(t.TempDir(), "stub-devpass-code")
	stubContent := fmt.Sprintf(`#!/bin/sh
# Stub devpass-code: observe the marker at process startup, then stay alive.
marker_found=false
for marker in "%s"/pending-*; do
  if [ -e "$marker" ]; then marker_found=true; fi
done
printf 'marker_found=%%s\n' "$marker_found" > "%s"
# Sleep briefly so tmux session stays alive long enough to be useful.
sleep 1
`, stateDir, logFile)
	if err := os.WriteFile(stubScript, []byte(stubContent), 0755); err != nil {
		t.Fatalf("write stub script: %v", err)
	}

	// Run dpmux with the stub as the devpass-code binary.
	cmd := exec.Command(dpmuxBin)
	var outBuf strings.Builder
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("RELAY_DEVPASS_BIN=%s", stubScript),
		fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir),
		"TMUX=", // unset TMUX so dpmux creates a fresh session
	)
	cmd.Stdout = &outBuf
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Logf("dpmux exit: %v (may be expected if tmux session exits cleanly)", err)
	}
	if sessionName := strings.TrimSpace(outBuf.String()); sessionName != "" {
		t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", sessionName).Run() })
	}

	// Wait for the stub to write its log.
	deadline := time.Now().Add(10 * time.Second)
	var logContent string
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(logFile)
		if err == nil && len(data) > 0 {
			logContent = strings.TrimSpace(string(data))
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if logContent == "" {
		t.Fatalf("stub did not write log file within 10s (marker check not executed)")
	}
	if logContent != "marker_found=true" {
		t.Fatalf("child started before pending marker existed: %q", logContent)
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	var markerPath string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "pending-") {
			markerPath = filepath.Join(stateDir, entry.Name())
			break
		}
	}
	if markerPath == "" {
		t.Fatal("pending marker was absent while the spawned child was alive")
	}
	markerData, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	var marker dpmuxPendingMarker
	if err := json.Unmarshal(markerData, &marker); err != nil || marker.SpawnInstant == 0 {
		t.Fatalf("pending marker missing durable spawn instant: %v (%q)", err, markerData)
	}
	if marker.SpawnInstant > time.Now().UnixMilli() {
		t.Fatalf("pending marker spawn instant %d is in the future", marker.SpawnInstant)
	}
	t.Log("✓ pending marker preceded devpass-code startup")

}

// TestDpmux_NewWindowMarkerBeforeSpawn verifies that dpmux writes the pending marker
// BEFORE running `tmux new-window` on the inside-pane (default-socket) path.
//
// Approach:
//   - Start a real tmux session on the DEFAULT per-uid socket.
//   - Run dpmux with TMUX set to look like the default socket, with a stub
//     devpass-code that observes the marker at its own startup.
//   - Verify the marker was already present.
func TestDpmux_NewWindowMarkerBeforeSpawn(t *testing.T) {
	mustBin(t, "tmux")
	dpmuxBin := mustBin(t, "dpmux")

	stateDir := dpmuxStateDir(t)
	logFile := filepath.Join(t.TempDir(), "nw-marker-check.log")

	// Create a host tmux session on the default socket.
	// Ensure a previous short-lived test server cannot leave a stale socket.
	_ = exec.Command("tmux", "-L", "default", "kill-server").Run()
	hostSession := fmt.Sprintf("test-nw-host-%d", time.Now().UnixMilli())
	startCmd := exec.Command("tmux", "-L", "default", "new-session", "-d", "-s", hostSession,
		"-x", "220", "-y", "50", "sleep 60")
	if out, err := startCmd.CombinedOutput(); err != nil {
		t.Fatalf("create host tmux session: %v: %s", err, out)
	}
	t.Cleanup(func() {
		exec.Command("tmux", "-L", "default", "kill-session", "-t", hostSession).Run()
	})

	// Build a stub that records the durable marker state at its own startup.
	stubScript := filepath.Join(t.TempDir(), "stub-devpass-code")
	stubContent := fmt.Sprintf(`#!/bin/sh
# Stub devpass-code: observe marker state before remaining alive.
marker_found=false
for marker in "%s"/pending-*; do
  if [ -e "$marker" ]; then marker_found=true; fi
done
printf 'marker_found=%%s\n' "$marker_found" > "%s"
sleep 1
`, stateDir, logFile)
	if err := os.WriteFile(stubScript, []byte(stubContent), 0755); err != nil {
		t.Fatalf("write stub script: %v", err)
	}

	// Determine the default socket path so we can fake TMUX correctly.
	defaultSocket := fmt.Sprintf("/tmp/tmux-%d/default", os.Getuid())
	// Construct a TMUX env var that looks like we are inside the default socket.
	// Format: <socket-path>,<server-pid>,<pane-window-index>
	fakeServerPID := "99999"
	fakeTMUXEnv := defaultSocket + "," + fakeServerPID + ",0"

	// Run dpmux as if inside the default socket.
	cmd := exec.Command(dpmuxBin)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("RELAY_DEVPASS_BIN=%s", stubScript),
		fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir),
		// Set TMUX to look like we are inside the default socket so dpmux takes
		// the new-window path.  extractHostSessionName queries "tmux display-message -p #S"
		// which will return the hostSession we created above.
		fmt.Sprintf("TMUX=%s", fakeTMUXEnv),
	)
	var outBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Logf("dpmux exit: %v", err)
	}
	t.Logf("dpmux stdout: %q", outBuf.String())

	// Wait for the stub to write its log.
	deadline := time.Now().Add(15 * time.Second)
	var logContent string
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(logFile)
		if err == nil && len(data) > 0 {
			logContent = strings.TrimSpace(string(data))
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if logContent == "" {
		t.Fatalf("stub did not write log within 15s (new-window marker check not executed)")
	}
	if logContent != "marker_found=true" {
		t.Fatalf("new-window child started before pending marker existed: %q", logContent)
	}

	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	var markerPath string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "pending-") {
			markerPath = filepath.Join(stateDir, entry.Name())
			break
		}
	}
	if markerPath == "" {
		t.Fatal("pending marker was absent while the new-window child was alive")
	}
	t.Log("✓ pending marker preceded devpass-code startup on new-window path")

	// Clean up dp-* tmux windows in the host session.
	t.Cleanup(func() {
		if out, err := exec.Command("tmux", "-L", "default", "list-windows",
			"-t", hostSession, "-F", "#{window_name}").Output(); err == nil {
			for _, w := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if strings.HasPrefix(w, "dp-") {
					exec.Command("tmux", "-L", "default", "kill-window",
						"-t", hostSession+":="+w).Run()
				}
			}
		}
	})
}

// dpmuxPendingMarker is the JSON structure of a pending marker file.
type dpmuxPendingMarker struct {
	Target       string `json:"target"`
	Nonce        string `json:"nonce"`
	SpawnInstant int64  `json:"spawnInstant"`
}

// dpmuxStateDir stays outside /tmp because the scope used for a fresh tmux
// birth has a private /tmp; /workspace is the shared integration-test mount.
func dpmuxStateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/workspace", "dpmux-marker-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestDpmux_ForeignSocketFallsBackToDefault verifies that when dpmux is invoked
// with $TMUX pointing to a non-default tmux socket (a "foreign" socket), it
// ignores that socket and creates the devpass-code session on the default
// per-uid socket instead.
//
// Approach:
//   - Start a second tmux server on socket name "relaytest" (-L relaytest).
//   - Set TMUX to the relaytest socket path to simulate running inside a foreign socket.
//   - Verify dpmux creates the new session on the DEFAULT socket (not relaytest).
func TestDpmux_ForeignSocketFallsBackToDefault(t *testing.T) {
	mustBin(t, "tmux")
	dpmuxBin := mustBin(t, "dpmux")

	stateDir := t.TempDir()

	// Start a tmux server on the "relaytest" socket (separate from default).
	relaytestSession := fmt.Sprintf("test-foreign-%d", time.Now().UnixMilli())
	startCmd := exec.Command("tmux", "-L", "relaytest", "new-session", "-d",
		"-s", relaytestSession, "-x", "220", "-y", "50", "sleep 60")
	if err := startCmd.Run(); err != nil {
		t.Fatalf("create relaytest tmux session: %v", err)
	}
	t.Cleanup(func() {
		exec.Command("tmux", "-L", "relaytest", "kill-server").Run()
	})

	// Get the socket path for the relaytest server.
	// tmux sockets are in /tmp/tmux-<uid>/ by default, named after "-L <name>".
	relaytestSocket := fmt.Sprintf("/tmp/tmux-%d/relaytest", os.Getuid())

	// Build a minimal stub devpass-code that exits immediately.
	stubScript := filepath.Join(t.TempDir(), "stub-devpass-code")
	stubContent := `#!/bin/sh
# Minimal stub: just exit so tmux session ends quickly.
sleep 1
`
	if err := os.WriteFile(stubScript, []byte(stubContent), 0755); err != nil {
		t.Fatalf("write stub script: %v", err)
	}

	// Construct a TMUX env var as if running inside the relaytest socket.
	fakeServerPID := "88888"
	fakeTMUXEnv := relaytestSocket + "," + fakeServerPID + ",0"

	// Run dpmux with TMUX pointing to the non-default relaytest socket.
	cmd := exec.Command(dpmuxBin)
	var outBuf strings.Builder
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("RELAY_DEVPASS_BIN=%s", stubScript),
		fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir),
		fmt.Sprintf("TMUX=%s", fakeTMUXEnv), // non-default socket
	)
	cmd.Stdout = &outBuf
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Logf("dpmux exit: %v", err)
	}

	sessionName := strings.TrimSpace(outBuf.String())
	if sessionName == "" {
		t.Fatalf("dpmux produced no session name on stdout")
	}
	t.Logf("dpmux reported session: %q", sessionName)

	t.Cleanup(func() {
		exec.Command("tmux", "-L", "default", "kill-session", "-t", sessionName).Run()
		os.Remove(filepath.Join(stateDir, "pending-"+sessionName))
	})

	// Verify the session was created on the DEFAULT socket, not relaytest.
	defaultCheck := exec.Command("tmux", "-L", "default", "has-session", "-t", sessionName)
	if err := defaultCheck.Run(); err != nil {
		t.Errorf("session %q not found on default socket: dpmux may have created it on the wrong socket (err: %v)", sessionName, err)
	} else {
		t.Logf("✓ session %q exists on default socket", sessionName)
	}

	// Verify the session was NOT created on the relaytest socket.
	relaytestCheck := exec.Command("tmux", "-L", "relaytest", "has-session", "-t", sessionName)
	if relaytestCheck.Run() == nil {
		t.Errorf("session %q found on relaytest socket; dpmux should have used the default socket for foreign-socket callers", sessionName)
	} else {
		t.Logf("✓ session %q not on relaytest socket (correct)", sessionName)
	}

	// Verify the pending marker was written with a nonce.
	markerPath := filepath.Join(stateDir, "pending-"+sessionName)
	markerData, err := os.ReadFile(markerPath)
	if err != nil {
		t.Logf("marker already cleaned up or not written: %v", err)
	} else {
		var m dpmuxPendingMarker
		if jsonErr := json.Unmarshal(markerData, &m); jsonErr != nil {
			t.Errorf("pending marker is not valid JSON: %v (content: %q)", jsonErr, markerData)
		} else if m.Nonce == "" {
			t.Error("pending marker has empty nonce; nonce is required for session binding")
		} else {
			t.Logf("✓ pending marker nonce=%q target=%q", m.Nonce, m.Target)
		}
	}
}
