//go:build integration

// inject_test.go: integration tests for milestone 2 (inject thread replies into pane).
package integration

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devpass/devpass-discord-relay/internal/inject"
	_ "modernc.org/sqlite"
)

// ── TestInject_ReadinessProbe ─────────────────────────────────────────────────
//
// Verifies that the readiness probe waits for "Ask anything..." before sending
// keys, and that the prompt lands exactly once in the DB.
//
// Uses FAKE_DP_SWALLOW_UNTIL_READY=1 + FAKE_DP_READY_DELAY_MS=3000 so that any
// keys sent before the ready prompt are silently discarded.
func TestInject_ReadinessProbe(t *testing.T) {
	mustBin(t, "tmux")
	fakeDP := mustBin(t, "fake-devpass-code")
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
	sessionID := "ses_inject_rp001"
	now := time.Now().UnixMilli()
	insertTestSession(t, db, sessionID, now)
	db.Close()

	// Start fake-devpass-code in a tmux pane with ready delay and swallow knob.
	sessionName := "dp-inject-rp"
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", sessionName).Run() })

	dpCmd := exec.Command("tmux", "new-session", "-d",
		"-s", sessionName,
		"-x", "200", "-y", "50",
		"env",
		fmt.Sprintf("XDG_DATA_HOME=%s", xdgHome),
		"FAKE_DP_SWALLOW_UNTIL_READY=1",
		"FAKE_DP_READY_DELAY_MS=3000",
		fakeDP, "--auto",
	)
	if out, err := dpCmd.CombinedOutput(); err != nil {
		t.Fatalf("start fake-devpass-code tmux: %v\n%s", err, out)
	}
	paneTarget := sessionName + ":0.0"

	// Write pending marker so relay can pair this session.
	pendingPath := filepath.Join(stateDir, "pending-"+sessionName)
	if err := os.WriteFile(pendingPath, knownSessionMarker(paneTarget, sessionID), 0644); err != nil {
		t.Fatal(err)
	}
	// Marker mtime: just before session creation.
	markerTime := time.UnixMilli(now - 1000)
	os.Chtimes(pendingPath, markerTime, markerTime)

	// Start relay.
	relayCmd := exec.Command(relayBin)
	relayCmd.Env = append(os.Environ(),
		"DISCORD_TOKEN=test-token",
		"DISCORD_CHANNEL_ID=test-channel-001",
		fmt.Sprintf("DISCORD_API_URL=http://127.0.0.1:%s", mockPort),
		fmt.Sprintf("RELAY_DEVPASS_DB=%s", dbPath),
		fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir),
		"DISCORD_USER_ID=user-allowed-001",
	)
	relayCmd.Stdout = io.Discard
	relayCmd.Stderr = io.Discard
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	t.Cleanup(func() { relayCmd.Process.Kill() })

	// Wait for relay to create the thread (root message + thread POST).
	var threadID string
	if err := waitForThreadCreation(t, mockPort, &threadID, 10*time.Second); err != nil {
		t.Fatalf("thread not created: %v", err)
	}
	t.Logf("thread created: id=%s", threadID)

	// The mock dispatch endpoint queues events even before a relay WebSocket is
	// connected.  Wait for the claimed thread record instead of sleeping: a
	// MESSAGE_CREATE delivered during the gateway's READY handshake is otherwise
	// silently lost and looks like a readiness-probe failure.
	if err := waitForSessionRecord(stateDir, sessionID, 5*time.Second); err != nil {
		t.Fatalf("relay did not register claimed session before dispatch: %v", err)
	}

	// Dispatch a MESSAGE_CREATE to the thread from the allowed user.
	msgEvent := buildMessageEvent("msg-dispatch-001", threadID, "user-allowed-001", false, "hello from discord", nil)
	if err := dispatchEvent(mockPort, msgEvent); err != nil {
		t.Fatalf("dispatch event: %v", err)
	}

	// Wait for the injected prompt to appear in the DB.
	// fake-devpass-code will write a user message part after receiving the text.
	db2, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	if err := waitForUserPrompt(t, db2, sessionID, "hello from discord", 20*time.Second); err != nil {
		t.Fatalf("injected prompt not found in DB: %v", err)
	}
	t.Log("✓ readiness probe: prompt landed in DB")

	// Verify it landed exactly once.
	var count int
	db2.QueryRow(
		`SELECT COUNT(*) FROM part WHERE session_id = ? AND data LIKE ?`,
		sessionID, `%hello from discord%`,
	).Scan(&count)
	if count != 1 {
		t.Errorf("expected prompt to appear exactly once; got %d occurrences", count)
	}
	t.Logf("✓ prompt landed exactly once (%d occurrences)", count)
}

// ── TestInject_ForeignAuthorDropped ─────────────────────────────────────────
//
// Verifies that messages from authors other than DISCORD_USER_ID are ignored.
func TestInject_ForeignAuthorDropped(t *testing.T) {
	mustBin(t, "tmux")
	relayBin := mustBin(t, "relay")
	mockDS := mustBin(t, "mock-discord")
	fakeDP := mustBin(t, "fake-devpass-code")
	_ = fakeDP

	xdgHome := t.TempDir()
	stateDir := t.TempDir()
	dbPath := filepath.Join(xdgHome, "devpass-code", "opencode.db")

	mockPort := "19886"
	mockGWPort := "19887"
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
	sessionID := "ses_inject_fa001"
	now := time.Now().UnixMilli()
	insertTestSession(t, db, sessionID, now)

	// Insert an assistant turn so the relay mirrors it and creates a thread.
	msgAsstID := "msg_asst_fa001"
	insertTestMessage(t, db, msgAsstID, sessionID, "assistant", now+100)
	insertTestPart(t, db, "prt_ss_fa", msgAsstID, sessionID, now+101, `{"type":"step-start"}`)
	insertTestPart(t, db, "prt_text_fa", msgAsstID, sessionID, now+102,
		`{"type":"text","text":"I am ready"}`)
	insertTestPart(t, db, "prt_sf_fa", msgAsstID, sessionID, now+200,
		`{"reason":"stop","type":"step-finish","tokens":{"total":5},"cost":0.0001}`)
	db.Close()

	// No active tmux session — inject should never fire.
	sessionName := "dp-inject-fa"
	pendingPath := filepath.Join(stateDir, "pending-"+sessionName)
	if err := os.WriteFile(pendingPath, knownSessionMarker("nonexistent-pane:0.0", sessionID), 0644); err != nil {
		t.Fatal(err)
	}
	markerTime := time.UnixMilli(now - 1000)
	os.Chtimes(pendingPath, markerTime, markerTime)

	relayCmd := exec.Command(relayBin)
	relayCmd.Env = append(os.Environ(),
		"DISCORD_TOKEN=test-token",
		"DISCORD_CHANNEL_ID=test-channel-001",
		fmt.Sprintf("DISCORD_API_URL=http://127.0.0.1:%s", mockPort),
		fmt.Sprintf("RELAY_DEVPASS_DB=%s", dbPath),
		fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir),
		"DISCORD_USER_ID=user-allowed-001",
	)
	relayCmd.Stdout = io.Discard
	relayCmd.Stderr = io.Discard
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	t.Cleanup(func() { relayCmd.Process.Kill() })

	// Wait for the relay to create the thread.
	var threadID string
	if err := waitForThreadCreation(t, mockPort, &threadID, 10*time.Second); err != nil {
		t.Fatalf("thread not created: %v", err)
	}
	t.Logf("thread created: id=%s", threadID)

	// Wait for the mirror to settle (relay finishes mirroring the turn).
	time.Sleep(500 * time.Millisecond)

	// Clear sent log so we start from a clean baseline.
	clearSentMessages(mockPort)

	// Dispatch a message from a bot — should be dropped.
	botEvent := buildMessageEvent("msg-bot-001", threadID, "bot-user-999", true, "I am a bot", nil)
	if err := dispatchEvent(mockPort, botEvent); err != nil {
		t.Fatalf("dispatch bot event: %v", err)
	}

	// Dispatch a message from a non-allowed user — should be dropped.
	foreignEvent := buildMessageEvent("msg-foreign-001", threadID, "user-foreign-999", false, "I am an outsider", nil)
	if err := dispatchEvent(mockPort, foreignEvent); err != nil {
		t.Fatalf("dispatch foreign event: %v", err)
	}

	// Wait to ensure any (erroneous) inject activity would have settled.
	time.Sleep(300 * time.Millisecond)

	// Check the DB: no new user parts should have appeared.
	db2, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	var count int
	db2.QueryRow(
		`SELECT COUNT(*) FROM part WHERE session_id = ? AND (data LIKE ? OR data LIKE ?)`,
		sessionID, "%I am a bot%", "%I am an outsider%",
	).Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 injected parts from foreign authors; got %d", count)
	}
	t.Log("✓ foreign author dropped: no injected parts in DB")
}

// ── TestInject_AttachmentPath ────────────────────────────────────────────────
//
// Verifies that image attachments are downloaded to the staging directory with
// sanitised filenames and that the local path is correctly computed.
func TestInject_AttachmentPath(t *testing.T) {
	stateDir := t.TempDir()

	// Serve a tiny fake attachment via a test HTTP server.
	attachContent := []byte("fake-image-data")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(attachContent)
	}))
	defer ts.Close()

	inj := inject.New(stateDir)
	sessionID := "ses_attach_test_001"

	atts := []inject.Attachment{
		{Filename: "my image\t.png", URL: ts.URL + "/image.png"},
	}

	paths, err := inj.DownloadAttachments(sessionID, atts)
	if err != nil {
		t.Fatalf("DownloadAttachments: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("expected 1 path, got %d", len(paths))
	}

	// Verify file content.
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatalf("read downloaded file %s: %v", paths[0], err)
	}
	if string(data) != string(attachContent) {
		t.Errorf("downloaded content mismatch: got %q, want %q", data, attachContent)
	}

	// Verify filename is sanitised (no tab).
	base := filepath.Base(paths[0])
	if strings.Contains(base, "\t") {
		t.Errorf("filename contains tab character: %q", base)
	}

	// Verify staging directory structure.
	expectedDir := filepath.Join(stateDir, "staging", sessionID)
	if !strings.HasPrefix(paths[0], expectedDir) {
		t.Errorf("path %q should be under %q", paths[0], expectedDir)
	}
	t.Logf("✓ attachment downloaded to %s (sanitised from %q)", paths[0], atts[0].Filename)
}

// TestApproval_AutoDenyTimeout verifies a pane-only permission prompt creates
// buttons and, when unanswered, receives the pinned Reject recipe and is edited
// to record the automatic denial.
func TestApproval_AutoDenyTimeout(t *testing.T) {
	mustBin(t, "tmux")
	fakeDP := mustBin(t, "fake-devpass-code")
	relayBin := mustBin(t, "relay")
	mockDS := mustBin(t, "mock-discord")

	xdgHome := t.TempDir()
	stateDir := t.TempDir()
	dbPath := filepath.Join(xdgHome, "devpass-code", "opencode.db")
	mockPort, mockGWPort := "19920", "19921"
	startMockDiscord(t, mockDS, mockPort, mockGWPort)
	if err := waitForHTTP(fmt.Sprintf("http://127.0.0.1:%s/api/v10/gateway", mockPort), 5*time.Second); err != nil {
		t.Fatal(err)
	}

	sessionName := "dp-approval-timeout"
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", sessionName).Run() })
	cmd := exec.Command("tmux", "new-session", "-d", "-s", sessionName, "-x", "200", "-y", "50", "env",
		fmt.Sprintf("XDG_DATA_HOME=%s", xdgHome), "FAKE_DP_PERMISSION_PROMPT=1", fakeDP, "--auto")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("start fake picker: %v\n%s", err, out)
	}
	paneTarget := sessionName + ":0.0"
	if err := waitForPaneText(t, paneTarget, "Permission required", 10*time.Second); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var sessionID string
	if err := waitForSessionRow(t, db, &sessionID, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	pendingPath := filepath.Join(stateDir, "pending-"+sessionName)
	if err := os.WriteFile(pendingPath, knownSessionMarker(paneTarget, sessionID), 0644); err != nil {
		t.Fatal(err)
	}
	var created int64
	if err := db.QueryRow(`SELECT time_created FROM session WHERE id = ?`, sessionID).Scan(&created); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(pendingPath, time.UnixMilli(created-1000), time.UnixMilli(created-1000))

	relayCmd := exec.Command(relayBin)
	relayCmd.Env = append(os.Environ(), "DISCORD_TOKEN=test-token", "DISCORD_CHANNEL_ID=test-channel-001",
		fmt.Sprintf("DISCORD_API_URL=http://127.0.0.1:%s", mockPort), fmt.Sprintf("RELAY_DEVPASS_DB=%s", dbPath),
		fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir), "APPROVAL_TIMEOUT_SECONDS=1")
	relayCmd.Stdout, relayCmd.Stderr = io.Discard, io.Discard
	if err := relayCmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { relayCmd.Process.Kill() })
	if err := waitForMessageContent(t, mockPort, "Permission denied automatically", 12*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := waitForPaneText(t, paneTarget, "Permission rejected", 5*time.Second); err != nil {
		t.Fatalf("deny recipe did not resolve pane: %v", err)
	}
}

func TestInteraction_ApproveDrivesApproveRecipe(t *testing.T) {
	testInteractionApproval(t, "approve", "Permission approved", "Permission rejected")
}

func TestInteraction_DenyDrivesDenyRecipe(t *testing.T) {
	testInteractionApproval(t, "deny", "Permission rejected", "Permission approved")
}

// testInteractionApproval proves each approval action is acknowledged and
// drives its own distinct pinned recipe in the fake pane.
func testInteractionApproval(t *testing.T, action, want, absent string) {
	t.Helper()
	mustBin(t, "tmux")
	fakeDP, relayBin, mockDS := mustBin(t, "fake-devpass-code"), mustBin(t, "relay"), mustBin(t, "mock-discord")
	xdgHome, stateDir := t.TempDir(), t.TempDir()
	dbPath := filepath.Join(xdgHome, "devpass-code", "opencode.db")
	mockPort, mockGWPort := "19924", "19925"
	if action == "deny" {
		mockPort, mockGWPort = "19928", "19929"
	}
	startMockDiscord(t, mockDS, mockPort, mockGWPort)
	if err := waitForHTTP(fmt.Sprintf("http://127.0.0.1:%s/api/v10/gateway", mockPort), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	sessionName := "dp-interaction-" + action
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", sessionName).Run() })
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", sessionName, "env", "XDG_DATA_HOME="+xdgHome, "FAKE_DP_PERMISSION_PROMPT=1", fakeDP, "--auto").CombinedOutput(); err != nil {
		t.Fatalf("start fake CLI: %v: %s", err, out)
	}
	pane := sessionName + ":0.0"
	if err := waitForPaneText(t, pane, "Permission required", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var sessionID string
	if err := waitForSessionRow(t, db, &sessionID, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(stateDir, "pending-"+sessionName)
	if err := os.WriteFile(pending, knownSessionMarker(pane, sessionID), 0644); err != nil {
		t.Fatal(err)
	}
	relay := exec.Command(relayBin)
	relay.Env = append(os.Environ(), "DISCORD_TOKEN=test-token", "DISCORD_CHANNEL_ID=test-channel-001", "DISCORD_USER_ID=user-allowed-001", "DISCORD_API_URL=http://127.0.0.1:"+mockPort, "RELAY_DEVPASS_DB="+dbPath, "RELAY_STATE_DIR="+stateDir)
	relay.Stdout, relay.Stderr = io.Discard, os.Stderr
	if err := relay.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relay.Process.Kill() })
	componentID := waitForComponentID(t, mockPort, "Permission required", 10*time.Second)
	if action == "deny" {
		componentID = waitForComponentIDAt(t, mockPort, "Permission required", 1, 10*time.Second)
	}
	interaction := map[string]interface{}{"t": "INTERACTION_CREATE", "d": map[string]interface{}{"id": "interaction-" + action + "-001", "application_id": "app", "type": 3, "token": "token", "version": 1, "channel_id": "2", "member": map[string]interface{}{"user": map[string]interface{}{"id": "user-allowed-001", "bot": false}}, "data": map[string]interface{}{"custom_id": componentID, "component_type": 2}}}
	if err := dispatchEvent(mockPort, interaction); err != nil {
		t.Fatal(err)
	}
	if err := waitForPaneText(t, pane, want, 5*time.Second); err != nil {
		t.Fatalf("%s recipe did not reach pane: %v", action, err)
	}
	paneText, err := exec.Command("tmux", "capture-pane", "-t", pane, "-p").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(paneText), absent) {
		t.Fatalf("%s recipe also produced opposite pane text %q:\n%s", action, absent, paneText)
	}
	if !sentContainsCallback(mockPort) {
		t.Fatal("interaction acknowledgement callback was not sent")
	}
}

// TestApproval_SecondPromptGetsButtons verifies prompt state resets after the
// first permission is finalized, allowing the second fake CLI prompt to post.
func TestApproval_SecondPromptGetsButtons(t *testing.T) {
	mustBin(t, "tmux")
	fakeDP, relayBin, mockDS := mustBin(t, "fake-devpass-code"), mustBin(t, "relay"), mustBin(t, "mock-discord")
	xdgHome, stateDir := t.TempDir(), t.TempDir()
	dbPath := filepath.Join(xdgHome, "devpass-code", "opencode.db")
	mockPort, mockGWPort := "19926", "19927"
	startMockDiscord(t, mockDS, mockPort, mockGWPort)
	if err := waitForHTTP(fmt.Sprintf("http://127.0.0.1:%s/api/v10/gateway", mockPort), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	sessionName := "dp-second-approval"
	pane := sessionName + ":0.0"
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", sessionName).Run() })
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", sessionName, "env", "XDG_DATA_HOME="+xdgHome, "FAKE_DP_PERMISSION_PROMPTS=2", fakeDP, "--auto").CombinedOutput(); err != nil {
		t.Fatalf("start fake CLI: %v: %s", err, out)
	}
	if err := waitForPaneText(t, pane, "Permission required", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var sessionID string
	if err := waitForSessionRow(t, db, &sessionID, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "pending-"+sessionName), knownSessionMarker(pane, sessionID), 0644); err != nil {
		t.Fatal(err)
	}
	relay := exec.Command(relayBin)
	relay.Env = append(os.Environ(), "DISCORD_TOKEN=test-token", "DISCORD_CHANNEL_ID=test-channel-001", "DISCORD_USER_ID=user-allowed-001", "DISCORD_API_URL=http://127.0.0.1:"+mockPort, "RELAY_DEVPASS_DB="+dbPath, "RELAY_STATE_DIR="+stateDir)
	relay.Stdout, relay.Stderr = io.Discard, io.Discard
	if err := relay.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relay.Process.Kill() })
	first := waitForComponentID(t, mockPort, "Permission required", 10*time.Second)
	if err := dispatchEvent(mockPort, map[string]interface{}{"t": "INTERACTION_CREATE", "d": map[string]interface{}{"id": "second-first", "application_id": "app", "type": 3, "token": "token", "version": 1, "channel_id": "2", "member": map[string]interface{}{"user": map[string]interface{}{"id": "user-allowed-001"}}, "data": map[string]interface{}{"custom_id": first, "component_type": 2}}}); err != nil {
		t.Fatal(err)
	}
	if err := waitForPaneText(t, pane, "Permission approved", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	count := 0
	for time.Now().Before(deadline) {
		count = componentMessageCount(mockPort, "Permission required")
		if count >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if count != 2 {
		out, _ := exec.Command("tmux", "capture-pane", "-t", pane, "-p").Output()
		t.Fatalf("got %d approval button messages, want 2; pane:\n%s", count, out)
	}
}

func waitForComponentID(t *testing.T, port, content string, timeout time.Duration) string {
	return waitForComponentIDAt(t, port, content, 0, timeout)
}

func waitForComponentIDAt(t *testing.T, port, content string, index int, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msgs, _ := getSentMessages(port)
		for _, message := range msgs {
			var body struct {
				Content    string `json:"content"`
				Components []struct {
					Components []struct {
						CustomID string `json:"custom_id"`
					} `json:"components"`
				} `json:"components"`
			}
			if json.Unmarshal(message, &body) == nil && strings.Contains(body.Content, content) && len(body.Components) > 0 && len(body.Components[0].Components) > index {
				return body.Components[0].Components[index].CustomID
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("component message %q not posted", content)
	return ""
}
func componentMessageCount(port, content string) int {
	msgs, _ := getSentMessages(port)
	count := 0
	for _, message := range msgs {
		var body struct {
			Content    string            `json:"content"`
			Components []json.RawMessage `json:"components"`
		}
		if json.Unmarshal(message, &body) == nil && strings.Contains(body.Content, content) && len(body.Components) > 0 {
			count++
		}
	}
	return count
}
func sentContainsCallback(port string) bool {
	msgs, _ := getSentMessages(port)
	for _, message := range msgs {
		var body struct {
			Type int `json:"type"`
		}
		if json.Unmarshal(message, &body) == nil && body.Type == 6 {
			return true
		}
	}
	return false
}

// TestQuestionPicker_MenuSelection verifies the fake-only picker contract: pane
// options become a Discord select menu and a selected value drives its recipe.
func TestQuestionPicker_MenuSelection(t *testing.T) {
	mustBin(t, "tmux")
	fakeDP := mustBin(t, "fake-devpass-code")
	relayBin := mustBin(t, "relay")
	mockDS := mustBin(t, "mock-discord")
	xdgHome, stateDir := t.TempDir(), t.TempDir()
	dbPath := filepath.Join(xdgHome, "devpass-code", "opencode.db")
	mockPort, mockGWPort := "19922", "19923"
	startMockDiscord(t, mockDS, mockPort, mockGWPort)
	if err := waitForHTTP(fmt.Sprintf("http://127.0.0.1:%s/api/v10/gateway", mockPort), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	sessionName := "dp-question-picker"
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", sessionName).Run() })
	cmd := exec.Command("tmux", "new-session", "-d", "-s", sessionName, "-x", "200", "-y", "50", "env",
		fmt.Sprintf("XDG_DATA_HOME=%s", xdgHome), "FAKE_DP_QUESTION_PICKER=1", fakeDP, "--auto")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("start fake picker: %v\n%s", err, out)
	}
	paneTarget := sessionName + ":0.0"
	if err := waitForPaneText(t, paneTarget, "Choose a fruit", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var sessionID string
	if err := waitForSessionRow(t, db, &sessionID, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	pendingPath := filepath.Join(stateDir, "pending-"+sessionName)
	if err := os.WriteFile(pendingPath, knownSessionMarker(paneTarget, sessionID), 0644); err != nil {
		t.Fatal(err)
	}
	var created int64
	if err := db.QueryRow(`SELECT time_created FROM session WHERE id = ?`, sessionID).Scan(&created); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(pendingPath, time.UnixMilli(created-1000), time.UnixMilli(created-1000))
	relayCmd := exec.Command(relayBin)
	relayCmd.Env = append(os.Environ(), "DISCORD_TOKEN=test-token", "DISCORD_CHANNEL_ID=test-channel-001",
		fmt.Sprintf("DISCORD_API_URL=http://127.0.0.1:%s", mockPort), fmt.Sprintf("RELAY_DEVPASS_DB=%s", dbPath), fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir), "DISCORD_USER_ID=user-allowed-001")
	relayCmd.Stdout, relayCmd.Stderr = io.Discard, io.Discard
	if err := relayCmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { relayCmd.Process.Kill() })
	var questionID string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		messages, err := getSentMessages(mockPort)
		if err == nil {
			for _, message := range messages {
				var body struct {
					Content    string `json:"content"`
					Components []struct {
						Components []struct {
							CustomID string `json:"custom_id"`
						} `json:"components"`
					} `json:"components"`
				}
				if json.Unmarshal(message, &body) == nil && strings.Contains(body.Content, "Choose a fruit") && len(body.Components) > 0 && len(body.Components[0].Components) > 0 {
					questionID = body.Components[0].Components[0].CustomID
					break
				}
			}
		}
		if questionID != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if questionID == "" {
		t.Fatal("question select menu was not posted")
	}
	interaction := map[string]interface{}{"t": "INTERACTION_CREATE", "d": map[string]interface{}{
		"id": "interaction-question-001", "application_id": "app", "type": 3, "token": "token", "version": 1, "channel_id": "2",
		"member": map[string]interface{}{"user": map[string]interface{}{"id": "user-allowed-001", "bot": false}},
		"data":   map[string]interface{}{"custom_id": questionID, "component_type": 3, "values": []string{"1"}},
	}}
	if err := dispatchEvent(mockPort, interaction); err != nil {
		t.Fatal(err)
	}
	if err := waitForPaneText(t, paneTarget, "Question answered", 5*time.Second); err != nil {
		t.Fatalf("select recipe did not resolve picker: %v", err)
	}
}

// ── helpers specific to inject tests ─────────────────────────────────────────

// buildMessageEvent constructs a MESSAGE_CREATE gateway event map.
func buildMessageEvent(id, channelID, authorID string, isBot bool, content string, attachments []map[string]interface{}) map[string]interface{} {
	atts := []interface{}{}
	for _, a := range attachments {
		atts = append(atts, a)
	}
	return map[string]interface{}{
		"t": "MESSAGE_CREATE",
		"d": map[string]interface{}{
			"id":         id,
			"channel_id": channelID,
			"content":    content,
			"author": map[string]interface{}{
				"id":  authorID,
				"bot": isBot,
			},
			"attachments": atts,
		},
	}
}

// dispatchEvent sends a gateway event to mock-discord's /control/dispatch.
func dispatchEvent(mockPort string, ev interface{}) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%s/control/dispatch", mockPort),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("dispatch: status %d: %s", resp.StatusCode, b)
	}
	return nil
}

// clearSentMessages sends DELETE /control/sent to reset the sent log.
func clearSentMessages(mockPort string) {
	req, _ := http.NewRequest(http.MethodDelete,
		fmt.Sprintf("http://127.0.0.1:%s/control/sent", mockPort), nil)
	http.DefaultClient.Do(req) //nolint:errcheck
}

// waitForThreadCreation polls /control/sent until a CreateThread request body
// (type:11) is found. It sets threadID to the mock-assigned ID (second sequential ID).
func waitForThreadCreation(t *testing.T, mockPort string, threadID *string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msgs, err := getSentMessages(mockPort)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		// Look for a CreateThread request body: {"name":"ses_...","type":11,...}
		for i, m := range msgs {
			var body struct {
				Name string `json:"name"`
				Type int    `json:"type"`
			}
			if err := json.Unmarshal(m, &body); err != nil {
				continue
			}
			if body.Type == 11 {
				// The relay made requests in order: (1) create root message, (2) create thread.
				// mock-discord assigns sequential IDs: root=1, thread=2, ...
				// The thread ID = i+2 (1-indexed, with root being msg #1).
				// Actually: each POST to /messages or /threads gets a unique ID.
				// Root message is request index 0 → ID "1".
				// Thread creation is request index i → ID = fmt.Sprint(i+1).
				*threadID = fmt.Sprintf("%d", i+1)
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("thread creation not found in sent messages after %s", timeout)
}

// waitForUserPrompt polls the DB until a part with the given text appears for the session.
func waitForUserPrompt(t *testing.T, db *sql.DB, sessionID, text string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var count int
		db.QueryRow(
			`SELECT COUNT(*) FROM part WHERE session_id = ? AND data LIKE ?`,
			sessionID, "%"+text+"%",
		).Scan(&count)
		if count > 0 {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("user prompt %q not found in DB for session %s after %s", text, sessionID, timeout)
}

func waitForSessionRecord(stateDir, sessionID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	path := filepath.Join(stateDir, "sessions.json")
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			var records []struct {
				SessionID string `json:"session_id"`
			}
			if json.Unmarshal(data, &records) == nil {
				for _, record := range records {
					if record.SessionID == sessionID {
						return nil
					}
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("session %s not present in %s after %s", sessionID, path, timeout)
}
