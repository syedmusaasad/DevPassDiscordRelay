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
		t.Logf("user-manager probe failed: systemd-run unavailable: %v; host relay-lifecycle target required", err)
		return
	}
	probe := exec.Command("systemctl", "--user", "show-environment")
	if out, err := probe.CombinedOutput(); err != nil {
		t.Logf("user-manager probe failed: %v: %s; host relay-lifecycle target required", err, out)
		return
	} else {
		t.Logf("user-manager probe passed: %s", strings.TrimSpace(string(out)))
	}
	mustBin(t, "tmux")
	relayBin, dpmuxBin, mockDS := mustBin(t, "relay"), mustBin(t, "dpmux"), mustBin(t, "mock-discord")
	xdg, state, tmuxTmp := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("TMUX_TMPDIR", tmuxTmp)
	port, gwPort := "19928", "19929"
	startMockDiscord(t, mockDS, port, gwPort)
	if err := waitForHTTP("http://127.0.0.1:"+port+"/api/v10/gateway", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	unit := fmt.Sprintf("relay-test-%d", os.Getpid())
	fakeBin := mustBin(t, "fake-devpass-code")
	seed := exec.Command(fakeBin, "--auto")
	seed.Env = append(os.Environ(), "XDG_DATA_HOME="+xdg)
	if err := seed.Run(); err != nil {
		t.Fatalf("seed read-only relay DB: %v", err)
	}
	baseEnv := []string{"XDG_DATA_HOME=" + xdg, "TMUX_TMPDIR=" + tmuxTmp, "RELAY_STATE_DIR=" + state, "RELAY_DEVPASS_DB=" + filepath.Join(xdg, "devpass-code", "opencode.db"), "RELAY_DPMUX_BIN=" + dpmuxBin, "RELAY_DEVPASS_BIN=" + fakeBin, "FAKE_DP_FORCE_NEW_SESSION=1", "DISCORD_TOKEN=test-token", "DISCORD_CHANNEL_ID=test-channel-001", "DISCORD_USER_ID=user-allowed-001", "DISCORD_API_URL=http://127.0.0.1:" + port}
	args := []string{"--user", "--unit", unit, "--property=Restart=no", "env"}
	args = append(args, baseEnv...)
	args = append(args, relayBin)
	if out, err := exec.Command(systemdRun, args...).CombinedOutput(); err != nil {
		t.Fatalf("start relay unit: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("systemctl", "--user", "stop", unit).Run() })
	if err := dispatchEvent(port, buildMessageEvent("restart-spawn", "test-channel-001", "user-allowed-001", false, "restart proof", nil)); err != nil {
		t.Fatal(err)
	}
	var target string
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
		if target != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if target == "" {
		t.Fatal("relay did not spawn dpmux pending marker")
	}
	if err := waitForPaneText(t, target, "Ask anything...", 10*time.Second); err != nil {
		stderr, _ := os.ReadFile(filepath.Join(state, "stderr-"+strings.Split(target, ":")[0]+".log"))
		unitStatus, _ := exec.Command("systemctl", "--user", "status", unit, "--no-pager").CombinedOutput()
		t.Fatalf("spawned pane not alive before restart: %v\ndpmux stderr:\n%s\nrelay unit:\n%s", err, stderr, unitStatus)
	}
	if out, err := exec.Command("systemctl", "--user", "restart", unit).CombinedOutput(); err != nil {
		t.Logf("transient unit restart failed (%v): %s; replacing relay unit", err, out)
		if stopOut, stopErr := exec.Command("systemctl", "--user", "stop", unit).CombinedOutput(); stopErr != nil {
			t.Fatalf("stop transient relay unit: %v\n%s", stopErr, stopOut)
		}
		if freshOut, freshErr := exec.Command(systemdRun, args...).CombinedOutput(); freshErr != nil {
			t.Fatalf("replace transient relay unit: %v\n%s", freshErr, freshOut)
		}
	}
	if err := waitForPaneText(t, target, "Ask anything...", 10*time.Second); err != nil {
		t.Fatalf("tmux pane did not survive relay restart: %v", err)
	}
}
