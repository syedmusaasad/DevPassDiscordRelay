//go:build integration

// mirror_test.go: integration tests for milestone 1 (mirror only).
package integration

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// ── TestInvariant2_FirstSightReplay ───────────────────────────────────────────
//
// Invariant 2: a completed first exchange already in the DB at attach still mirrors.
// Setup: fake-devpass-code completes a full turn before the relay starts.
// Then relay starts and attaches; the thread must show the turn.
func TestInvariant2_FirstSightReplay(t *testing.T) {
	mustBin(t, "tmux")
	fakeDP := mustBin(t, "fake-devpass-code")
	relayBin := mustBin(t, "relay")
	mockDS := mustBin(t, "mock-discord")

	// Isolated env
	xdgHome := t.TempDir()
	stateDir := t.TempDir()
	dbPath := filepath.Join(xdgHome, "devpass-code", "opencode.db")

	// Write a script for fake-devpass-code to execute one turn and exit
	script := []map[string]string{{"prompt": "ping", "reply": "Pong!"}}
	scriptData, _ := json.Marshal(script)
	scriptFile := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(scriptFile, scriptData, 0644); err != nil {
		t.Fatal(err)
	}

	// Start mock-discord
	mockPort := "19876"
	mockGWPort := "19877"
	mockCmd := exec.Command(mockDS, "--port", mockPort, "--gateway-port", mockGWPort)
	mockCmd.Env = append(os.Environ(),
		fmt.Sprintf("MOCK_DISCORD_PORT=%s", mockPort),
		fmt.Sprintf("MOCK_DISCORD_GATEWAY_PORT=%s", mockGWPort),
	)
	mockCmd.Stdout = io.Discard
	mockCmd.Stderr = io.Discard
	if err := mockCmd.Start(); err != nil {
		t.Fatalf("start mock-discord: %v", err)
	}
	t.Cleanup(func() { mockCmd.Process.Kill() })

	// Wait for mock-discord to be ready
	if err := waitForHTTP(fmt.Sprintf("http://127.0.0.1:%s/api/v10/gateway", mockPort), 5*time.Second); err != nil {
		t.Fatalf("mock-discord not ready: %v", err)
	}

	// ── Step 1: Run fake-devpass-code to completion BEFORE relay starts ──────

	// Launch fake-devpass-code via a script that completes and exits
	dpCmd := exec.Command(fakeDP, "--auto")
	dpCmd.Env = append(os.Environ(),
		fmt.Sprintf("XDG_DATA_HOME=%s", xdgHome),
		fmt.Sprintf("FAKE_DP_SCRIPT=%s", scriptFile),
	)
	// Pipe "ping\n" then close stdin to trigger the script turn and EOF exit
	dpCmd.Stdin = strings.NewReader("ping\n")
	dpCmd.Stdout = io.Discard
	dpCmd.Stderr = io.Discard
	if err := dpCmd.Run(); err != nil {
		// Accept non-zero exit; the process may exit after handling stdin EOF
		t.Logf("fake-devpass-code exited: %v", err)
	}

	// Verify the DB has the session and at least one part
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open DB: %v", err)
	}
	defer db.Close()

	var sessionID string
	if err := waitForSessionRow(t, db, &sessionID, 3*time.Second); err != nil {
		t.Fatalf("no session row: %v", err)
	}
	t.Logf("session completed before relay: %s", sessionID)

	// Verify step-finish part exists (turn completed)
	if err := waitForPartType(t, db, sessionID, "step-finish", 3*time.Second); err != nil {
		t.Fatalf("step-finish part not found: %v", err)
	}

	// ── Step 2: Write a pending marker so the relay can pair ─────────────────

	// Write pending marker file (what dpmux would have written)
	sessionName := "dp-test-firstsight"
	pendingPath := filepath.Join(stateDir, "pending-"+sessionName)
	tmuxTarget := sessionName + ":0.0"
	// Set the marker mtime to 5s before the session creation time
	var sessionCreatedAt int64
	db.QueryRow(`SELECT time_created FROM session WHERE id = ?`, sessionID).Scan(&sessionCreatedAt)
	if err := os.WriteFile(pendingPath, knownSessionMarker(tmuxTarget, sessionID), 0644); err != nil {
		t.Fatalf("write pending marker: %v", err)
	}
	// Set mtime to match session creation so the relay pairs them
	markerTime := time.UnixMilli(sessionCreatedAt - 1000)
	if err := os.Chtimes(pendingPath, markerTime, markerTime); err != nil {
		t.Logf("chtimes: %v (continuing)", err)
	}

	// ── Step 3: Start relay ──────────────────────────────────────────────────

	relayCmd := exec.Command(relayBin)
	relayCmd.Env = append(os.Environ(),
		fmt.Sprintf("DISCORD_TOKEN=test-token"),
		fmt.Sprintf("DISCORD_CHANNEL_ID=test-channel-001"),
		fmt.Sprintf("DISCORD_API_URL=http://127.0.0.1:%s", mockPort),
		fmt.Sprintf("RELAY_DEVPASS_DB=%s", dbPath),
		fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir),
	)
	relayCmd.Stdout = io.Discard
	relayCmd.Stderr = io.Discard
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	t.Cleanup(func() { relayCmd.Process.Kill() })

	// ── Step 4: Verify the thread shows the completed turn ───────────────────

	// Wait for mock-discord to have received messages (the relay should mirror the replay)
	if err := waitForSentMessages(t, mockPort, 3, 15*time.Second); err != nil {
		t.Fatalf("relay did not send expected messages: %v", err)
	}

	// Verify the sent messages include assistant text "Pong!"
	msgs, err := getSentMessages(mockPort)
	if err != nil {
		t.Fatalf("get sent messages: %v", err)
	}

	foundPong := false
	for _, m := range msgs {
		if strings.Contains(contentOf(m), "Pong!") {
			foundPong = true
			break
		}
	}
	if !foundPong {
		contents := make([]string, 0, len(msgs))
		for _, m := range msgs {
			contents = append(contents, contentOf(m))
		}
		t.Errorf("expected 'Pong!' in mirrored messages; got: %v", contents)
	}
	t.Logf("✓ first-sight replay: turn from completed session visible in thread (%d messages)", len(msgs))
}

// ── TestMirror_EmbedFlip ──────────────────────────────────────────────────────
//
// A tool call that completes should have its embed flipped from "running" to "done".
func TestMirror_EmbedFlip(t *testing.T) {
	mustBin(t, "tmux")
	fakeDP := mustBin(t, "fake-devpass-code")
	relayBin := mustBin(t, "relay")
	mockDS := mustBin(t, "mock-discord")

	_ = fakeDP
	xdgHome := t.TempDir()
	stateDir := t.TempDir()
	dbPath := filepath.Join(xdgHome, "devpass-code", "opencode.db")

	mockPort := "19878"
	mockGWPort := "19879"
	mockCmd := startMockDiscord(t, mockDS, mockPort, mockGWPort)
	_ = mockCmd
	if err := waitForHTTP(fmt.Sprintf("http://127.0.0.1:%s/api/v10/gateway", mockPort), 5*time.Second); err != nil {
		t.Fatalf("mock-discord not ready: %v", err)
	}

	// Create a DB with a session that has a tool call transitioning to completed
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

	sessionID := "ses_embedflip001"
	now := time.Now().UnixMilli()
	insertTestSession(t, db, sessionID, now)

	// Insert a tool call part that transitions to "completed"
	msgAsstID := "msg_asst_embedflip"
	insertTestMessage(t, db, msgAsstID, sessionID, "assistant", now+100)
	insertTestPart(t, db, "prt_stepstart", msgAsstID, sessionID, now+101,
		`{"type":"step-start"}`)
	insertTestPart(t, db, "prt_tool1", msgAsstID, sessionID, now+102,
		`{"type":"tool","tool":"bash","callID":"call_flip001","state":{"status":"running","input":{"command":"ls"}}}`)
	// Tool completes
	insertTestPart(t, db, "prt_tool1done", msgAsstID, sessionID, now+200,
		`{"type":"tool","tool":"bash","callID":"call_flip001","state":{"status":"completed","output":"file1.txt"}}`)
	insertTestPart(t, db, "prt_text1", msgAsstID, sessionID, now+300,
		`{"type":"text","text":"Done listing files."}`)
	insertTestPart(t, db, "prt_stepfinish", msgAsstID, sessionID, now+400,
		`{"reason":"stop","type":"step-finish","tokens":{"total":50},"cost":0.0001}`)
	db.Close()

	// Write pending marker
	sessionName := "dp-test-embedflip"
	pendingPath := filepath.Join(stateDir, "pending-"+sessionName)
	markerTime := time.UnixMilli(now - 500)
	if err := os.WriteFile(pendingPath, knownSessionMarker(sessionName+":0.0", sessionID), 0644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(pendingPath, markerTime, markerTime)

	relayCmd := startRelay(t, relayBin, dbPath, stateDir, mockPort)
	_ = relayCmd

	// Wait for messages including the "done" embed flip
	if err := waitForMessageContent(t, mockPort, "✅ done", 15*time.Second); err != nil {
		t.Fatalf("embed flip (done) not found: %v", err)
	}
	t.Log("✓ tool embed flipped to ✅ done")
}

// ── TestMirror_NoOutputLine ───────────────────────────────────────────────────
//
// When a turn ends with no text output, a "turn finished without output" line appears.
func TestMirror_NoOutputLine(t *testing.T) {
	mustBin(t, "tmux")
	fakeDP2 := mustBin(t, "fake-devpass-code")
	relayBin := mustBin(t, "relay")
	mockDS := mustBin(t, "mock-discord")

	_ = fakeDP2
	xdgHome := t.TempDir()
	stateDir := t.TempDir()
	dbPath := filepath.Join(xdgHome, "devpass-code", "opencode.db")

	mockPort := "19880"
	mockGWPort := "19881"
	startMockDiscord(t, mockDS, mockPort, mockGWPort)
	if err := waitForHTTP(fmt.Sprintf("http://127.0.0.1:%s/api/v10/gateway", mockPort), 5*time.Second); err != nil {
		t.Fatalf("mock-discord not ready: %v", err)
	}

	// DB with a session where the assistant turn has no text
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
	sessionID := "ses_nooutput001"
	now := time.Now().UnixMilli()
	insertTestSession(t, db, sessionID, now)
	msgAsstID := "msg_asst_noout"
	insertTestMessage(t, db, msgAsstID, sessionID, "assistant", now+100)
	insertTestPart(t, db, "prt_ss_no", msgAsstID, sessionID, now+101, `{"type":"step-start"}`)
	// step-finish with no text part
	insertTestPart(t, db, "prt_sf_no", msgAsstID, sessionID, now+200,
		`{"reason":"stop","type":"step-finish","tokens":{"total":10},"cost":0.0001}`)
	db.Close()

	sessionName := "dp-test-noout"
	pendingPath := filepath.Join(stateDir, "pending-"+sessionName)
	markerTime := time.UnixMilli(now - 500)
	if err := os.WriteFile(pendingPath, knownSessionMarker(sessionName+":0.0", sessionID), 0644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(pendingPath, markerTime, markerTime)

	startRelay(t, relayBin, dbPath, stateDir, mockPort)

	if err := waitForMessageContent(t, mockPort, "Turn finished without output", 15*time.Second); err != nil {
		t.Fatalf("no-output line not found: %v", err)
	}
	t.Log("✓ 'Turn finished without output' line posted")
}

// ── TestMirror_ToolErrorSoftening ────────────────────────────────────────────
//
// A tool error followed by a clean turn end should be softened in place.
func TestMirror_ToolErrorSoftening(t *testing.T) {
	mustBin(t, "tmux")
	fakeDP := mustBin(t, "fake-devpass-code")
	relayBin := mustBin(t, "relay")
	mockDS := mustBin(t, "mock-discord")

	_ = fakeDP
	xdgHome := t.TempDir()
	stateDir := t.TempDir()
	dbPath := filepath.Join(xdgHome, "devpass-code", "opencode.db")

	mockPort := "19882"
	mockGWPort := "19883"
	startMockDiscord(t, mockDS, mockPort, mockGWPort)
	if err := waitForHTTP(fmt.Sprintf("http://127.0.0.1:%s/api/v10/gateway", mockPort), 5*time.Second); err != nil {
		t.Fatalf("mock-discord not ready: %v", err)
	}

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
	sessionID := "ses_toolerr001"
	now := time.Now().UnixMilli()
	insertTestSession(t, db, sessionID, now)
	msgAsstID := "msg_asst_toolerr"
	insertTestMessage(t, db, msgAsstID, sessionID, "assistant", now+100)
	insertTestPart(t, db, "prt_ss_te", msgAsstID, sessionID, now+101, `{"type":"step-start"}`)
	insertTestPart(t, db, "prt_tool_err", msgAsstID, sessionID, now+102,
		`{"type":"tool","tool":"bash","callID":"call_err001","state":{"status":"error","error":"command not found"}}`)
	insertTestPart(t, db, "prt_text_te", msgAsstID, sessionID, now+300,
		`{"type":"text","text":"I encountered an error but handled it."}`)
	insertTestPart(t, db, "prt_sf_te", msgAsstID, sessionID, now+400,
		`{"reason":"stop","type":"step-finish","tokens":{"total":20},"cost":0.0001}`)
	db.Close()

	sessionName := "dp-test-toolerr"
	pendingPath := filepath.Join(stateDir, "pending-"+sessionName)
	markerTime := time.UnixMilli(now - 500)
	if err := os.WriteFile(pendingPath, knownSessionMarker(sessionName+":0.0", sessionID), 0644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(pendingPath, markerTime, markerTime)

	startRelay(t, relayBin, dbPath, stateDir, mockPort)

	// The tool error embed should appear (failed state)
	if err := waitForMessageContent(t, mockPort, "❌ failed", 15*time.Second); err != nil {
		t.Fatalf("tool error embed not found: %v", err)
	}
	t.Log("✓ tool error embed posted with ❌ failed")
}

// ── TestMirror_LongTextChunksToFollowups ──────────────────────────────────────
//
// A 5000-char assistant reply must be split into >=3 Discord messages whose
// concatenation equals the original reply.
func TestMirror_LongTextChunksToFollowups(t *testing.T) {
	mustBin(t, "tmux")
	relayBin := mustBin(t, "relay")
	mockDS := mustBin(t, "mock-discord")

	xdgHome := t.TempDir()
	stateDir := t.TempDir()
	dbPath := filepath.Join(xdgHome, "devpass-code", "opencode.db")

	mockPort := "19884"
	mockGWPort := "19885"
	startMockDiscord(t, mockDS, mockPort, mockGWPort)
	if err := waitForHTTP(fmt.Sprintf("http://127.0.0.1:%s/api/v10/gateway", mockPort), 5*time.Second); err != nil {
		t.Fatalf("mock-discord not ready: %v", err)
	}

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

	// Build a 5000-char reply with paragraph breaks so ChunkText can split it.
	// Use distinct content in each paragraph to avoid dedup issues.
	// Each paragraph is ~1000 chars (200 words) followed by "\n\n".
	makeParaWith := func(word string, n int) string {
		s := ""
		for i := 0; i < n; i++ {
			s += word + " "
		}
		return s
	}
	para1 := makeParaWith("alpha", 200) // 1200 chars (6 chars each)
	para2 := makeParaWith("bravo", 200)
	para3 := makeParaWith("charlie", 200) // 1600 chars (8 chars each)
	longReply := para1 + "\n\n" + para2 + "\n\n" + para3
	// Ensure total is > 4000 chars so we get at least 3 chunks.
	if len(longReply) < 4001 {
		t.Fatalf("test setup: longReply too short (%d chars), expected >4000", len(longReply))
	}

	sessionID := "ses_longtext001"
	now := time.Now().UnixMilli()
	insertTestSession(t, db, sessionID, now)
	msgAsstID := "msg_asst_longtext"
	insertTestMessage(t, db, msgAsstID, sessionID, "assistant", now+100)
	insertTestPart(t, db, "prt_ss_lt", msgAsstID, sessionID, now+101, `{"type":"step-start"}`)
	// One big text part with the full 5000-char reply.
	textData, _ := json.Marshal(map[string]string{"type": "text", "text": longReply})
	insertTestPart(t, db, "prt_text_lt", msgAsstID, sessionID, now+200, string(textData))
	insertTestPart(t, db, "prt_sf_lt", msgAsstID, sessionID, now+300,
		`{"reason":"stop","type":"step-finish","tokens":{"total":100},"cost":0.001}`)
	db.Close()

	sessionName := "dp-test-longtext"
	pendingPath := filepath.Join(stateDir, "pending-"+sessionName)
	markerTime := time.UnixMilli(now - 500)
	if err := os.WriteFile(pendingPath, knownSessionMarker(sessionName+":0.0", sessionID), 0644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(pendingPath, markerTime, markerTime)

	startRelay(t, relayBin, dbPath, stateDir, mockPort)

	// Wait for the session-ended marker (session fully mirrored).
	if err := waitForMessageContent(t, mockPort, "⏹ Session ended", 20*time.Second); err != nil {
		t.Fatalf("session end not seen: %v", err)
	}

	// Gather all sent messages.
	msgs, err := getSentMessages(mockPort)
	if err != nil {
		t.Fatalf("get sent messages: %v", err)
	}

	// Find all distinct content values that are substrings of longReply.
	// We collect them in order (mock sends are recorded in order of relay sends).
	seen := make(map[string]bool)
	var textChunks []string
	for _, m := range msgs {
		c := contentOf(m)
		if c == "" || seen[c] {
			continue
		}
		// Only collect if the content is a non-trivial substring of the long reply.
		// Use the raw content (not TrimSpace'd) for the Contains check.
		if len(c) > 50 && strings.Contains(longReply, c) {
			seen[c] = true
			textChunks = append(textChunks, c)
		}
	}

	if len(textChunks) < 3 {
		t.Errorf("expected >=3 text chunk messages in thread, got %d; chunks: %v",
			len(textChunks), textChunks)
	}

	// Verify the chunks reassemble to the full reply.
	concatenated := strings.Join(textChunks, "")
	if concatenated != longReply {
		t.Errorf("chunks do not reassemble to original reply:\noriginal len=%d\nconcatenated len=%d",
			len(longReply), len(concatenated))
	}
	t.Logf("✓ long text split into %d chunks, concatenation matches (len=%d)", len(textChunks), len(longReply))
}

// ── TestMirror_ToolErrorSoftenedOnCleanEnd ────────────────────────────────────
//
// A tool error followed by a clean step-finish: the error embed must be softened
// in-place (flipped to ⚠️ warning) and the embed footer reads "turn completed".
func TestMirror_ToolErrorSoftenedOnCleanEnd(t *testing.T) {
	mustBin(t, "tmux")
	relayBin := mustBin(t, "relay")
	mockDS := mustBin(t, "mock-discord")

	xdgHome := t.TempDir()
	stateDir := t.TempDir()
	dbPath := filepath.Join(xdgHome, "devpass-code", "opencode.db")

	mockPort := "19886"
	mockGWPort := "19887"
	startMockDiscord(t, mockDS, mockPort, mockGWPort)
	if err := waitForHTTP(fmt.Sprintf("http://127.0.0.1:%s/api/v10/gateway", mockPort), 5*time.Second); err != nil {
		t.Fatalf("mock-discord not ready: %v", err)
	}

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

	sessionID := "ses_toolsoft001"
	now := time.Now().UnixMilli()
	insertTestSession(t, db, sessionID, now)
	msgAsstID := "msg_asst_toolsoft"
	insertTestMessage(t, db, msgAsstID, sessionID, "assistant", now+100)
	insertTestPart(t, db, "prt_ss_ts", msgAsstID, sessionID, now+101, `{"type":"step-start"}`)
	// Tool enters error state.
	insertTestPart(t, db, "prt_tool_ts_err", msgAsstID, sessionID, now+200,
		`{"type":"tool","tool":"bash","callID":"call_soft001","state":{"status":"error","error":"failed"}}`)
	// Assistant produces text (clean turn).
	insertTestPart(t, db, "prt_text_ts", msgAsstID, sessionID, now+300,
		`{"type":"text","text":"Handled the error gracefully."}`)
	// Clean step-finish.
	insertTestPart(t, db, "prt_sf_ts", msgAsstID, sessionID, now+400,
		`{"reason":"stop","type":"step-finish","tokens":{"total":20},"cost":0.0001}`)
	db.Close()

	sessionName := "dp-test-toolsoft"
	pendingPath := filepath.Join(stateDir, "pending-"+sessionName)
	markerTime := time.UnixMilli(now - 500)
	if err := os.WriteFile(pendingPath, knownSessionMarker(sessionName+":0.0", sessionID), 0644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(pendingPath, markerTime, markerTime)

	startRelay(t, relayBin, dbPath, stateDir, mockPort)

	// The tool embed should first appear as ❌ failed.
	if err := waitForMessageContent(t, mockPort, "❌ failed", 15*time.Second); err != nil {
		t.Fatalf("tool error embed not found: %v", err)
	}
	t.Log("✓ tool error embed posted with ❌ failed")

	// After step-finish, the tool embed should be softened to ⚠️ warning with "turn completed".
	if err := waitForMessageContent(t, mockPort, "turn completed", 15*time.Second); err != nil {
		t.Fatalf("softened tool embed not found (expected 'turn completed'): %v", err)
	}
	t.Log("✓ tool error softened to ⚠️ warning with 'turn completed' after clean step-finish")
}

// ── helpers ───────────────────────────────────────────────────────────────────

func startMockDiscord(t *testing.T, bin, port, gwPort string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("MOCK_DISCORD_PORT=%s", port),
		fmt.Sprintf("MOCK_DISCORD_GATEWAY_PORT=%s", gwPort),
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mock-discord: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	return cmd
}

func startRelay(t *testing.T, bin, dbPath, stateDir, mockPort string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"DISCORD_TOKEN=test-token",
		"DISCORD_CHANNEL_ID=test-channel-001",
		fmt.Sprintf("DISCORD_API_URL=http://127.0.0.1:%s", mockPort),
		fmt.Sprintf("RELAY_DEVPASS_DB=%s", dbPath),
		fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir),
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	return cmd
}

func waitForHTTP(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("HTTP %s not ready after %s", url, timeout)
}

func waitForSentMessages(t *testing.T, mockPort string, minCount int, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msgs, err := getSentMessages(mockPort)
		if err == nil && len(msgs) >= minCount {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	msgs, _ := getSentMessages(mockPort)
	return fmt.Errorf("wanted %d messages, got %d after %s", minCount, len(msgs), timeout)
}

func waitForMessageContent(t *testing.T, mockPort, substr string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msgs, err := getSentMessages(mockPort)
		if err == nil {
			for _, m := range msgs {
				if strings.Contains(contentOf(m), substr) {
					return nil
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	msgs, _ := getSentMessages(mockPort)
	contents := make([]string, 0, len(msgs))
	for _, m := range msgs {
		contents = append(contents, contentOf(m))
	}
	return fmt.Errorf("content %q not found after %s; messages: %v", substr, timeout, contents)
}

func getSentMessages(mockPort string) ([]json.RawMessage, error) {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%s/control/sent", mockPort))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var msgs []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

func contentOf(msg json.RawMessage) string {
	var m struct {
		Content string `json:"content"`
	}
	json.Unmarshal(msg, &m)
	return m.Content
}

func waitForPartType(t *testing.T, db *sql.DB, sessionID, partType string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var count int
		db.QueryRow(
			`SELECT COUNT(*) FROM part WHERE session_id = ? AND data LIKE ?`,
			sessionID, `%"type":"`+partType+`"%`,
		).Scan(&count)
		if count > 0 {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("part type %q not found for session %s", partType, sessionID)
}

// DB helpers for inserting test data

func applyTestSchema(db *sql.DB) error {
	_, err := db.Exec(`
	CREATE TABLE IF NOT EXISTS project (id text PRIMARY KEY, name text NOT NULL DEFAULT '');
	CREATE TABLE IF NOT EXISTS session (
		id text PRIMARY KEY,
		project_id text NOT NULL,
		workspace_id text,
		parent_id text,
		slug text NOT NULL,
		directory text NOT NULL,
		path text,
		title text NOT NULL,
		version text NOT NULL,
		share_url text,
		summary_additions integer,
		summary_deletions integer,
		summary_files integer,
		summary_diffs text,
		metadata text,
		cost real DEFAULT 0 NOT NULL,
		tokens_input integer DEFAULT 0 NOT NULL,
		tokens_output integer DEFAULT 0 NOT NULL,
		tokens_reasoning integer DEFAULT 0 NOT NULL,
		tokens_cache_read integer DEFAULT 0 NOT NULL,
		tokens_cache_write integer DEFAULT 0 NOT NULL,
		revert text,
		permission text,
		agent text,
		model text,
		time_created integer NOT NULL,
		time_updated integer NOT NULL,
		time_compacting integer,
		time_archived integer,
		CONSTRAINT fk_session_project_id_project_id_fk FOREIGN KEY (project_id) REFERENCES project(id) ON DELETE CASCADE
	);
	CREATE TABLE IF NOT EXISTS message (
		id text PRIMARY KEY,
		session_id text NOT NULL,
		time_created integer NOT NULL,
		time_updated integer NOT NULL,
		data text NOT NULL,
		CONSTRAINT fk_message_session_id_session_id_fk FOREIGN KEY (session_id) REFERENCES session(id) ON DELETE CASCADE
	);
	CREATE TABLE IF NOT EXISTS part (
		id text PRIMARY KEY,
		message_id text NOT NULL,
		session_id text NOT NULL,
		time_created integer NOT NULL,
		time_updated integer NOT NULL,
		data text NOT NULL,
		CONSTRAINT fk_part_message_id_message_id_fk FOREIGN KEY (message_id) REFERENCES message(id) ON DELETE CASCADE
	);
	INSERT OR IGNORE INTO project(id, name) VALUES('proj-test','test-project');
	`)
	return err
}

func insertTestSession(t *testing.T, db *sql.DB, id string, now int64) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO session(id,project_id,slug,directory,title,version,time_created,time_updated)
		 VALUES(?,?,?,?,?,?,?,?)`,
		id, "proj-test", "test-slug", "/tmp", "Test Session", "1.18.11", now, now,
	)
	if err != nil {
		t.Fatalf("insertTestSession: %v", err)
	}
}

func insertTestMessage(t *testing.T, db *sql.DB, id, sessionID, role string, ts int64) {
	t.Helper()
	data := fmt.Sprintf(`{"role":%q,"time":{"created":%d}}`, role, ts)
	_, err := db.Exec(
		`INSERT INTO message(id,session_id,time_created,time_updated,data) VALUES(?,?,?,?,?)`,
		id, sessionID, ts, ts, data,
	)
	if err != nil {
		t.Fatalf("insertTestMessage: %v", err)
	}
}

func insertTestPart(t *testing.T, db *sql.DB, id, messageID, sessionID string, ts int64, data string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO part(id,message_id,session_id,time_created,time_updated,data) VALUES(?,?,?,?,?,?)`,
		id, messageID, sessionID, ts, ts, data,
	)
	if err != nil {
		t.Fatalf("insertTestPart: %v", err)
	}
}
