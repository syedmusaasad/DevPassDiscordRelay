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
// Invariant 1: a stale session row created AFTER relay start but BEFORE the
// spawn must never be bound to the spawn, even if its id sorts lexicographically
// first among all rows created after relay start.
//
// Setup:
//  1. Create a DB (no pre-existing rows) and start the relay.
//  2. Insert a "stale" session row whose id sorts before all real session ids
//     (e.g. "ses_000_stale") and whose time_created is after relay start but
//     will be before spawnStart.  Its title is a plain string (no nonce prefix)
//     so neither scanSessions nor handleSpawnFromDiscord can match it.
//  3. Wait for the relay to see the stale row (one poll cycle).
//  4. Send a Discord channel message to the relay channel.  The relay spawns
//     dpmux, which launches fake-devpass-code and creates a NEW session row
//     with a nonce-bearing title.  FAKE_DP_FORCE_NEW_SESSION=1 prevents
//     fake-devpass-code from auto-resuming the stale row.
//  5. Verify the relay creates a Discord thread for the NEW session only.
//     The stale session id must not appear in any sent message.
//  6. Verify the original Discord message text is injected as the first prompt
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

	// ── Step 1: Create an empty DB and start the relay ────────────────────────
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
	db.Close()

	// FAKE_DP_FORCE_NEW_SESSION=1 prevents fake-devpass-code from auto-resuming
	// the stale session when launched by dpmux.  This ensures a brand-new
	// session row with a nonce-bearing title is created after spawnStart.
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

	// Allow the relay one poll cycle to start watching before we insert the stale row.
	time.Sleep(1200 * time.Millisecond)

	// ── Step 2: Insert a stale row AFTER relay start, BEFORE spawn ────────────
	//
	// The stale session has:
	//   - An id that sorts BEFORE any real session id ("ses_000..." < "ses_1...")
	//   - A plain title (no "nonce:" prefix) so the nonce-based binder cannot match it
	//   - time_created = now (which is after relay start but will be before spawnStart)
	//
	// This is the exact race the invariant test is guarding against: a process
	// (e.g. another relay instance or a background task) creating a session row
	// that, under a naive time-ordering binder, would be selected instead of the
	// real spawn session.
	db2, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	staleSessionID := "ses_000_stale_invariant1"
	staleCreatedAt := time.Now().UnixMilli()
	insertTestSession(t, db2, staleSessionID, staleCreatedAt)
	db2.Close()
	t.Logf("stale session inserted: id=%s (sorts first, no nonce, created after relay start)", staleSessionID)

	// Allow one more poll cycle so the relay sees the stale row.
	// The relay should not pair it (no matching nonce in title → no pending marker match).
	time.Sleep(1200 * time.Millisecond)

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
	t.Logf("spawn triggered at spawnStart=%d (stale row created at %d)", spawnStart, staleCreatedAt)

	// Verify staleCreatedAt < spawnStart: the stale row was created before the spawn.
	if staleCreatedAt >= spawnStart {
		t.Logf("warning: stale row created at same ms as spawnStart; test may be less precise")
	}

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
	// Find the new session: created after spawnStart, id != staleSessionID.
	db3, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db3.Close()

	var newSessionID string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		db3.QueryRow(
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
	t.Logf("new session id: %s (sorts after stale %s: %v)", newSessionID, staleSessionID, newSessionID > staleSessionID)

	if err := waitForUserPrompt(t, db3, newSessionID, "invariant1: this is the first prompt", 30*time.Second); err != nil {
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
