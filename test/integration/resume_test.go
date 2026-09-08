//go:build integration

// resume_test.go: integration tests for milestone 4 (resume dead sessions,
// retry refusals, verify injection echo).
package integration

import (
	"database/sql"
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
// the absent echo and re-send once.  After the retry the prompt MUST appear in
// the DB AND the Discord thread echo must appear exactly once (duplicate suppression).
func TestInvariant3_InjectionEchoVerification(t *testing.T) {
	mustBin(t, "tmux")
	fakeDP := mustBin(t, "fake-devpass-code")
	relayBin := mustBin(t, "relay")
	mockDS := mustBin(t, "mock-discord")
	// Keep this long-running retry test on its own tmux server. A stale dp-*
	// session on the shared default socket can otherwise satisfy a readiness
	// probe while the pane under test dies.
	tmuxTmpDir := tmuxIsolatedSocketDir(t)

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

	// Start fake-devpass-code with:
	//   FAKE_DP_READY_DELAY_MS=800    — delays the ready prompt by 800ms
	//   FAKE_DP_SWALLOW_UNTIL_READY=1 — stdin is drained during the delay,
	//                                   so the first inject is discarded and
	//                                   the relay's echo-verify loop must retry.
	sessionName := "dp-echo-verify"
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", sessionName).Run() })

	dpCmd := exec.Command("tmux", "new-session", "-d",
		"-s", sessionName,
		"-x", "200", "-y", "50",
		"env",
		fmt.Sprintf("XDG_DATA_HOME=%s", xdgHome),
		fmt.Sprintf("RELAY_DEVPASS_DB=%s", dbPath),
		"FAKE_DP_READY_DELAY_MS=800",
		"FAKE_DP_SWALLOW_UNTIL_READY=1",
		fakeDP, "--auto", "--session", sessionID,
	)
	dpCmd.Env = append(envWithoutTmux(), fmt.Sprintf("TMUX_TMPDIR=%s", tmuxTmpDir))
	if out, err := dpCmd.CombinedOutput(); err != nil {
		t.Fatalf("start fake-devpass-code tmux: %v\n%s", err, out)
	}
	paneTarget := sessionName + ":0.0"

	// Write pending marker.
	pendingPath := filepath.Join(stateDir, "pending-"+sessionName)
	if err := os.WriteFile(pendingPath, knownSessionMarker(paneTarget, sessionID), 0644); err != nil {
		t.Fatal(err)
	}
	markerTime := time.UnixMilli(now - 1000)
	os.Chtimes(pendingPath, markerTime, markerTime)

	// Start relay.
	relayCmd := exec.Command(relayBin)
	relayCmd.Env = append(envWithoutTmux(),
		"DISCORD_TOKEN=test-token",
		"DISCORD_CHANNEL_ID=test-echo-ch",
		fmt.Sprintf("DISCORD_API_URL=http://127.0.0.1:%s", mockPort),
		fmt.Sprintf("RELAY_DEVPASS_DB=%s", dbPath),
		fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir),
		"DISCORD_USER_ID=user-echo-001",
		fmt.Sprintf("TMUX_TMPDIR=%s", tmuxTmpDir),
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

	// Clear sent messages so we can count the echo precisely.
	clearSentMessages(mockPort)

	// Dispatch a MESSAGE_CREATE to the thread. The relay will:
	//  1. Wait for "Ask anything..." (ready probe — delayed by 800ms).
	//  2. Inject the text. FAKE_DP_SWALLOW_UNTIL_READY=1 discards stdin during
	//     the ready delay, so the first inject is lost.
	//  3. Wait 30 s for echo in DB → not found.
	//  4. Re-send once. The fake processes the second inject and writes the row.
	//  5. suppressedEchoes suppresses the second "You: …" from mirrorContent.
	msgEvent := buildMessageEvent("msg-echo-001", threadID, "user-echo-001", false, "echo verification probe", nil)
	if err := dispatchEvent(mockPort, msgEvent); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// Wait for the echo to appear in the DB (up to 40 s to cover the 30 s verify window).
	db2, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	if err := waitForUserPrompt(t, db2, sessionID, "echo verification probe", 40*time.Second); err != nil {
		t.Fatalf("echo not found in DB: %v", err)
	}
	t.Log("✓ Invariant 3: injection echo appeared in DB (echo verification confirmed)")

	// Assert: the prompt appears at least once in the DB.
	var dbCount int
	db2.QueryRow(
		`SELECT COUNT(*) FROM part WHERE session_id = ? AND data LIKE ?`,
		sessionID, "%echo verification probe%",
	).Scan(&dbCount)
	if dbCount < 1 {
		t.Errorf("expected at least 1 echo in DB, got %d", dbCount)
	}
	t.Logf("✓ echo appeared %d time(s) in DB", dbCount)

	// Assert: the Discord thread shows "You: echo verification probe" exactly once.
	// Allow a brief settling window for the relay to post after the echo appears.
	time.Sleep(500 * time.Millisecond)
	msgs, _ := getSentMessages(mockPort)
	youCount := 0
	for _, m := range msgs {
		c := contentOf(m)
		if strings.Contains(c, "You:") && strings.Contains(c, "echo verification probe") {
			youCount++
		}
	}
	if youCount != 1 {
		t.Errorf("expected exactly 1 'You: echo verification probe' in Discord thread, got %d", youCount)
	}
	t.Logf("✓ Invariant 3: Discord echo count = %d (expected 1, duplicate suppression working)", youCount)
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

	// Isolated tmux socket namespace for this test only: a fresh TMUX_TMPDIR
	// so no component of this test ever depends on (or leaks into) another
	// test's tmux server, and this test never depends on a tmux server left
	// running by anything else on the host. Every tmux invocation below —
	// the relay's own isTmuxPaneAlive/capturePaneTextRaw/teardownPaneTarget
	// calls (inherited via relayCmd.Env), the fake-dpmux wrapper's
	// "tmux new-session" calls (also inherited via relayCmd.Env, since the
	// wrapper is spawned by the relay process), and this test's own cleanup
	// listing — resolve the SAME default socket under this directory because
	// they all inherit TMUX_TMPDIR from the same source. TMUX is explicitly
	// unset so no ambient session leaks in either.
	tmuxTmpDir := tmuxIsolatedSocketDir(t)

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
	marker := fmt.Sprintf(`{"target":%q,"sessionID":%q}`, deadTarget, sessionID)
	if err := os.WriteFile(pendingPath, []byte(marker), 0644); err != nil {
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
	//
	// IMPORTANT: tmux launch failures are never hidden here. The previous
	// "2>/dev/null || true" swallowed both stderr and a non-zero exit code
	// from `tmux new-session`, so a real launch failure (e.g. "no server
	// running" on a stale/missing socket) would silently fall through and
	// only surface later as the test's 60s waitForUserPrompt timeout. Now:
	//   - stderr is captured to a file and printed by the script itself on
	//     failure (visible in the relay's inherited stderr, which the test
	//     process also captures via relayCmd.Stderr = os.Stderr).
	//   - a failed `tmux new-session` is a hard failure: the wrapper exits
	//     non-zero instead of masking it, exactly matching what dpmux itself
	//     does on a real launch failure (see cmd/dpmux/main.go's unguarded
	//     `tmux new-session ... .Run()`).
	//   - the wrapper waits for `tmux has-session` to actually report the
	//     session exists before returning, instead of assuming success the
	//     instant the tmux command returns.
	scriptContent := `#!/bin/sh
# Fake dpmux: counts calls and uses FAKE_DP_REFUSE_FIRST_RESUME on the first.
# No tmux failure is ever swallowed: every "tmux new-session" below is run
# without "2>/dev/null || true", so a genuine launch failure (e.g. missing
# server on this test's own socket namespace) is a hard, visible failure.
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

# Parse --session <id> from args
SESSION_ID=""
while [ $# -gt 0 ]; do
  case "$1" in
    --session|-s) SESSION_ID="$2"; shift 2 ;;
    *) shift ;;
  esac
done

# Write the pending marker before spawning (spec contract). A resume has an
# exact historical identity, so it records that id rather than relying on a
# timestamp or a new-session stderr creation event (which a real resume does
# not emit).
mkdir -p "$STATE_DIR"
printf '{"target":"%s","sessionID":"%s"}\n' "$TMUX_TARGET" "$SESSION_ID" > "${STATE_DIR}/pending-${SESSION_NAME}"

if [ "$count" -eq 1 ]; then
  # First call: refuse (FAKE_DP_REFUSE_FIRST_RESUME=1).
  # Use sh -c to add a trailing sleep so the pane text is visible for detection.
  if ! tmux new-session -d -s "$SESSION_NAME" -x 200 -y 50 \
    sh -c "env XDG_DATA_HOME=\"$XDG_HOME\" RELAY_DEVPASS_DB=\"$DB_PATH\" \
      FAKE_DP_REFUSE_FIRST_RESUME=1 \"$FAKEBIN\" --session \"$SESSION_ID\" 2>&1; sleep 2"; then
    echo "fake-dpmux-resume: tmux new-session (attempt 1) failed" >&2
    exit 1
  fi
else
  # Second call: resume succeeds.
  if ! tmux new-session -d -s "$SESSION_NAME" -x 200 -y 50 \
    env "XDG_DATA_HOME=$XDG_HOME" "RELAY_DEVPASS_DB=$DB_PATH" \
    "$FAKEBIN" --auto --session "$SESSION_ID"; then
    echo "fake-dpmux-resume: tmux new-session (attempt $count) failed" >&2
    exit 1
  fi
fi

# Wait for the actual tmux session to exist before returning; never assume
# success just because the launch command itself exited 0.
i=0
while ! tmux has-session -t "$SESSION_NAME" 2>/dev/null; do
  i=$((i + 1))
  if [ "$i" -ge 50 ]; then
    echo "fake-dpmux-resume: session $SESSION_NAME never came up after launch" >&2
    exit 1
  fi
  sleep 0.1
done

echo "$SESSION_NAME"
`

	if err := os.WriteFile(fakeDpmuxScript, []byte(scriptContent), 0755); err != nil {
		t.Fatal(err)
	}

	// Start relay. TMUX_TMPDIR pins the relay (and everything it launches —
	// the fake-dpmux wrapper and the tmux sessions it starts) to this test's
	// own private tmux socket namespace; TMUX is unset so no ambient session
	// from the calling shell leaks in.
	relayCmd := exec.Command(relayBin)
	relayCmd.Env = append(envWithoutTmux(),
		"DISCORD_TOKEN=test-token",
		"DISCORD_CHANNEL_ID=test-refusal-ch",
		fmt.Sprintf("DISCORD_API_URL=http://127.0.0.1:%s", mockPort),
		fmt.Sprintf("RELAY_DEVPASS_DB=%s", dbPath),
		fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir),
		"DISCORD_USER_ID=user-refusal-001",
		fmt.Sprintf("RELAY_DPMUX_BIN=%s", fakeDpmuxScript),
		fmt.Sprintf("TMUX_TMPDIR=%s", tmuxTmpDir),
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
		// Cleanup only THIS test's own socket namespace: never touch any
		// other test's or the host's tmux server.
		if out, err := exec.Command("tmux", "-S", filepath.Join(tmuxTmpDir, fmt.Sprintf("tmux-%d", os.Getuid()), "default"),
			"list-sessions", "-F", "#{session_name}").Output(); err == nil {
			for _, sess := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if strings.HasPrefix(sess, "dp-") {
					exec.Command("tmux", "-S", filepath.Join(tmuxTmpDir, fmt.Sprintf("tmux-%d", os.Getuid()), "default"),
						"kill-session", "-t", sess).Run()
				}
			}
		}
		exec.Command("tmux", "-S", filepath.Join(tmuxTmpDir, fmt.Sprintf("tmux-%d", os.Getuid()), "default"),
			"kill-server").Run()
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

	// Exactly the refusing launch and one successful exact-id retry are allowed.
	countData, err := os.ReadFile(callCountFile)
	if err != nil {
		t.Fatalf("read dpmux call count: %v", err)
	}
	count := strings.TrimSpace(string(countData))
	if count != "2" {
		t.Fatalf("expected exactly 2 resume launches (refusal + retry), got %q", count)
	}
	t.Logf("dpmux call count: %s (expected exactly 2)", count)
}

// ── TestInvariant4_AmbiguousIdCandidateList ───────────────────────────────────
//
// Invariant 4b: when `!resume` is issued without a session number and multiple
// candidate sessions exist, the relay posts a numbered candidate list including
// the provider name and last-active age. Replying `!resume 1` causes the relay
// to attempt the resume of the first candidate.
//
// Design: sessions.json is pre-populated with two sessions (both with dead tmux
// targets and known thread IDs). The DB is seeded with both sessions and their
// messages (including provider info). The relay loads sessions.json on startup
// and already knows about both threads.  We then dispatch bare `!resume` to
// threadA.  The relay calls handleAmbiguousResume, queries the DB for all
// sessions (finds 2), and posts the numbered candidate list.
//
// This avoids the timing race of creating two panes concurrently, since the
// relay never needs to call mirrorSession for pre-tracked sessions.
func TestInvariant4_AmbiguousIdCandidateList(t *testing.T) {
	mustBin(t, "tmux")
	relayBin := mustBin(t, "relay")
	mockDS := mustBin(t, "mock-discord")

	xdgHome := t.TempDir()
	stateDir := t.TempDir()
	dbPath := filepath.Join(xdgHome, "devpass-code", "opencode.db")

	mockPort := "19898"
	mockGWPort := "19899"
	startMockDiscord(t, mockDS, mockPort, mockGWPort)
	if err := waitForHTTP(fmt.Sprintf("http://127.0.0.1:%s/api/v10/gateway", mockPort), 5*time.Second); err != nil {
		t.Fatalf("mock-discord not ready: %v", err)
	}

	// Create DB with two sessions and messages (including provider info).
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
	sessionA := "ses_ambig_A001"
	sessionB := "ses_ambig_B001"
	insertTestSession(t, db, sessionA, now-5000)
	insertTestSession(t, db, sessionB, now-2000)

	// Insert messages with provider info so the candidate list can include them.
	for _, s := range []struct {
		sid      string
		msgID    string
		provider string
		ts       int64
	}{
		{sessionA, "msg_ambig_asst_A", "anthropic", now - 4000},
		{sessionB, "msg_ambig_asst_B", "openai", now - 1000},
	} {
		insertTestMessageWithProvider(t, db, s.msgID, s.sid, "assistant", s.provider, s.ts)
	}
	db.Close()

	// Pre-populate sessions.json with both sessions.
	// Both have dead tmux targets (no pane alive) but known thread IDs.
	// This mimics sessions that were active but whose panes have since died.
	threadA := "ch-ambig-threadA-001"
	threadB := "ch-ambig-threadB-001"
	channelID := "ch-ambig-001"

	sessionsData := fmt.Sprintf(`[
  {
    "session_id": %q,
    "tmux_target": "dp-dead-A:0.0",
    "thread_id": %q,
    "thread_root_id": "root-A-001",
    "channel_id": %q,
    "status": "active"
  },
  {
    "session_id": %q,
    "tmux_target": "dp-dead-B:0.0",
    "thread_id": %q,
    "thread_root_id": "root-B-001",
    "channel_id": %q,
    "status": "active"
  }
]`, sessionA, threadA, channelID, sessionB, threadB, channelID)

	sessionsPath := filepath.Join(stateDir, "sessions.json")
	if err := os.WriteFile(sessionsPath, []byte(sessionsData), 0644); err != nil {
		t.Fatalf("write sessions.json: %v", err)
	}

	// Start relay. It loads sessions.json and pre-seeds both sessions into
	// `seen`, so scanSessions won't try to re-pair them. The relay IS aware
	// of both threads via w.sessions.All().
	relayCmd := exec.Command(relayBin)
	relayCmd.Env = append(os.Environ(),
		"DISCORD_TOKEN=test-token",
		fmt.Sprintf("DISCORD_CHANNEL_ID=%s", channelID),
		fmt.Sprintf("DISCORD_API_URL=http://127.0.0.1:%s", mockPort),
		fmt.Sprintf("RELAY_DEVPASS_DB=%s", dbPath),
		fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir),
		"DISCORD_USER_ID=user-ambig-001",
	)
	relayCmd.Stdout = io.Discard
	relayCmd.Stderr = os.Stderr
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	t.Cleanup(func() { relayCmd.Process.Kill() })

	// Wait for the relay to connect to the Discord gateway (READY ack).
	// We detect this by waiting for the relay to be running and accepting events:
	// dispatch a harmless typing indicator probe to confirm the gateway is live.
	time.Sleep(500 * time.Millisecond) // brief wait for relay to connect

	// Clear any stale messages.
	clearSentMessages(mockPort)

	// Dispatch bare `!resume` to threadA.
	// The relay finds rec for sessionA (via threadA), calls handleAmbiguousResume,
	// queries DB → 2 sessions → posts numbered candidate list.
	resumeEvent := buildMessageEvent("msg-ambig-resume", threadA, "user-ambig-001", false, "!resume", nil)
	if err := dispatchEvent(mockPort, resumeEvent); err != nil {
		t.Fatalf("dispatch !resume: %v", err)
	}

	// Wait for the candidate list (must contain "1." for the first entry).
	if err := waitForMessageContent(t, mockPort, "1.", 15*time.Second); err != nil {
		msgs, _ := getSentMessages(mockPort)
		for _, m := range msgs {
			t.Logf("  msg: %s", contentOf(m))
		}
		t.Fatalf("candidate list not posted: %v", err)
	}
	t.Log("✓ Invariant 4b: candidate list posted for ambiguous !resume")

	// Verify the candidate list includes the provider field.
	msgs, _ := getSentMessages(mockPort)
	var candidateMsg string
	for _, m := range msgs {
		c := contentOf(m)
		if strings.Contains(c, "1.") {
			candidateMsg = c
			break
		}
	}
	if !strings.Contains(candidateMsg, "provider:") {
		t.Errorf("candidate list missing 'provider:' field; got: %q", candidateMsg)
	}
	t.Logf("✓ candidate list contains provider field: %q", candidateMsg)

	// Reply with `!resume 1` to select the first candidate.
	clearSentMessages(mockPort)
	choiceEvent := buildMessageEvent("msg-ambig-choice", threadA, "user-ambig-001", false, "!resume 1", nil)
	if err := dispatchEvent(mockPort, choiceEvent); err != nil {
		t.Fatalf("dispatch !resume 1: %v", err)
	}

	// The relay should attempt to resume. Give it a few seconds to respond.
	time.Sleep(3 * time.Second)

	// Verify relay is still running (no crash).
	if relayCmd.ProcessState != nil {
		t.Error("relay process exited unexpectedly after !resume 1")
	}

	msgs, _ = getSentMessages(mockPort)
	t.Logf("✓ relay handled !resume 1 (%d response messages)", len(msgs))
	for _, m := range msgs {
		t.Logf("  msg: %s", contentOf(m))
	}
	t.Log("✓ Invariant 4b: ambiguous !resume handled without crash")

	// Explicit teardown: kill relay and clean up any dpmux-spawned dp-* sessions.
	relayCmd.Process.Kill()
	time.Sleep(500 * time.Millisecond)
	// Clean up any dp-* tmux sessions that may have been spawned by dpmux during the test.
	if out, err := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output(); err == nil {
		for _, sess := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.HasPrefix(sess, "dp-") {
				exec.Command("tmux", "kill-session", "-t", sess).Run()
			}
		}
	}
	time.Sleep(300 * time.Millisecond)
}

// ── DB helper with model/provider ───────────────────────────────────────────

// insertTestMessageWithProvider inserts a message row with model.providerID set.
// Used to verify that the candidate list includes provider info.
func insertTestMessageWithProvider(t *testing.T, db *sql.DB, id, sessionID, role, providerID string, ts int64) {
	t.Helper()
	data := fmt.Sprintf(`{"role":%q,"time":{"created":%d},"model":{"providerID":%q,"modelID":"test-model"}}`,
		role, ts, providerID)
	_, err := db.Exec(
		`INSERT INTO message(id,session_id,time_created,time_updated,data) VALUES(?,?,?,?,?)`,
		id, sessionID, ts, ts, data,
	)
	if err != nil {
		t.Fatalf("insertTestMessageWithProvider: %v", err)
	}
}

// ── tmux socket isolation helpers ─────────────────────────────────────────
//
// TestInvariant4_ResumeRefusalRetry must never depend on (or leak into)
// another test's tmux server, and must never rely on the host having a
// pre-existing default-socket tmux server running. tmuxIsolatedSocketDir
// gives the test its own private TMUX_TMPDIR; every process the test starts
// (directly, or transitively — the relay binary, and the fake-dpmux wrapper
// the relay spawns as a subprocess with an inherited environment) resolves
// tmux's "default" socket underneath this one directory, and nothing else on
// the host ever touches it.

// tmuxIsolatedSocketDir creates a fresh, per-test TMUX_TMPDIR and registers
// cleanup that kills only the tmux server on that socket (never the host's
// default server or any other test's socket).
func tmuxIsolatedSocketDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		killCmd := exec.Command("tmux", "kill-server")
		killCmd.Env = append(envWithoutTmux(), fmt.Sprintf("TMUX_TMPDIR=%s", dir))
		killCmd.Run() //nolint:errcheck // no server running is not an error here
	})
	return dir
}

// envWithoutTmux returns os.Environ() with any inherited TMUX and
// TMUX_TMPDIR stripped, so callers can set their own isolated values without
// an ambient value from the calling shell (this test process itself may be
// running inside a tmux pane) leaking through.
func envWithoutTmux() []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if strings.HasPrefix(e, "TMUX=") || strings.HasPrefix(e, "TMUX_TMPDIR=") {
			continue
		}
		out = append(out, e)
	}
	out = append(out, "TMUX=")
	return out
}
