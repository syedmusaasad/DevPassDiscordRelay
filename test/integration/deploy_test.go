//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRelay_RefusesWithoutUserID starts the relay with DISCORD_USER_ID unset
// and then empty. It must exit non-zero with exactly one log line naming the
// variable, and must never reach the watcher.
func TestRelay_RefusesWithoutUserID(t *testing.T) {
	relayBin := mustBin(t, "relay")
	for _, tc := range []struct {
		name string
		env  []string
	}{
		{"unset", nil},
		{"empty", []string{"DISCORD_USER_ID="}},
		{"blank", []string{"DISCORD_USER_ID=   "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, state := t.TempDir(), t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, relayBin)
			cmd.Dir = home // no repo-root .env fallback
			cmd.Env = []string{
				"PATH=" + os.Getenv("PATH"), "HOME=" + home,
				"DISCORD_TOKEN=test-token", "DISCORD_CHANNEL_ID=test-channel-001",
				"DISCORD_API_URL=http://127.0.0.1:1", "RELAY_STATE_DIR=" + state,
			}
			cmd.Env = append(cmd.Env, tc.env...)
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			err := cmd.Run()
			if ctx.Err() != nil {
				t.Fatalf("relay did not exit on its own:\n%s", out.String())
			}
			exitErr, ok := err.(*exec.ExitError)
			if !ok || exitErr.ExitCode() == 0 {
				t.Fatalf("relay err = %v, want non-zero exit\n%s", err, out.String())
			}
			lines := 0
			for _, l := range strings.Split(out.String(), "\n") {
				if strings.Contains(l, "DISCORD_USER_ID") {
					lines++
				}
			}
			if lines != 1 {
				t.Fatalf("want exactly one log line naming DISCORD_USER_ID, got %d:\n%s", lines, out.String())
			}
			if strings.Contains(out.String(), "starting watcher") {
				t.Fatalf("relay reached the watcher without DISCORD_USER_ID:\n%s", out.String())
			}
			t.Logf("exit=%d output=%q", exitErr.ExitCode(), strings.TrimSpace(out.String()))
		})
	}
}

// TestInstalledUnit_ResolvesDpmux installs into a temp HOME through install.sh,
// then runs the installed relay with exactly the unit's Environment lines and a
// minimal PATH (/usr/bin:/bin, no ~/.local/bin) under a disposable
// systemd-run --user unit. A spawn must still reach the installed dpmux, which
// is only possible through the unit's RELAY_DPMUX_BIN.
func TestInstalledUnit_ResolvesDpmux(t *testing.T) {
	systemdRun, err := exec.LookPath("systemd-run")
	if err != nil {
		t.Skipf("systemd-run --user unavailable: probe failed: %v", err)
	}
	if out, err := exec.Command("systemctl", "--user", "show-environment").CombinedOutput(); err != nil {
		t.Skipf("systemd-run --user unavailable: probe failed: %v: %s", err, out)
	}
	relayBin, mockDS, fakeBin := mustBin(t, "relay"), mustBin(t, "mock-discord"), mustBin(t, "fake-devpass-code")
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	unitSrc := filepath.Join(repo, "systemd", "devpass-discord-relay.service")
	installSh := filepath.Join(repo, "install.sh")

	home, state, srcBin := t.TempDir(), t.TempDir(), t.TempDir()
	ranMarker := filepath.Join(state, "stub-dpmux-ran")
	// Stub dpmux: proves the installed path was executed without starting a CLI.
	stub := fmt.Sprintf("#!/bin/sh\necho \"$0\" > %q\necho dp-stub\n", ranMarker)
	if err := os.WriteFile(filepath.Join(srcBin, "dpmux"), []byte(stub), 0755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(relayBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcBin, "relay"), data, 0755); err != nil {
		t.Fatal(err)
	}
	inst := exec.Command("bash", installSh)
	inst.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home,
		"RELAY_INSTALL_SRC_BIN=" + srcBin, "RELAY_INSTALL_SKIP_BUILD=1", "RELAY_INSTALL_SKIP_SYSTEMD=1",
	}
	if out, err := inst.CombinedOutput(); err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	installedDpmux := filepath.Join(home, ".local", "bin", "dpmux")
	installedUnit := filepath.Join(home, ".config", "systemd", "user", "devpass-discord-relay.service")
	unitBytes, err := os.ReadFile(installedUnit)
	if err != nil {
		t.Fatalf("install.sh did not install the unit: %v", err)
	}
	if orig, _ := os.ReadFile(unitSrc); !bytes.Equal(orig, unitBytes) {
		t.Fatal("installed unit differs from systemd/devpass-discord-relay.service")
	}

	// Take the unit's Environment lines and ExecStart verbatim (with %h expanded).
	unitEnv := map[string]string{}
	execStart := ""
	for _, line := range strings.Split(string(unitBytes), "\n") {
		line = strings.ReplaceAll(strings.TrimSpace(line), "%h", home)
		switch {
		case strings.HasPrefix(line, "Environment="):
			kv := strings.TrimPrefix(line, "Environment=")
			if i := strings.Index(kv, "="); i > 0 {
				unitEnv[kv[:i]] = kv[i+1:]
			}
		case strings.HasPrefix(line, "ExecStart="):
			execStart = strings.TrimPrefix(line, "ExecStart=")
		}
	}
	if unitEnv["RELAY_DPMUX_BIN"] != installedDpmux {
		t.Fatalf("unit RELAY_DPMUX_BIN = %q, want %q", unitEnv["RELAY_DPMUX_BIN"], installedDpmux)
	}
	if execStart != filepath.Join(home, ".local", "bin", "relay") {
		t.Fatalf("unit ExecStart = %q", execStart)
	}
	// Minimal PATH: the user manager's default, without ~/.local/bin.
	unitEnv["PATH"] = "/usr/bin:/bin"

	// Seed the DB at the unit's XDG_DATA_HOME so the read-only open succeeds.
	seed := exec.Command(fakeBin, "--auto")
	seed.Env = append(os.Environ(), "XDG_DATA_HOME="+unitEnv["XDG_DATA_HOME"])
	if out, err := seed.CombinedOutput(); err != nil {
		t.Fatalf("seed relay DB: %v\n%s", err, out)
	}

	port, gwPort := "19938", "19939"
	startMockDiscord(t, mockDS, port, gwPort)
	if err := waitForHTTP("http://127.0.0.1:"+port+"/api/v10/gateway", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	unit := fmt.Sprintf("relay-unit-test-%d", os.Getpid())
	relayLog := filepath.Join(state, "relay.log")
	args := []string{"--user", "--no-block", "--unit", unit, "--property=Restart=no"}
	for k, v := range unitEnv {
		args = append(args, "--setenv="+k+"="+v)
	}
	for _, kv := range []string{
		"RELAY_STATE_DIR=" + state, "DISCORD_TOKEN=test-token", "DISCORD_CHANNEL_ID=test-channel-001",
		"DISCORD_USER_ID=user-allowed-001", "DISCORD_API_URL=http://127.0.0.1:" + port,
	} {
		args = append(args, "--setenv="+kv)
	}
	args = append(args, "sh", "-c", "exec \"$1\" >\"$2\" 2>&1", "relay-unit-test", execStart, relayLog)
	if out, err := exec.Command(systemdRun, args...).CombinedOutput(); err != nil {
		t.Fatalf("start relay unit: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("systemctl", "--user", "stop", unit).Run() })

	waitForRelayReady(t, state)
	if err := dispatchEvent(port, buildMessageEvent("unit-spawn", "test-channel-001", "user-allowed-001", false, "spawn via installed unit", nil)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(ranMarker); err == nil {
			if got := strings.TrimSpace(string(b)); got != installedDpmux {
				t.Fatalf("stub ran as %q, want installed path %q", got, installedDpmux)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	logData, _ := os.ReadFile(relayLog)
	status, _ := exec.Command("systemctl", "--user", "status", unit, "--no-pager").CombinedOutput()
	t.Fatalf("installed dpmux never ran under minimal PATH\nunit:\n%s\nrelay log:\n%s", status, logData)
}
