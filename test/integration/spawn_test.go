//go:build integration

// spawn_test.go: integration tests for milestone 3 (spawn from Discord message).
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

// ── TestInvariant1_OwnTranscriptBinding ──────────────────────────────────────
//
// Invariant 1: a stale session row created after relay start but BEFORE the
// spawn must never be bound to the spawn.
//
// Setup:
//  1. Create a DB and insert a "stale" session whose time_created is set to a
//     past timestamp (before spawnStart).  Also write a pending marker for the
//     stale session with an earlier mtime so findPendingMarker's time-range
//     heuristic would accept it under a naive implementation.
//  2. Start the relay (it sees the stale session on its first scan but cannot
//     pair it because the marker name doesn't match any real dpmux session).
//  3. Send a Discord channel message to the relay channel.  The relay spawns
//     dpmux, which launches fake-devpass-code and creates a NEW session row
//     with time_created > spawnStart.  FAKE_DP_FORCE_NEW_SESSION=1 prevents
//     fake-devpass-code from auto-resuming the stale row.
//  4. Verify the relay creates a Discord thread for the NEW session only.
//     The stale session id must not appear in any sent message.
//  5. Verify the original Discord message text is injected as the first prompt
//     in the NEW session.
func TestInvariant1_OwnTranscriptBinding(t *testing.T) {
	mustBin(t, "tmux")
	mustBin(t, "dpmux")
	fakeDP := mustBin(t, "fake-devpass-code")
	relayBin := mustBin(t, "relay")
	mockDS := mustBin(t, "mock-discord")

	xdgHome := t.TempDir()
	stateDir := t.TempDir()
	dbPath := filepath.Join(xdgHome, "devpass-code", "opencode.db")

	mockPort := "19890"
	mockGWPort := "19891"
	startMockDiscord(t, mockDS, mockPort, mockGWPort)
	if err := waitForHTTP(fmt.Sprintf("http://127.0.0.1:%s/api/v10/gateway", mockPort), 5*time.Second); err != nil {
		t.Fatalf("mock-discord not ready: %v", err)
	}

	// ── Step 1: Create DB with a stale session ────────────────────────────────
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

	// The stale session has a time_created 10 seconds in the past.  We set it
	// to a definitive past value so that spawnStart (recorded just before the
	// Discord message is dispatched) will always be AFTER it.
	staleSessionID := "ses_stale_invariant1_binding"
	staleCreatedAt := time.Now().UnixMilli() - 10_000
	insertTestSession(t, db, staleSessionID, staleCreatedAt)
	db.Close()

	// Do NOT write a pending marker for the stale session.  This means
	// scanSessions will see the stale session on each poll but won't find a
	// matching marker → it treats the session as unmatched and skips it.
	//
	// The invariant under test: when the relay's spawn path (handleSpawnFromDiscord)
	// later runs, it must poll the DB for sessions with time_created >= spawnStart
	// and bind only those — never the stale session whose time_created < spawnStart.
	//
	// If the spawn path naively picked the "first unbound session" (by
	// time_created order), it would pick the stale session erroneously.

	// ── Step 2: Start the relay ───────────────────────────────────────────────
	//
	// FAKE_DP_FORCE_NEW_SESSION=1 prevents fake-devpass-code from auto-resuming
	// the stale session when launched by dpmux.  This ensures a brand-new
	// session row is created after spawnStart.
	relayCmd := exec.Command(relayBin)
	relayCmd.Env = append(os.Environ(),
		"DISCORD_TOKEN=test-token",
		"DISCORD_CHANNEL_ID=test-spawn-ch1",
		fmt.Sprintf("DISCORD_API_URL=http://127.0.0.1:%s", mockPort),
		fmt.Sprintf("RELAY_DEVPASS_DB=%s", dbPath),
		fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir),
		"DISCORD_USER_ID=user-inv1-001",
		fmt.Sprintf("RELAY_DEVPASS_BIN=%s", fakeDP),
		"FAKE_DP_FORCE_NEW_SESSION=1",
	)
	relayCmd.Stdout = io.Discard
	relayCmd.Stderr = os.Stderr
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	t.Cleanup(func() { relayCmd.Process.Kill() })

	// Allow the relay one poll cycle to see the stale session.  It will not
	// pair it because the stale marker name won't match the real session id.
	time.Sleep(1500 * time.Millisecond)

	// ── Step 3: Dispatch a channel message to trigger spawn ───────────────────
	// spawnStart is recorded BEFORE the event is dispatched so it is strictly
	// less than the new session's time_created.
	spawnStart := time.Now().UnixMilli()
	channelMsg := buildMessageEvent(
		"msg-inv1-trigger",
		"test-spawn-ch1",
		"user-inv1-001",
		false,
		"invariant1: this is the first prompt",
		nil,
	)
	if err := dispatchEvent(mockPort, channelMsg); err != nil {
		t.Fatalf("dispatch channel message: %v", err)
	}
	t.Logf("spawn triggered at spawnStart=%d", spawnStart)

	// ── Step 4: Wait for the relay to create a thread ─────────────────────────
	var threadID string
	if err := waitForThreadCreation(t, mockPort, &threadID, 60*time.Second); err != nil {
		t.Fatalf("thread not created within 60s: %v", err)
	}
	t.Logf("thread created: id=%s", threadID)

	// ── Step 5: Verify correct session was bound ──────────────────────────────
	msgs, err := getSentMessages(mockPort)
	if err != nil {
		t.Fatalf("get sent messages: %v", err)
	}

	// The stale session id must NOT appear in any Discord message.
	for _, m := range msgs {
		c := contentOf(m)
		if strings.Contains(c, staleSessionID) {
			t.Errorf("stale session %q appeared in Discord message %q; spawn path bound the wrong session", staleSessionID, c)
		}
	}

	// At least one message must reference a new session (ses_... other than stale).
	var newSessionFound bool
	for _, m := range msgs {
		c := contentOf(m)
		if strings.Contains(c, "ses_") && !strings.Contains(c, staleSessionID) {
			newSessionFound = true
			break
		}
	}
	if !newSessionFound {
		contents := make([]string, 0, len(msgs))
		for _, m := range msgs {
			contents = append(contents, contentOf(m))
		}
		t.Errorf("no new session id found in Discord messages; messages: %v", contents)
	}
	t.Log("✓ Invariant 1: stale session not bound; new session referenced in Discord")

	// ── Step 6: Verify first prompt injected ─────────────────────────────────
	// Find the new session: time_created >= spawnStart and id != staleSessionID.
	db2, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	var newSessionID string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		db2.QueryRow(
			`SELECT id FROM session WHERE id != ? AND time_created >= ? AND parent_id IS NULL ORDER BY time_created ASC LIMIT 1`,
			staleSessionID, spawnStart,
		).Scan(&newSessionID)
		if newSessionID != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if newSessionID == "" {
		t.Fatal("no new session row with time_created >= spawnStart in DB")
	}
	t.Logf("new session id: %s", newSessionID)

	if err := waitForUserPrompt(t, db2, newSessionID, "invariant1: this is the first prompt", 30*time.Second); err != nil {
		t.Fatalf("first prompt not injected into new session: %v", err)
	}
	t.Log("✓ first prompt injected into the new (non-stale) session")

	// Clean up dp-* tmux sessions.
	t.Cleanup(func() {
		if out, err := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output(); err == nil {
			for _, sess := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if strings.HasPrefix(sess, "dp-") {
					exec.Command("tmux", "kill-session", "-t", sess).Run()
				}
			}
		}
	})
}

// ── TestSpawn_ChannelMessageTriggersDpmux ──────────────────────────────────
//
// Verifies end-to-end that a Discord message in the relay channel (not a thread)
// causes the relay to spawn dpmux and create a Discord thread for the new session.
// Also verifies the original message text is injected as the first prompt.
func TestSpawn_ChannelMessageTriggersDpmux(t *testing.T) {
	mustBin(t, "tmux")
	mustBin(t, "dpmux")
	fakeDP := mustBin(t, "fake-devpass-code")
	relayBin := mustBin(t, "relay")
	mockDS := mustBin(t, "mock-discord")

	xdgHome := t.TempDir()
	stateDir := t.TempDir()
	dbPath := filepath.Join(xdgHome, "devpass-code", "opencode.db")

	mockPort := "19892"
	mockGWPort := "19893"
	startMockDiscord(t, mockDS, mockPort, mockGWPort)
	if err := waitForHTTP(fmt.Sprintf("http://127.0.0.1:%s/api/v10/gateway", mockPort), 5*time.Second); err != nil {
		t.Fatalf("mock-discord not ready: %v", err)
	}

	// Start relay with RELAY_DEVPASS_BIN pointing at fake-devpass-code.
	// No pre-existing sessions in DB: fake-devpass-code will create a fresh one.
	relayCmd := exec.Command(relayBin)
	relayCmd.Env = append(os.Environ(),
		"DISCORD_TOKEN=test-token",
		"DISCORD_CHANNEL_ID=test-spawn-ch2",
		fmt.Sprintf("DISCORD_API_URL=http://127.0.0.1:%s", mockPort),
		fmt.Sprintf("RELAY_DEVPASS_DB=%s", dbPath),
		fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir),
		"DISCORD_USER_ID=user-spawn-002",
		fmt.Sprintf("RELAY_DEVPASS_BIN=%s", fakeDP),
		"FAKE_DP_FORCE_NEW_SESSION=1",
	)
	relayCmd.Stdout = io.Discard
	relayCmd.Stderr = os.Stderr
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	t.Cleanup(func() { relayCmd.Process.Kill() })
	time.Sleep(300 * time.Millisecond)

	// Send a message in the main channel.
	channelMsg := buildMessageEvent(
		"msg-ch-spawn-002",
		"test-spawn-ch2",
		"user-spawn-002",
		false,
		"please do something useful for me",
		nil,
	)
	if err := dispatchEvent(mockPort, channelMsg); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// Wait for a thread to be created.
	var threadID string
	if err := waitForThreadCreation(t, mockPort, &threadID, 60*time.Second); err != nil {
		t.Fatalf("thread not created: %v", err)
	}
	t.Logf("✓ thread created: id=%s", threadID)

	// Verify the first prompt lands in the DB.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var sessionID string
	if err := waitForSessionRow(t, db, &sessionID, 10*time.Second); err != nil {
		t.Fatalf("no session row: %v", err)
	}
	t.Logf("session id: %s", sessionID)

	if err := waitForUserPrompt(t, db, sessionID, "please do something useful for me", 30*time.Second); err != nil {
		t.Fatalf("first prompt not injected: %v", err)
	}
	t.Log("✓ first prompt injected into the spawned session")

	// Clean up dp-* tmux sessions.
	t.Cleanup(func() {
		if out, err := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output(); err == nil {
			for _, sess := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if strings.HasPrefix(sess, "dp-") {
					exec.Command("tmux", "kill-session", "-t", sess).Run()
				}
			}
		}
	})
}
