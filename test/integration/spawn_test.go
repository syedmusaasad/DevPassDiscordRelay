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
	waitForRelayReady(t, stateDir)
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
			`SELECT id FROM session WHERE id != ? AND time_created >= ? AND parent_id IS NULL ORDER BY time_created DESC LIMIT 1`,
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
	waitForRelayReady(t, stateDir)
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
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		err = db.QueryRow(`SELECT session_id FROM part WHERE data LIKE ? ORDER BY time_created DESC LIMIT 1`, "%please do something useful for me%").Scan(&sessionID)
		if err == nil && sessionID != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if sessionID == "" {
		t.Fatal("no spawned prompt owner")
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

// ── TestInvariant1_ConcurrentForeignSessionRejected ──────────────────────────
//
// Invariant 1 (phase-15-fix): an unrelated, foreign top-level session row that
// appears AFTER spawn start but BEFORE the real spawn's own (delayed) row —
// including a row created in the exact same working directory — must never be
// bound. Only the session actually proven via the phase-16-verified stderr-log
// identity protocol (message=created id=ses_<ID> in the launch's own exclusive
// stderr file) is bound and receives the queued prompt.
//
// Setup:
//  1. Start the relay watching an empty DB.
//  2. Dispatch a Discord channel message to trigger a spawn. fake-devpass-code
//     is configured with FAKE_DP_TRANSCRIPT_DELAY_MS so it delays creating its
//     own session row, opening a window during which...
//  3. ...a foreign top-level session row is inserted directly into the DB
//     (simulating an unrelated concurrent process, in the same "directory"
//     column value) with a time_created between spawnStart and the real row's
//     eventual creation.
//  4. Only the real (delayed) session — proven by its own exclusive stderr log
//     — must be bound: the foreign session id must never appear in any Discord
//     message, and the first prompt must land in the real session only.
func TestInvariant1_ConcurrentForeignSessionRejected(t *testing.T) {
	mustBin(t, "tmux")
	mustBin(t, "dpmux")
	fakeDP := mustBin(t, "fake-devpass-code")
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

	// fake-devpass-code will sleep 2s before creating its own session row,
	// giving the test time to insert a foreign row first.
	relayCmd := exec.Command(relayBin)
	relayCmd.Env = append(os.Environ(),
		"DISCORD_TOKEN=test-token",
		"DISCORD_CHANNEL_ID=test-spawn-ch3",
		fmt.Sprintf("DISCORD_API_URL=http://127.0.0.1:%s", mockPort),
		fmt.Sprintf("RELAY_DEVPASS_DB=%s", dbPath),
		fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir),
		"DISCORD_USER_ID=user-inv1c-001",
		fmt.Sprintf("RELAY_DEVPASS_BIN=%s", fakeDP),
		"FAKE_DP_FORCE_NEW_SESSION=1",
		"FAKE_DP_TRANSCRIPT_DELAY_MS=2000",
	)
	relayCmd.Stdout = io.Discard
	relayCmd.Stderr = os.Stderr
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	t.Cleanup(func() { relayCmd.Process.Kill() })
	time.Sleep(300 * time.Millisecond)

	spawnStart := time.Now().UnixMilli()
	channelMsg := buildMessageEvent(
		"msg-inv1c-trigger",
		"test-spawn-ch3",
		"user-inv1c-001",
		false,
		"invariant1c: this is the first prompt",
		nil,
	)
	waitForRelayReady(t, stateDir)
	if err := dispatchEvent(mockPort, channelMsg); err != nil {
		t.Fatalf("dispatch channel message: %v", err)
	}
	t.Logf("spawn triggered at spawnStart=%d", spawnStart)

	// Give dpmux time to launch and write the pending marker, then insert the
	// foreign row BEFORE the fake's own (delayed ~2s) row appears.
	time.Sleep(600 * time.Millisecond)

	db2, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	foreignSessionID := "ses_concurrent_foreign_001"
	foreignCreatedAt := time.Now().UnixMilli()
	if foreignCreatedAt < spawnStart {
		foreignCreatedAt = spawnStart + 1
	}
	// Same "directory" value as the real spawn would use (relay's cwd) — a
	// shared directory alone must not be treated as proof of ownership.
	wd, _ := os.Getwd()
	_, err = db2.Exec(
		`INSERT INTO session(id,project_id,slug,directory,title,version,time_created,time_updated)
		 VALUES(?,?,?,?,?,?,?,?)`,
		foreignSessionID, "proj-test", "foreign-slug", wd, "Foreign Concurrent Session", "1.18.11",
		foreignCreatedAt, foreignCreatedAt,
	)
	db2.Close()
	if err != nil {
		t.Fatalf("insert foreign session: %v", err)
	}
	t.Logf("foreign session inserted: id=%s createdAt=%d (after spawnStart, before real row)", foreignSessionID, foreignCreatedAt)

	// ── Wait for the relay to create a thread ─────────────────────────────
	var threadID string
	if err := waitForThreadCreation(t, mockPort, &threadID, 60*time.Second); err != nil {
		t.Fatalf("thread not created within 60s: %v", err)
	}
	t.Logf("thread created: id=%s", threadID)

	msgs, err := getSentMessages(mockPort)
	if err != nil {
		t.Fatalf("get sent messages: %v", err)
	}

	// The foreign session id must NEVER appear in any Discord message.
	for _, m := range msgs {
		c := contentOf(m)
		if strings.Contains(c, foreignSessionID) {
			t.Errorf("foreign session %q appeared in Discord message %q; spawn path bound the wrong session", foreignSessionID, c)
		}
	}

	var boundSessionID string
	for _, m := range msgs {
		c := contentOf(m)
		if strings.Contains(c, "ses_") && !strings.Contains(c, foreignSessionID) {
			// Extract the session id token starting with ses_.
			idx := strings.Index(c, "ses_")
			rest := c[idx:]
			end := len(rest)
			for i, ch := range rest {
				if ch == '`' || ch == '\n' || ch == ' ' {
					end = i
					break
				}
			}
			boundSessionID = rest[:end]
			break
		}
	}
	if boundSessionID == "" {
		contents := make([]string, 0, len(msgs))
		for _, m := range msgs {
			contents = append(contents, contentOf(m))
		}
		t.Fatalf("no real session id found in Discord messages; messages: %v", contents)
	}
	if boundSessionID == foreignSessionID {
		t.Fatalf("relay bound the foreign session %q", foreignSessionID)
	}
	t.Logf("✓ Invariant 1c: foreign concurrent session rejected; bound session: %s", boundSessionID)

	// ── Verify the first prompt landed only in the real (bound) session ────
	db3, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db3.Close()

	if err := waitForUserPrompt(t, db3, boundSessionID, "invariant1c: this is the first prompt", 30*time.Second); err != nil {
		t.Fatalf("first prompt not injected into bound session: %v", err)
	}
	var foreignPromptCount int
	db3.QueryRow(
		`SELECT COUNT(*) FROM part WHERE session_id = ? AND data LIKE ?`,
		foreignSessionID, "%invariant1c: this is the first prompt%",
	).Scan(&foreignPromptCount)
	if foreignPromptCount > 0 {
		t.Errorf("prompt was injected into the foreign session %q; expected only the real session", foreignSessionID)
	}
	t.Log("✓ first prompt injected only into the real, verified session")

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

// ── TestInvariant1_MissingIdentityFailsClosed ────────────────────────────────
//
// Invariant 1 (phase-15-fix): when identity evidence is absent or invalid — no
// "message=created id=ses_<ID>" line ever appears in the launch's own stderr
// log (e.g. the process never gets --print-logs support, or the pane dies
// during the wait) — the relay must NEVER claim, mirror, or inject into any
// row, even when eligible foreign rows exist in the DB and the pane was
// briefly alive.
//
// Setup:
//  1. Start the relay with a short RELAY_SPAWN_BIND_TIMEOUT_MS.
//  2. Point RELAY_DEVPASS_BIN at a stub that starts (so the pane is briefly
//     alive), writes NOTHING to stderr, and then exits quickly (pane dies
//     during the wait).
//  3. Insert an eligible foreign top-level row (time_created >= spawnInstant)
//     into the DB — a naive first-row-after-spawn binder would incorrectly
//     bind it.
//  4. Assert: no Discord thread/message is ever created referencing any
//     session, and no text is ever injected into any pane.
func TestInvariant1_MissingIdentityFailsClosed(t *testing.T) {
	mustBin(t, "tmux")
	mustBin(t, "dpmux")
	relayBin := mustBin(t, "relay")
	mockDS := mustBin(t, "mock-discord")

	xdgHome := t.TempDir()
	stateDir := t.TempDir()
	dbPath := filepath.Join(xdgHome, "devpass-code", "opencode.db")

	mockPort := "19900"
	mockGWPort := "19901"
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
	db.Close()

	// Stub "devpass-code": starts (pane briefly alive), never logs anything to
	// stderr, then exits immediately — simulating both "no identity evidence"
	// and "pane dies during the wait".
	stubScript := filepath.Join(t.TempDir(), "stub-no-identity")
	stubContent := "#!/bin/sh\n# No stderr identity evidence is ever written. Exit quickly.\nsleep 0.3\nexit 0\n"
	if err := os.WriteFile(stubScript, []byte(stubContent), 0755); err != nil {
		t.Fatal(err)
	}

	relayCmd := exec.Command(relayBin)
	relayCmd.Env = append(os.Environ(),
		"DISCORD_TOKEN=test-token",
		"DISCORD_CHANNEL_ID=test-spawn-ch4",
		fmt.Sprintf("DISCORD_API_URL=http://127.0.0.1:%s", mockPort),
		fmt.Sprintf("RELAY_DEVPASS_DB=%s", dbPath),
		fmt.Sprintf("RELAY_STATE_DIR=%s", stateDir),
		"DISCORD_USER_ID=user-inv1m-001",
		fmt.Sprintf("RELAY_DEVPASS_BIN=%s", stubScript),
		"RELAY_SPAWN_BIND_TIMEOUT_MS=3000",
	)
	relayCmd.Stdout = io.Discard
	relayCmd.Stderr = os.Stderr
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	t.Cleanup(func() { relayCmd.Process.Kill() })
	time.Sleep(300 * time.Millisecond)

	spawnStart := time.Now().UnixMilli()
	channelMsg := buildMessageEvent(
		"msg-inv1m-trigger",
		"test-spawn-ch4",
		"user-inv1m-001",
		false,
		"invariant1m: should never be injected",
		nil,
	)
	waitForRelayReady(t, stateDir)
	if err := dispatchEvent(mockPort, channelMsg); err != nil {
		t.Fatalf("dispatch channel message: %v", err)
	}

	// While the bind wait is in progress, insert an eligible foreign row that a
	// naive first-row-after-spawn binder would have accepted.
	time.Sleep(500 * time.Millisecond)
	db2, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	eligibleForeignID := "ses_eligible_but_unproven_001"
	foreignCreatedAt := spawnStart + 100
	_, err = db2.Exec(
		`INSERT INTO session(id,project_id,slug,directory,title,version,time_created,time_updated)
		 VALUES(?,?,?,?,?,?,?,?)`,
		eligibleForeignID, "proj-test", "elig-slug", "/tmp", "Eligible Unproven", "1.18.11",
		foreignCreatedAt, foreignCreatedAt,
	)
	db2.Close()
	if err != nil {
		t.Fatalf("insert eligible foreign session: %v", err)
	}
	t.Logf("eligible-but-unproven foreign session inserted: id=%s createdAt=%d", eligibleForeignID, foreignCreatedAt)

	// Wait past the bind timeout to give handleSpawnFromDiscord a chance to
	// (incorrectly) bind, then assert it never did.
	time.Sleep(5 * time.Second)

	msgs, err := getSentMessages(mockPort)
	if err != nil {
		t.Fatalf("get sent messages: %v", err)
	}
	if len(msgs) != 0 {
		contents := make([]string, 0, len(msgs))
		for _, m := range msgs {
			contents = append(contents, contentOf(m))
		}
		t.Fatalf("expected no Discord messages/threads when identity evidence is missing, got %d: %v", len(msgs), contents)
	}
	t.Log("✓ Invariant 1: missing identity evidence → no thread/message ever created")

	// The queued prompt must never have been injected anywhere, including into
	// the eligible-but-unproven foreign session.
	db3, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db3.Close()
	var injectedCount int
	db3.QueryRow(
		`SELECT COUNT(*) FROM part WHERE data LIKE ?`,
		"%invariant1m: should never be injected%",
	).Scan(&injectedCount)
	if injectedCount > 0 {
		t.Errorf("prompt text was injected somewhere despite missing identity evidence (count=%d)", injectedCount)
	}
	t.Log("✓ no injection occurred without verified identity")

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
