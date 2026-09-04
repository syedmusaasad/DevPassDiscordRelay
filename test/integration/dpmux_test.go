//go:build integration

// dpmux_test.go: integration tests for cmd/dpmux.
package integration

import (
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
// Approach: point RELAY_DEVPASS_BIN at a stub shell script that, as its very first
// action, records whether the pending-* marker already existed in RELAY_STATE_DIR.
// If the marker exists at stub start, the ordering is correct.
func TestDpmux_MarkerBeforeSpawn(t *testing.T) {
	mustBin(t, "tmux")
	dpmuxBin := mustBin(t, "dpmux")

	stateDir := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "marker-check.log")

	// Build a stub binary (shell script wrapped in a tmux-launchable form).
	// The stub checks for pending-* files and logs the result, then sleeps briefly
	// so tmux doesn't exit immediately.
	stubScript := filepath.Join(t.TempDir(), "stub-devpass-code")
	stubContent := fmt.Sprintf(`#!/bin/sh
# Stub devpass-code: check if pending marker exists at startup.
marker_found=false
for f in "%s"/pending-*; do
    if [ -f "$f" ]; then
        marker_found=true
        break
    fi
done
echo "marker_found=$marker_found" > "%s"
# Sleep briefly so tmux session stays alive long enough to be useful.
sleep 1
`, stateDir, logFile)
	if err := os.WriteFile(stubScript, []byte(stubContent), 0755); err != nil {
		t.Fatalf("write stub script: %v", err)
	}

	// Run dpmux with the stub as the devpass-code binary.
	cmd := exec.Command(dpmuxBin)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("RELAY_DEVPASS_BIN=%s", stubScript),
		fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir),
		"TMUX=", // unset TMUX so dpmux creates a fresh session
	)
	cmd.Stdout = os.Stderr // route dpmux stdout to test log
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Logf("dpmux exit: %v (may be expected if tmux session exits cleanly)", err)
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

	t.Logf("stub log: %s", logContent)
	if !strings.Contains(logContent, "marker_found=true") {
		t.Errorf("pending marker was NOT present when stub started; got: %q", logContent)
		t.Errorf("pending marker must be written BEFORE spawning the devpass-code process")
	} else {
		t.Log("✓ pending marker existed before devpass-code stub started")
	}

	// Clean up any lingering tmux sessions created by the test.
	// List sessions and kill any dp-* ones.
	if out, err := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output(); err == nil {
		for _, sess := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.HasPrefix(sess, "dp-") {
				exec.Command("tmux", "kill-session", "-t", sess).Run()
			}
		}
	}
}
