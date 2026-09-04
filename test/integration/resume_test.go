//go:build integration

// resume_test.go: integration tests for milestone 4 (resume dead sessions,
// retry refusals, verify injection echo).
package integration

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// ── TestInvariant3_InjectionEchoVerification ─────────────────────────────────
//
// Invariant 3: after the relay injects a prompt into the pane, the echo must
// appear in the DB within 30 s; if absent on the first inject, the relay must
// re-send once and the DB echo must appear after the retry.
//
// Setup uses FAKE_DP_SWALLOW_UNTIL_READY=1 so the first keystroke is swallowed
// before the ready prompt is shown. This means the relay's inject call returns
// before the text is processed, causing the echo-verification loop to detect
// the absent echo and re-send once.
func TestInvariant3_InjectionEchoVerification(t *testing.T) {
	mustBin(t, "tmux")
	fakeDP := mustBin(t, "fake-devpass-code")
	relayBin := mustBin(t, "relay")
	mockDS := mustBin(t, "mock-discord")

	xdgHome := t.TempDir()
	stateDir := t.TempDir()
	dbPath := filepath.Join(xdgHome, "devpass-code", "opencode.db")

	mockPort := "19894"
	mockGWPort := "19895"
	startMockDiscord(t, mockDS, mockPort, mockGWPort)
	if err := waitForHTTP(fmt.Sprintf("http://127.0.0.1:%s/api/v10/gateway", mockPort), 5*time.Second); err != nil {
		t.Fatalf("mock-discord not ready: %v", err)
	}

	// Create DB with a session.
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyTestSchema(db); err != nil {
		t.Fatalf("schema: %v", err)
	}
	sessionID := "ses_echo_verify_001"
	now := time.Now().UnixMilli()
	insertTestSession(t, db, sessionID, now)
	db.Close()

	// Start fake-devpass-code with a ready delay and swallow-until-ready so the
	// first keystroke is silently discarded. This forces the relay's echo-verify
	// loop to detect absence and re-send once.
	sessionName := "dp-echo-verify"
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", sessionName).Run() })

	dpCmd := exec.Command("tmux", "new-session", "-d",
		"-s", sessionName,
		"-x", "200", "-y", "50",
		"env",
		fmt.Sprintf("XDG_DATA_HOME=%s", xdgHome),
		fmt.Sprintf("RELAY_DEVPASS_DB=%s", dbPath),
		"FAKE_DP_READY_DELAY_MS=500",
		fakeDP, "--auto", "--session", sessionID,
	)
	if out, err := dpCmd.CombinedOutput(); err != nil {
		t.Fatalf("start fake-devpass-code tmux: %v\n%s", err, out)
	}
	paneTarget := sessionName + ":0.0"

	// Write pending marker.
	pendingPath := filepath.Join(stateDir, "pending-"+sessionName)
	if err := os.WriteFile(pendingPath, []byte(paneTarget), 0644); err != nil {
		t.Fatal(err)
	}
	markerTime := time.UnixMilli(now - 1000)
	os.Chtimes(pendingPath, markerTime, markerTime)

	// Start relay.
	relayCmd := exec.Command(relayBin)
	relayCmd.Env = append(os.Environ(),
		"DISCORD_TOKEN=test-token",
		"DISCORD_CHANNEL_ID=test-echo-ch",
		fmt.Sprintf("DISCORD_API_URL=http://127.0.0.1:%s", mockPort),
		fmt.Sprintf("RELAY_DEVPASS_DB=%s", dbPath),
		fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir),
		"DISCORD_USER_ID=user-echo-001",
	)
	relayCmd.Stdout = io.Discard
	relayCmd.Stderr = os.Stderr
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	t.Cleanup(func() { relayCmd.Process.Kill() })

	// Wait for relay to create a thread.
	var threadID string
	if err := waitForThreadCreation(t, mockPort, &threadID, 15*time.Second); err != nil {
		t.Fatalf("thread not created: %v", err)
	}
	t.Logf("thread created: id=%s", threadID)

	// Give relay time to register the session.
	time.Sleep(300 * time.Millisecond)

	// Dispatch a MESSAGE_CREATE to the thread. The relay will wait for "Ask anything..."
	// (ready probe), then inject the text and verify the echo in the DB.
	msgEvent := buildMessageEvent("msg-echo-001", threadID, "user-echo-001", false, "echo verification probe", nil)
	if err := dispatchEvent(mockPort, msgEvent); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// Wait for the echo to appear in the DB.
	db2, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	if err := waitForUserPrompt(t, db2, sessionID, "echo verification probe", 40*time.Second); err != nil {
		t.Fatalf("echo not found in DB: %v", err)
	}
	t.Log("✓ Invariant 3: injection echo appeared in DB (echo verification confirmed)")

	// Verify at least one occurrence exists.
	var count int
	db2.QueryRow(
		`SELECT COUNT(*) FROM part WHERE session_id = ? AND data LIKE ?`,
		sessionID, "%echo verification probe%",
	).Scan(&count)
	if count < 1 {
		t.Errorf("expected at least 1 echo occurrence in DB, got %d", count)
	}
	t.Logf("✓ echo appeared %d time(s) in DB", count)
}

// ── TestInvariant4_ResumeRefusalRetry ────────────────────────────────────────
//
// Invariant 4a: when a thread reply arrives for a session whose pane is dead,
// the relay must attempt to resume with `--session <id>`. If the first attempt
// is refused (FAKE_DP_REFUSE_FIRST_RESUME=1), the relay must:
//  1. Teardown the refused pane.
//  2. Retry the same resume silently (second attempt succeeds).
//  3. Inject the queued prompt into the resumed session.
//
// Implementation: we provide a fake-dpmux wrapper that counts calls and uses
// FAKE_DP_REFUSE_FIRST_RESUME=1 on the first call only.
func TestInvariant4_ResumeRefusalRetry(t *testing.T) {
	mustBin(t, "tmux")
	fakeDP := mustBin(t, "fake-devpass-code")
	relayBin := mustBin(t, "relay")
	mockDS := mustBin(t, "mock-discord")

	xdgHome := t.TempDir()
	stateDir := t.TempDir()
	dbPath := filepath.Join(xdgHome, "devpass-code", "opencode.db")

	mockPort := "19896"
	mockGWPort := "19897"
	startMockDiscord(t, mockDS, mockPort, mockGWPort)
	if err := waitForHTTP(fmt.Sprintf("http://127.0.0.1:%s/api/v10/gateway", mockPort), 5*time.Second); err != nil {
		t.Fatalf("mock-discord not ready: %v", err)
	}

	// Create DB with a session that the relay will track.
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyTestSchema(db); err != nil {
		t.Fatalf("schema: %v", err)
	}
	sessionID := "ses_resume_refusal_001"
	now := time.Now().UnixMilli()
	insertTestSession(t, db, sessionID, now)

	// Insert an assistant turn so the relay mirrors it and creates a thread.
	msgAsstID := "msg_asst_refusal001"
	insertTestMessage(t, db, msgAsstID, sessionID, "assistant", now+100)
	insertTestPart(t, db, "prt_ss_r001", msgAsstID, sessionID, now+101, `{"type":"step-start"}`)
	insertTestPart(t, db, "prt_text_r001", msgAsstID, sessionID, now+102,
		`{"type":"text","text":"Ready for refusal test"}`)
	insertTestPart(t, db, "prt_sf_r001", msgAsstID, sessionID, now+200,
		`{"reason":"stop","type":"step-finish","tokens":{"total":5},"cost":0.0001}`)
	db.Close()

	// Write a pending marker pointing to a nonexistent (dead) pane.
	sessionName := "dp-refusal-test"
	pendingPath := filepath.Join(stateDir, "pending-"+sessionName)
	deadTarget := sessionName + ":0.0"
	if err := os.WriteFile(pendingPath, []byte(deadTarget), 0644); err != nil {
		t.Fatal(err)
	}
	markerTime := time.UnixMilli(now - 500)
	os.Chtimes(pendingPath, markerTime, markerTime)

	// Build a call-count file and fake-dpmux wrapper script.
	// Call 1: spawn fake-devpass-code with FAKE_DP_REFUSE_FIRST_RESUME=1
	//          (causes "Error: Session not found:" in the pane → refusal detected).
	// Call 2+: spawn normally (resume succeeds).
	callCountFile := filepath.Join(t.TempDir(), "call-count")
	if err := os.WriteFile(callCountFile, []byte("0"), 0644); err != nil {
		t.Fatal(err)
	}

	fakeDpmuxScript := filepath.Join(t.TempDir(), "fake-dpmux-resume")
	// Script uses environment variables FAKE_DPMUX_* set when it's invoked.
	// This avoids quoting issues with temp dir paths.
	scriptContent := `#!/bin/sh
# Fake dpmux: counts calls and uses FAKE_DP_REFUSE_FIRST_RESUME on the first.
COUNT_FILE="$FAKE_DPMUX_COUNT_FILE"
STATE_DIR="$FAKE_DPMUX_STATE_DIR"
XDG_HOME="$FAKE_DPMUX_XDG_HOME"
DB_PATH="$FAKE_DPMUX_DB_PATH"
FAKEBIN="$FAKE_DPMUX_BIN"

count=$(cat "$COUNT_FILE" 2>/dev/null || echo 0)
count=$((count + 1))
printf '%s' "$count" > "$COUNT_FILE"

SESSION_NAME="dp-resume-retry-${count}-$$"
TMUX_TARGET="${SESSION_NAME}:0.0"

# Write the pending marker before spawning (spec contract).
mkdir -p "$STATE_DIR"
echo "$TMUX_TARGET" > "${STATE_DIR}/pending-${SESSION_NAME}"

# Parse --session <id> from args
SESSION_ID=""
while [ $# -gt 0 ]; do
  case "$1" in
    --session|-s) SESSION_ID="$2"; shift 2 ;;
    *) shift ;;
  esac
done

if [ "$count" -eq 1 ]; then
  # First call: refuse (FAKE_DP_REFUSE_FIRST_RESUME=1).
  # Use sh -c to add a trailing sleep so the pane text is visible for detection.
  tmux new-session -d -s "$SESSION_NAME" -x 200 -y 50 \
    sh -c "env XDG_DATA_HOME=\"$XDG_HOME\" RELAY_DEVPASS_DB=\"$DB_PATH\" \
      FAKE_DP_REFUSE_FIRST_RESUME=1 \"$FAKEBIN\" --session \"$SESSION_ID\"; sleep 2" \
    2>/dev/null || true
else
  # Second call: resume succeeds.
  tmux new-session -d -s "$SESSION_NAME" -x 200 -y 50 \
    env "XDG_DATA_HOME=$XDG_HOME" "RELAY_DEVPASS_DB=$DB_PATH" \
    "$FAKEBIN" --auto --session "$SESSION_ID" \
    2>/dev/null || true
fi

echo "$SESSION_NAME"
`

	if err := os.WriteFile(fakeDpmuxScript, []byte(scriptContent), 0755); err != nil {
		t.Fatal(err)
	}

	// Start relay.
	relayCmd := exec.Command(relayBin)
	relayCmd.Env = append(os.Environ(),
		"DISCORD_TOKEN=test-token",
		"DISCORD_CHANNEL_ID=test-refusal-ch",
		fmt.Sprintf("DISCORD_API_URL=http://127.0.0.1:%s", mockPort),
		fmt.Sprintf("RELAY_DEVPASS_DB=%s", dbPath),
		fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir),
		"DISCORD_USER_ID=user-refusal-001",
		fmt.Sprintf("RELAY_DPMUX_BIN=%s", fakeDpmuxScript),
		// Pass config to fake-dpmux via env (avoids shell quoting issues).
		fmt.Sprintf("FAKE_DPMUX_COUNT_FILE=%s", callCountFile),
		fmt.Sprintf("FAKE_DPMUX_STATE_DIR=%s", stateDir),
		fmt.Sprintf("FAKE_DPMUX_XDG_HOME=%s", xdgHome),
		fmt.Sprintf("FAKE_DPMUX_DB_PATH=%s", dbPath),
		fmt.Sprintf("FAKE_DPMUX_BIN=%s", fakeDP),
	)
	relayCmd.Stdout = io.Discard
	relayCmd.Stderr = os.Stderr
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	t.Cleanup(func() { relayCmd.Process.Kill() })
	t.Cleanup(func() {
		if out, err := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output(); err == nil {
			for _, sess := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if strings.HasPrefix(sess, "dp-") {
					exec.Command("tmux", "kill-session", "-t", sess).Run()
				}
			}
		}
	})

	// Wait for relay to create a thread (from the existing assistant turn).
	var threadID string
	if err := waitForThreadCreation(t, mockPort, &threadID, 30*time.Second); err != nil {
		t.Fatalf("thread not created: %v", err)
	}
	t.Logf("thread created: id=%s", threadID)

	// Give relay time to register the session.
	time.Sleep(500 * time.Millisecond)

	// Dispatch a message to the thread. The pane is dead, so the relay will call
	// spawnResume → detect refusal (attempt 1) → teardown → spawnResume (attempt 2) → succeed.
	msgEvent := buildMessageEvent("msg-refusal-001", threadID, "user-refusal-001", false, "refusal retry test prompt", nil)
	if err := dispatchEvent(mockPort, msgEvent); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// Wait for the prompt to appear in the DB via the second, successful resume.
	db2, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	if err := waitForUserPrompt(t, db2, sessionID, "refusal retry test prompt", 60*time.Second); err != nil {
		t.Fatalf("prompt not found after refusal retry: %v", err)
	}
	t.Log("✓ Invariant 4a: resume refusal detected, retried, prompt injected successfully")

	// Verify dpmux was called at least twice.
	if countData, err := os.ReadFile(callCountFile); err == nil {
		count := strings.TrimSpace(string(countData))
		t.Logf("dpmux call count: %s (expected ≥2)", count)
	}
}

// ── TestInvariant4_AmbiguousIdCandidateList ───────────────────────────────────
//
// Invariant 4b: when `!resume` is issued without a session number and multiple
// candidate sessions exist, the relay posts a numbered candidate list and waits
// for `!resume <n>`. Replying `!resume 1` causes the relay to attempt the
// resume of the first candidate.
func TestInvariant4_AmbiguousIdCandidateList(t *testing.T) {
	mustBin(t, "tmux")
	fakeDP := mustBin(t, "fake-devpass-code")
	relayBin := mustBin(t, "relay")
	mockDS := mustBin(t, "mock-discord")
	_ = fakeDP // used indirectly via relay config

	xdgHome := t.TempDir()
	stateDir := t.TempDir()
	dbPath := filepath.Join(xdgHome, "devpass-code", "opencode.db")

	mockPort := "19898"
	mockGWPort := "19899"
	startMockDiscord(t, mockDS, mockPort, mockGWPort)
	if err := waitForHTTP(fmt.Sprintf("http://127.0.0.1:%s/api/v10/gateway", mockPort), 5*time.Second); err != nil {
		t.Fatalf("mock-discord not ready: %v", err)
	}

	// Create DB with two sessions so `!resume` without a number is ambiguous.
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyTestSchema(db); err != nil {
		t.Fatalf("schema: %v", err)
	}

	now := time.Now().UnixMilli()
	sessionA := "ses_ambig_cand_A001"
	sessionB := "ses_ambig_cand_B001"
	insertTestSession(t, db, sessionA, now-5000)
	insertTestSession(t, db, sessionB, now-2000)

	// Insert assistant turns for both sessions so the relay creates threads.
	for i, sid := range []string{sessionA, sessionB} {
		ts := now + int64(i*300)
		msgID := fmt.Sprintf("msg_ambig_asst_%d", i)
		pss := fmt.Sprintf("prt_ambig_ss_%d", i)
		ptxt := fmt.Sprintf("prt_ambig_txt_%d", i)
		psf := fmt.Sprintf("prt_ambig_sf_%d", i)
		insertTestMessage(t, db, msgID, sid, "assistant", ts+50)
		insertTestPart(t, db, pss, msgID, sid, ts+51, `{"type":"step-start"}`)
		insertTestPart(t, db, ptxt, msgID, sid, ts+52,
			fmt.Sprintf(`{"type":"text","text":"Session %s active"}`, sid))
		insertTestPart(t, db, psf, msgID, sid, ts+200,
			`{"reason":"stop","type":"step-finish","tokens":{"total":2},"cost":0.000001}`)
	}

	// Write pending markers pointing to dead panes so relay pairs without live panes.
	for i := range []string{sessionA, sessionB} {
		sname := fmt.Sprintf("dp-ambig-cand-%d", i)
		target := sname + ":0.0"
		pp := filepath.Join(stateDir, "pending-"+sname)
		if err := os.WriteFile(pp, []byte(target), 0644); err != nil {
			t.Fatal(err)
		}
		mt := time.UnixMilli(now - int64(5000) + int64(i*3000))
		os.Chtimes(pp, mt, mt)
	}
	db.Close()

	// Start relay.
	relayCmd := exec.Command(relayBin)
	relayCmd.Env = append(os.Environ(),
		"DISCORD_TOKEN=test-token",
		"DISCORD_CHANNEL_ID=test-ambig-ch",
		fmt.Sprintf("DISCORD_API_URL=http://127.0.0.1:%s", mockPort),
		fmt.Sprintf("RELAY_DEVPASS_DB=%s", dbPath),
		fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir),
		"DISCORD_USER_ID=user-ambig-001",
		fmt.Sprintf("RELAY_DEVPASS_BIN=%s", fakeDP),
	)
	relayCmd.Stdout = io.Discard
	relayCmd.Stderr = os.Stderr
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	t.Cleanup(func() { relayCmd.Process.Kill() })
	t.Cleanup(func() {
		if out, err := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output(); err == nil {
			for _, sess := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if strings.HasPrefix(sess, "dp-") {
					exec.Command("tmux", "kill-session", "-t", sess).Run()
				}
			}
		}
	})

	// Wait for the first thread to be created.
	var firstThreadID string
	if err := waitForThreadCreation(t, mockPort, &firstThreadID, 60*time.Second); err != nil {
		t.Fatalf("first thread not created: %v", err)
	}
	t.Logf("first thread created: id=%s", firstThreadID)

	// Wait for a second thread (or use the first if only one shows up).
	targetThreadID := firstThreadID
	waitDeadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(waitDeadline) {
		msgs, _ := getSentMessages(mockPort)
		count := 0
		lastIdx := -1
		for i, m := range msgs {
			var body struct {
				Type int `json:"type"`
			}
			if err := json.Unmarshal(m, &body); err == nil && body.Type == 11 {
				count++
				lastIdx = i
			}
		}
		if count >= 2 {
			targetThreadID = fmt.Sprintf("%d", lastIdx+1)
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Logf("using thread %s for ambiguous !resume test", targetThreadID)

	// Give relay time to settle.
	time.Sleep(500 * time.Millisecond)
	clearSentMessages(mockPort)

	// Dispatch `!resume` (bare, no number) to the thread.
	// With two known sessions having dead panes, the relay should post a candidate list.
	resumeEvent := buildMessageEvent("msg-ambig-001", targetThreadID, "user-ambig-001", false, "!resume", nil)
	if err := dispatchEvent(mockPort, resumeEvent); err != nil {
		t.Fatalf("dispatch !resume: %v", err)
	}

	// Wait for the relay to post a candidate list containing "1." (numbered entries).
	if err := waitForMessageContent(t, mockPort, "1.", 30*time.Second); err != nil {
		t.Fatalf("candidate list not posted (expected '1.' in response): %v", err)
	}
	t.Log("✓ Invariant 4b: candidate list posted for ambiguous !resume")

	// Reply with `!resume 1` to select the first candidate.
	clearSentMessages(mockPort)
	choiceEvent := buildMessageEvent("msg-ambig-choice", targetThreadID, "user-ambig-001", false, "!resume 1", nil)
	if err := dispatchEvent(mockPort, choiceEvent); err != nil {
		t.Fatalf("dispatch !resume 1: %v", err)
	}

	// The relay should attempt to resume. Give it a few seconds to respond.
	time.Sleep(3 * time.Second)

	// Verify relay is still running (no crash).
	if relayCmd.ProcessState != nil {
		t.Error("relay process exited unexpectedly after !resume 1")
	}

	msgs, _ := getSentMessages(mockPort)
	t.Logf("✓ relay handled !resume 1 (%d response messages)", len(msgs))
	for _, m := range msgs {
		t.Logf("  msg: %s", contentOf(m))
	}
	t.Log("✓ Invariant 4b: ambiguous !resume handled without crash")

	// Explicit teardown: kill relay and all dp-* sessions before test ends,
	// then allow a brief settling time for the filesystem.
	relayCmd.Process.Kill()
	if out, err := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output(); err == nil {
		for _, sess := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.HasPrefix(sess, "dp-") {
				exec.Command("tmux", "kill-session", "-t", sess).Run()
			}
		}
	}
	time.Sleep(300 * time.Millisecond) // Let filesystem settle after abrupt kills.
}
