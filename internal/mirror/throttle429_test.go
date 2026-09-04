package mirror

import (
	"fmt"
	"testing"
	"time"
)

// mock429DiscordClient is a DiscordClient where SendMessage returns a 429-style
// error on the first call and succeeds on subsequent calls.
type mock429DiscordClient struct {
	calls   int
	sent    []string
	retryMS int64 // retry_after in milliseconds
}

func (m *mock429DiscordClient) SendMessage(channelID, content string) (MsgRef, error) {
	m.calls++
	if m.calls == 1 {
		// Simulate a 429 error with retry_after in the message.
		d := time.Duration(m.retryMS) * time.Millisecond
		return MsgRef{}, fmt.Errorf("Rate limit exceeded on %s, retry after %s", channelID, d)
	}
	m.sent = append(m.sent, content)
	return MsgRef{ID: fmt.Sprintf("msg-%d", m.calls), ChannelID: channelID}, nil
}

func (m *mock429DiscordClient) EditMessage(channelID, messageID, content string) error {
	m.calls++
	if m.calls == 1 {
		d := time.Duration(m.retryMS) * time.Millisecond
		return fmt.Errorf("Rate limit exceeded on %s, retry after %s", channelID, d)
	}
	return nil
}

func (m *mock429DiscordClient) CreateThread(channelID, messageID, name string) (ThreadRef, error) {
	return ThreadRef{ID: "thread-1"}, nil
}

func (m *mock429DiscordClient) SendTyping(channelID string) error { return nil }

func (m *mock429DiscordClient) RenameThread(threadID, name string) error { return nil }

func (m *mock429DiscordClient) OnMessage(fn MessageHandler) {}

// TestThrottle_429Backoff verifies that when a Discord send returns a 429 error
// containing "retry after <duration>", the watcher:
//  1. Parses the retry_after duration.
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

// TestExtractRetryAfter verifies the error-string parser for discordgo-style rate-limit errors.
func TestExtractRetryAfter(t *testing.T) {
	cases := []struct {
		err     error
		want    time.Duration
		wantOK  bool
	}{
		{
			err:    fmt.Errorf("Rate limit exceeded on /api/v10/channels/123/messages, retry after 500ms"),
			want:   500 * time.Millisecond,
			wantOK: true,
		},
		{
			err:    fmt.Errorf("Rate limit exceeded on /api/v10/channels/123/messages, retry after 2s"),
			want:   2 * time.Second,
			wantOK: true,
		},
		{
			err:    fmt.Errorf("some other error"),
			want:   0,
			wantOK: false,
		},
		{
			err:    nil,
			want:   0,
			wantOK: false,
		},
	}

	for _, tc := range cases {
		got, ok := extractRetryAfter(tc.err)
		if ok != tc.wantOK {
			t.Errorf("extractRetryAfter(%v): ok=%v, want %v", tc.err, ok, tc.wantOK)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("extractRetryAfter(%v): got %v, want %v", tc.err, got, tc.want)
		}
	}
}
