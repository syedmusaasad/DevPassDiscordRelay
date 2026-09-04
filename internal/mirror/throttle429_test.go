package mirror

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// rateLimitTestError is a typed 429 error for use in unit tests.
// It implements the retryAfterDurationGetter interface checked by extractRetryAfter.
type rateLimitTestError struct {
	retryAfter time.Duration
	channel    string
}

func (e *rateLimitTestError) Error() string {
	return fmt.Sprintf("Rate limit exceeded on %s, retry after %s", e.channel, e.retryAfter)
}

func (e *rateLimitTestError) RetryAfterDuration() time.Duration {
	return e.retryAfter
}

// mock429DiscordClient is a DiscordClient where SendMessage returns a typed
// *rateLimitTestError on the first call and succeeds on subsequent calls.
type mock429DiscordClient struct {
	calls   int
	sent    []string
	retryMS int64 // retry_after in milliseconds
}

func (m *mock429DiscordClient) SendMessage(channelID, content string) (MsgRef, error) {
	m.calls++
	if m.calls == 1 {
		// Return a typed 429 error so extractRetryAfter exercises the typed path.
		d := time.Duration(m.retryMS) * time.Millisecond
		return MsgRef{}, &rateLimitTestError{retryAfter: d, channel: channelID}
	}
	m.sent = append(m.sent, content)
	return MsgRef{ID: fmt.Sprintf("msg-%d", m.calls), ChannelID: channelID}, nil
}

func (m *mock429DiscordClient) EditMessage(channelID, messageID, content string) error {
	m.calls++
	if m.calls == 1 {
		d := time.Duration(m.retryMS) * time.Millisecond
		return &rateLimitTestError{retryAfter: d, channel: channelID}
	}
	return nil
}

func (m *mock429DiscordClient) CreateThread(channelID, messageID, name string) (ThreadRef, error) {
	return ThreadRef{ID: "thread-1"}, nil
}

func (m *mock429DiscordClient) SendTyping(channelID string) error { return nil }

func (m *mock429DiscordClient) RenameThread(threadID, name string) error { return nil }

func (m *mock429DiscordClient) OnMessage(fn MessageHandler) {}

// TestThrottle_429Backoff verifies that when a Discord send returns a typed 429
// *rateLimitTestError, the watcher:
//  1. Extracts the retry_after duration via the typed interface.
//  2. Feeds it into the Throttle via SetBackoff.
//  3. Sleeps for at least retry_after.
//  4. Retries the send once and succeeds.
func TestThrottle_429Backoff(t *testing.T) {
	const retryAfterMS = 100 // small value to keep the test fast

	mock := &mock429DiscordClient{retryMS: retryAfterMS}

	// Build a minimal Watcher. We don't need DB or full config for this unit test.
	w := &Watcher{
		discord:  mock,
		throttle: NewThrottle(10, 5*time.Second),
		sessions: &Sessions{path: t.TempDir() + "/sessions.json"},
	}

	channelID := "ch-test-429"
	content := "hello world"

	start := time.Now()
	w.throttle.Wait(channelID) // consume the pre-send throttle slot as discordSend would
	msg, err := w.discordSend(channelID, content)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected discordSend to succeed after retry, got: %v", err)
	}
	if msg.ID == "" {
		t.Error("expected a non-empty message ID after retry")
	}
	if mock.calls != 2 {
		t.Errorf("expected exactly 2 calls to SendMessage (1 429 + 1 retry), got %d", mock.calls)
	}
	minDelay := time.Duration(retryAfterMS) * time.Millisecond
	if elapsed < minDelay/2 {
		// Allow 50% slack for clock granularity.
		t.Errorf("expected at least %v sleep for 429 backoff, but total elapsed was only %v", minDelay, elapsed)
	}
	t.Logf("✓ 429 backoff: %d calls, elapsed=%v (retry_after=%v)", mock.calls, elapsed.Round(time.Millisecond), minDelay)
}

// TestExtractRetryAfter verifies the typed and string-fallback paths for rate-limit errors.
func TestExtractRetryAfter(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		want   time.Duration
		wantOK bool
	}{
		{
			name:   "typed rateLimitTestError",
			err:    &rateLimitTestError{retryAfter: 500 * time.Millisecond},
			want:   500 * time.Millisecond,
			wantOK: true,
		},
		{
			name:   "string fallback 500ms",
			err:    fmt.Errorf("Rate limit exceeded on /api/v10/channels/123/messages, retry after 500ms"),
			want:   500 * time.Millisecond,
			wantOK: true,
		},
		{
			name:   "string fallback 2s",
			err:    fmt.Errorf("Rate limit exceeded on /api/v10/channels/123/messages, retry after 2s"),
			want:   2 * time.Second,
			wantOK: true,
		},
		{
			name:   "unrelated error",
			err:    fmt.Errorf("some other error"),
			want:   0,
			wantOK: false,
		},
		{
			name:   "nil error",
			err:    nil,
			want:   0,
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := extractRetryAfter(tc.err)
			if ok != tc.wantOK {
				t.Errorf("extractRetryAfter(%v): ok=%v, want %v", tc.err, ok, tc.wantOK)
				return
			}
			if ok && got != tc.want {
				t.Errorf("extractRetryAfter(%v): got %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// ── Refusal detection tests ───────────────────────────────────────────────────

// testDB creates a minimal in-memory SQLite database with the session schema.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open test DB: %v", err)
	}
	_, err = db.Exec(`
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
		INSERT OR IGNORE INTO project(id, name) VALUES('proj-test','test');
	`)
	if err != nil {
		t.Fatalf("init test DB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestDetectRefusal_ForeignIDNoPaneText verifies that detectRefusal returns true
// when a new session row with a DIFFERENT id appears in the DB since the baseline,
// even when the tmux pane is completely absent (no "Error: Session not found:"
// text is available in the pane).
//
// This exercises the baseline-before-spawn contract: success requires BOTH the
// pane to be ready AND no new session row with a foreign id since the baseline.
func TestDetectRefusal_ForeignIDNoPaneText(t *testing.T) {
	db := testDB(t)

	stateDir := t.TempDir()
	sessions, err := LoadSessions(stateDir + "/sessions.json")
	if err != nil {
		t.Fatalf("load sessions: %v", err)
	}

	w := &Watcher{
		db:       db,
		sessions: sessions,
		throttle: NewThrottle(10, 5*time.Second),
		discord:  &mock429DiscordClient{},
	}

	wantSessionID := "ses_wanted_001"
	foreignSessionID := "ses_foreign_001"

	// Baseline is taken NOW (before the foreign insert simulates a spawn).
	baselineMS := time.Now().UnixMilli()

	// Simulate: after spawn, a foreign session row is created (wrong id).
	// No pane text is available (target is empty / dead).
	now := time.Now().UnixMilli()
	_, err = db.Exec(
		`INSERT INTO session(id,project_id,slug,directory,title,version,time_created,time_updated)
		 VALUES(?,?,?,?,?,?,?,?)`,
		foreignSessionID, "proj-test", "slug", "/tmp", "Foreign", "1.0", now, now,
	)
	if err != nil {
		t.Fatalf("insert foreign session: %v", err)
	}

	// detectRefusal must detect the foreign id even though the pane target is
	// nonexistent (tmux capture-pane will fail, so no "Error:" text).
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	refused, detectErr := w.detectRefusal(ctx, wantSessionID, "nonexistent-target:0.0", baselineMS)
	if detectErr != nil {
		t.Logf("detectRefusal returned error (acceptable): %v", detectErr)
	}
	if !refused {
		t.Errorf("expected detectRefusal to return true for foreign session id (got false)")
	} else {
		t.Logf("✓ detectRefusal correctly identified foreign id as refusal (no pane text needed)")
	}
}
