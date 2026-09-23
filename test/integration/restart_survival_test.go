//go:build integration

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

// TestRestartSurvival_DisposableUnit starts the relay itself in a disposable
// user unit, sends a spawn through its Discord path, restarts that unit, and
// proves the dpmux-created tmux window remains alive outside the relay cgroup.
func TestRestartSurvival_DisposableUnit(t *testing.T) {
	systemdRun, err := exec.LookPath("systemd-run")
	if err != nil {
		t.Skipf("user-manager probe failed: systemd-run unavailable: %v; host relay-lifecycle target required", err)
	}
	probe := exec.Command("systemctl", "--user", "show-environment")
	if out, err := probe.CombinedOutput(); err != nil {
		t.Skipf("user-manager probe failed: %v: %s; host relay-lifecycle target required", err, out)
	} else {
		t.Logf("user-manager probe passed: %s", strings.TrimSpace(string(out)))
	}
	mustBin(t, "tmux")
	relayBin, dpmuxBin, mockDS := mustBin(t, "relay"), mustBin(t, "dpmux"), mustBin(t, "mock-discord")
	xdg, state, tmuxTmp := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("TMUX_TMPDIR", tmuxTmp)
	tmuxBin := filepath.Join(t.TempDir(), "relay-test-tmux")
	// Keep all commands on dpmux's explicit `-L default` socket. TMUX_TMPDIR
	// makes that default socket isolated; adding a second -L here would make
	// the observer look at a different server from the spawned pane.
	if err := os.WriteFile(tmuxBin, []byte("#!/bin/sh\nexec /usr/bin/tmux \"$@\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	port, gwPort := "19928", "19929"
	startMockDiscord(t, mockDS, port, gwPort)
	if err := waitForHTTP("http://127.0.0.1:"+port+"/api/v10/gateway", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	unit := fmt.Sprintf("relay-test-%d", os.Getpid())
	relayLog := filepath.Join(state, "relay.log")
	fakeBin := mustBin(t, "fake-devpass-code")
	seed := exec.Command(fakeBin, "--auto")
	seed.Env = append(os.Environ(), "XDG_DATA_HOME="+xdg)
	if err := seed.Run(); err != nil {
		t.Fatalf("seed read-only relay DB: %v", err)
	}
	baseEnv := []string{"XDG_DATA_HOME=" + xdg, "TMUX_TMPDIR=" + tmuxTmp, "RELAY_TMUX_BIN=" + tmuxBin, "RELAY_STATE_DIR=" + state, "RELAY_DEVPASS_DB=" + filepath.Join(xdg, "devpass-code", "opencode.db"), "RELAY_DPMUX_BIN=" + dpmuxBin, "RELAY_DEVPASS_BIN=" + fakeBin, "FAKE_DP_FORCE_NEW_SESSION=1", "DISCORD_TOKEN=test-token", "DISCORD_CHANNEL_ID=test-channel-001", "DISCORD_USER_ID=user-allowed-001", "DISCORD_API_URL=http://127.0.0.1:" + port}
	// --no-block is essential here: without it systemd-run waits for the
	// long-running relay to exit, so this test never reaches the restart proof.
	args := []string{"--user", "--no-block", "--unit", unit, "--property=Restart=no", "env"}
	args = append(args, baseEnv...)
	args = append(args, "sh", "-c", "exec \"$1\" >\"$2\" 2>&1", "relay-test", relayBin, relayLog)
	if out, err := exec.Command(systemdRun, args...).CombinedOutput(); err != nil {
		t.Fatalf("start relay unit: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("systemctl", "--user", "stop", unit).Run() })
	// The mock gateway accepts dispatch only after relay has completed its
	// websocket handshake; a transient unit has no process handle to wait on.
	time.Sleep(500 * time.Millisecond)
	var target string
	// Allow the gateway identify/READY exchange to settle before the one real
	// spawn event. Re-dispatching would legitimately create multiple concurrent
	// fake CLI writers against the same SQLite database.
	time.Sleep(time.Second)
	if err := dispatchEvent(port, buildMessageEvent("restart-spawn", "test-channel-001", "user-allowed-001", false, "restart proof", nil)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(state)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "pending-") {
				data, _ := os.ReadFile(filepath.Join(state, e.Name()))
				var marker struct {
					Target string `json:"target"`
				}
				if json.Unmarshal(data, &marker) == nil && marker.Target != "" {
					target = marker.Target
					break
				}
			}
		}
		// The relay consumes a verified pending marker quickly. Its durable
		// sessions store retains the exact dpmux target after that handoff.
		if target == "" {
			var records []struct {
				Target string `json:"tmux_target"`
			}
			if data, err := os.ReadFile(filepath.Join(state, "sessions.json")); err == nil && json.Unmarshal(data, &records) == nil && len(records) > 0 {
				target = records[0].Target
			}
		}
		if target != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if target == "" {
		unitStatus, _ := exec.Command("systemctl", "--user", "status", unit, "--no-pager").CombinedOutput()
		journal, _ := exec.Command("journalctl", "--user", "-u", unit, "--no-pager").CombinedOutput()
		logData, _ := os.ReadFile(relayLog)
		t.Fatalf("relay did not spawn dpmux pending marker\nrelay unit:\n%s\nrelay journal:\n%s\nrelay log:\n%s", unitStatus, journal, logData)
	}
	if err := waitForPaneTextWithEnv(t, target, "Ask anything...", 10*time.Second, "TMUX_TMPDIR="+tmuxTmp, "RELAY_TMUX_BIN="+tmuxBin); err != nil {
		stderr, _ := os.ReadFile(filepath.Join(state, "stderr-"+strings.Split(target, ":")[0]+".log"))
		unitStatus, _ := exec.Command("systemctl", "--user", "status", unit, "--no-pager").CombinedOutput()
		t.Fatalf("spawned pane not alive before restart: %v\ndpmux stderr:\n%s\nrelay unit:\n%s", err, stderr, unitStatus)
	}
	if out, err := exec.Command("systemctl", "--user", "restart", unit).CombinedOutput(); err != nil {
		t.Fatalf("restart original transient relay unit: %v\n%s", err, out)
	}
	if err := waitForPaneTextWithEnv(t, target, "Ask anything...", 10*time.Second, "TMUX_TMPDIR="+tmuxTmp, "RELAY_TMUX_BIN="+tmuxBin); err != nil {
		t.Fatalf("tmux pane did not survive relay restart: %v", err)
	}
}

func waitForPaneTextWithEnv(t *testing.T, target, want string, timeout time.Duration, env ...string) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var latest []byte
	for time.Now().Before(deadline) {
		tmux := os.Getenv("RELAY_TMUX_BIN")
		for _, entry := range env {
			if strings.HasPrefix(entry, "RELAY_TMUX_BIN=") {
				tmux = strings.TrimPrefix(entry, "RELAY_TMUX_BIN=")
			}
		}
		cmd := exec.Command(tmux, "capture-pane", "-t", target, "-p")
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.Output()
		if err == nil {
			latest = out
			if strings.Contains(string(out), want) {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout after %s; pane content:\n%s", timeout, latest)
}
