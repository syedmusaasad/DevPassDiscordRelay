//go:build integration

// Package integration contains integration tests that require tmux,
// fake-devpass-code, and mock-discord to be on PATH.
//
// Run via: make integration
// Or directly: go test -tags integration ./test/integration/...
package integration

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// mustBin returns the path to a binary that must be on PATH.
func mustBin(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("binary %q not found on PATH: %v", name, err)
	}
	return p
}

// TestSmoke_FakeDevpassCodeStartsAndReady verifies:
//  1. fake-devpass-code starts in a tmux pane
//  2. The ready signature "Ask anything..." appears in the pane
//  3. A session row appears in the SQLite DB
func TestSmoke_FakeDevpassCodeStartsAndReady(t *testing.T) {
	// Require binaries
	mustBin(t, "tmux")
	fakeDP := mustBin(t, "fake-devpass-code")

	// Isolated XDG home
	xdgHome := t.TempDir()
	dbPath := filepath.Join(xdgHome, "devpass-code", "opencode.db")

	sessionName := fmt.Sprintf("smoke-test-%d", time.Now().UnixMilli())

	// Kill any leftover session on test exit
	t.Cleanup(func() {
		exec.Command("tmux", "kill-session", "-t", sessionName).Run()
	})

	// Launch fake-devpass-code in a tmux pane
	cmd := exec.Command("tmux", "new-session", "-d",
		"-s", sessionName,
		"-x", "200", "-y", "50",
		"env",
		fmt.Sprintf("XDG_DATA_HOME=%s", xdgHome),
		fakeDP, "--auto",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to start tmux session: %v\n%s", err, out)
	}

	// Wait for "Ask anything..." to appear in the pane
	paneTarget := sessionName + ":0.0"
	if err := waitForPaneText(t, paneTarget, "Ask anything...", 10*time.Second); err != nil {
		t.Fatalf("ready signature not seen: %v", err)
	}
	t.Log("✓ ready signature 'Ask anything...' appeared in pane")

	// Wait for the DB file to appear
	if err := waitForFile(dbPath, 5*time.Second); err != nil {
		t.Fatalf("DB file not created: %v", err)
	}
	t.Logf("✓ DB file created at %s", dbPath)

	// Open the DB and wait for a session row
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open DB: %v", err)
	}
	defer db.Close()

	var sessionID string
	if err := waitForSessionRow(t, db, &sessionID, 5*time.Second); err != nil {
		t.Fatalf("session row not found: %v", err)
	}
	t.Logf("✓ session row found: id=%s", sessionID)

	if !strings.HasPrefix(sessionID, "ses_") {
		t.Errorf("session ID should start with 'ses_', got %q", sessionID)
	}
}

// TestSmoke_FakeDevpassCodeRefusesUnknownSession verifies that --session with
// an unknown ID causes "Error: Session not found: <id>" on stderr and exits non-zero.
func TestSmoke_FakeDevpassCodeRefusesUnknownSession(t *testing.T) {
	mustBin(t, "tmux")
	fakeDP := mustBin(t, "fake-devpass-code")

	xdgHome := t.TempDir()
	bogusID := "ses_doesnotexist999"

	cmd := exec.Command(fakeDP, "--session", bogusID)
	cmd.Env = append(os.Environ(), fmt.Sprintf("XDG_DATA_HOME=%s", xdgHome))
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatal("expected non-zero exit for unknown session, got nil error")
	}
	if !strings.Contains(string(out), fmt.Sprintf("Error: Session not found: %s", bogusID)) {
		t.Errorf("expected refusal text in output, got:\n%s", out)
	}
	t.Logf("✓ refusal text correct: %s", strings.TrimSpace(string(out)))
}

// TestSmoke_FakeDevpassCodeRefuseFirstResume verifies FAKE_DP_REFUSE_FIRST_RESUME=1.
func TestSmoke_FakeDevpassCodeRefuseFirstResume(t *testing.T) {
	fakeDP := mustBin(t, "fake-devpass-code")

	xdgHome := t.TempDir()
	bogusID := "ses_fakeresume001"

	cmd := exec.Command(fakeDP, "--session", bogusID)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("XDG_DATA_HOME=%s", xdgHome),
		"FAKE_DP_REFUSE_FIRST_RESUME=1",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit")
	}
	if !strings.Contains(string(out), "Error: Session not found:") {
		t.Errorf("expected refusal text, got:\n%s", out)
	}
	t.Logf("✓ FAKE_DP_REFUSE_FIRST_RESUME works")
}

// ── helpers ───────────────────────────────────────────────────────────────────

func waitForPaneText(t *testing.T, pane, substr string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd := exec.Command("tmux", "capture-pane", "-t", pane, "-p")
		out, err := cmd.Output()
		if err == nil && strings.Contains(string(out), substr) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	// Last capture for diagnostics
	cmd := exec.Command("tmux", "capture-pane", "-t", pane, "-p")
	out, _ := cmd.Output()
	return fmt.Errorf("timeout after %s; pane content:\n%s", timeout, out)
}

func waitForFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("file %s did not appear within %s", path, timeout)
}

func waitForSessionRow(t *testing.T, db *sql.DB, sessionID *string, timeout time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			row := db.QueryRowContext(ctx, `SELECT id FROM session LIMIT 1`)
			var id string
			if err := row.Scan(&id); err == nil {
				*sessionID = id
				return nil
			}
		}
	}
}
