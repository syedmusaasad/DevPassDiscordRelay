//go:build integration

// discord429_test.go: regression test for the phase-15 finding that the real
// discordgo.Session, constructed with default options, sleeps-and-retries a
// live HTTP 429 internally (ShouldRetryOnRateLimit defaults to true), which
// means the relay's own bounded, context-cancellable throttle/retry code
// (discordSend/discordEdit in internal/mirror) never sees the error.
//
// TestDiscord429_ProductionClientBackoff uses the REAL production client
// constructor (internal/discord.New, the same constructor cmd/relay/main.go
// wraps) against a local mock-discord REST server that returns one genuine
// HTTP 429 (JSON body + numeric Retry-After) followed by 200, and asserts:
//  1. The error surfaces to the relay's mirror.Watcher throttle path as
//     *discordgo.RateLimitError (not silently retried inside discordgo).
//  2. The retry actually waits at least RetryAfter.
//  3. Exactly one retry occurs — request count at the mock server is bounded
//     (429 + 1 retry), never an internal discordgo retry storm.
//  4. No duplicate message is sent (SendMessage is called exactly twice:
//     the failed attempt and the one successful retry).
package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/devpass/devpass-discord-relay/internal/discord"
	"github.com/devpass/devpass-discord-relay/internal/mirror"
)

// mirrorDiscordAdapter adapts *discord.Client (the real production client
// wrapper, internal/discord.New) to mirror.DiscordClient. This is the same
// shape of adapter cmd/relay/main.go's discordAdapter provides for its own
// discordgo.Session; here it wraps the internal/discord.Client type
// explicitly so the test exercises internal/discord.New(), not a hand-rolled
// stub.
type mirrorDiscordAdapter struct {
	c *discord.Client
}

func (a *mirrorDiscordAdapter) SendMessage(channelID, content string) (mirror.MsgRef, error) {
	msg, err := a.c.SendMessage(channelID, content)
	if err != nil {
		return mirror.MsgRef{}, err
	}
	return mirror.MsgRef{ID: msg.ID, ChannelID: msg.ChannelID}, nil
}

func (a *mirrorDiscordAdapter) EditMessage(channelID, messageID, content string) error {
	_, err := a.c.EditMessage(channelID, messageID, content)
	return err
}

func (a *mirrorDiscordAdapter) CreateThread(channelID, messageID, name string) (mirror.ThreadRef, error) {
	ch, err := a.c.CreateThread(channelID, messageID, name)
	if err != nil {
		return mirror.ThreadRef{}, err
	}
	return mirror.ThreadRef{ID: ch.ID}, nil
}

func (a *mirrorDiscordAdapter) SendTyping(channelID string) error {
	return a.c.SendTyping(channelID)
}

func (a *mirrorDiscordAdapter) RenameThread(threadID, name string) error {
	_, err := a.c.Session.ChannelEdit(threadID, &discordgo.ChannelEdit{Name: name})
	return err
}

func (a *mirrorDiscordAdapter) OnMessage(fn mirror.MessageHandler) {}

// TestDiscord429_ProductionClientBackoff is the phase-15 regression for
// finding 3: it must use the REAL production client constructor
// (discord.New), not a custom error stub, and must prove the request that
// hits discordgo actually gets a real 429 HTTP response from a mock REST
// server before succeeding.
func TestDiscord429_ProductionClientBackoff(t *testing.T) {
	mockDS := mustBin(t, "mock-discord")

	mockPort := "19910"
	mockGWPort := "19911"
	startMockDiscord(t, mockDS, mockPort, mockGWPort)
	if err := waitForHTTP(fmt.Sprintf("http://127.0.0.1:%s/api/v10/gateway", mockPort), 5*time.Second); err != nil {
		t.Fatalf("mock-discord not ready: %v", err)
	}

	channelID := "ch-429-backoff-001"

	// Construct the REAL production Discord client via the same constructor
	// cmd/relay/main.go's adapter wraps: internal/discord.New(). This is the
	// exact client type/config the running relay binary builds, not a test
	// double, satisfying "no custom error stub alone is sufficient".
	client, err := discord.New(discord.Config{
		Token:  "test-token",
		APIURL: fmt.Sprintf("http://127.0.0.1:%s", mockPort),
	})
	if err != nil {
		t.Fatalf("discord.New: %v", err)
	}
	if client.Session.ShouldRetryOnRateLimit {
		t.Fatal("expected discord.New's production client to have ShouldRetryOnRateLimit=false; " +
			"a live 429 would be slept-and-retried inside discordgo instead of surfacing to the relay")
	}

	// The exact REST path discordgo will hit for ChannelMessageSend, using
	// discordgo's OWN APIVersion (whatever discord.New derived EndpointAPI
	// from), so this test tracks the real client's routing rather than
	// hard-coding an API version that could drift.
	msgPath := fmt.Sprintf("/api/v%s/channels/%s/messages", discordgo.APIVersion, channelID)

	// Configure mock-discord to return exactly ONE real HTTP 429 (with a
	// fractional-second Retry-After, to prove fractional RetryAfter survives
	// the round trip) for this path, then clear itself automatically so the
	// retry gets a real 200.
	const retryAfterSeconds = 0.6 // 600ms; fractional on purpose
	if err := setMockRateLimit(mockPort, msgPath, retryAfterSeconds, 1); err != nil {
		t.Fatalf("configure mock 429: %v", err)
	}
	t.Cleanup(func() { setMockRateLimit(mockPort, msgPath, 0, 0) })

	// Build a minimal Watcher around the REAL client via the adapter, exactly
	// as cmd/relay/main.go does, so discordSend (the relay's bounded,
	// context-cancellable throttle/retry path) is what actually gets
	// exercised — not a hand-rolled call to client.SendMessage directly.
	w, err := mirror.NewWatcherForTest(&mirrorDiscordAdapter{c: client})
	if err != nil {
		t.Fatalf("NewWatcherForTest: %v", err)
	}

	reqCountBefore, err := getMockReqCount(mockPort, msgPath)
	if err != nil {
		t.Fatalf("reqcount before: %v", err)
	}

	start := time.Now()
	msg, err := w.DiscordSendForTest(channelID, "hello after 429")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("expected discordSend to succeed after retry, got error: %v", err)
	}
	if msg.ID == "" {
		t.Error("expected a non-empty message ID after retry")
	}

	minDelay := time.Duration(retryAfterSeconds * float64(time.Second))
	if elapsed < minDelay/2 {
		// Allow slack for scheduling/clock granularity, but the retry must
		// have actually waited close to RetryAfter, not returned immediately
		// (which would mean discordgo's own retry loop raced ours, or ours
		// didn't wait at all).
		t.Errorf("expected at least ~%v elapsed for 429 backoff (RetryAfter=%.3fs), got %v",
			minDelay, retryAfterSeconds, elapsed)
	}
	t.Logf("✓ retry waited %v (RetryAfter=%.3fs)", elapsed.Round(time.Millisecond), retryAfterSeconds)

	reqCountAfter, err := getMockReqCount(mockPort, msgPath)
	if err != nil {
		t.Fatalf("reqcount after: %v", err)
	}
	gotRequests := reqCountAfter - reqCountBefore
	// Exactly 2: the first (429) request, and the one successful retry.
	// If discordgo's internal retry loop were still active (ShouldRetryOnRateLimit
	// left at its default true), or if the relay's retry loop mis-bounded and
	// looped, this count would be higher.
	if gotRequests != 2 {
		t.Errorf("expected exactly 2 REST requests to %s (1 429 + 1 retry), got %d", msgPath, gotRequests)
	}
	t.Logf("✓ bounded retry: %d total requests to %s (expected 2)", gotRequests, msgPath)

	// No duplicate message: exactly one message body among the requests to
	// this channel should have succeeded (the mock only records bodies for
	// non-429 responses), i.e. exactly one entry in /control/sent referencing
	// this content.
	msgs, err := getSentMessages(mockPort)
	if err != nil {
		t.Fatalf("get sent messages: %v", err)
	}
	dupCount := 0
	for _, m := range msgs {
		if contentOf(m) == "hello after 429" {
			dupCount++
		}
	}
	if dupCount != 1 {
		t.Errorf("expected exactly 1 successfully-recorded send of the retried content, got %d", dupCount)
	}
	t.Logf("✓ no duplicate message: %d recorded send(s) of the retried content", dupCount)
}

// ── helpers specific to the 429 backoff test ─────────────────────────────────

// setMockRateLimit configures (or clears, if retryAfter<=0) a 429 injection
// on mock-discord for the given REST path. count>0 limits how many
// consecutive requests receive the 429 before it auto-clears (see
// cmd/mock-discord's /control/ratelimit).
func setMockRateLimit(mockPort, path string, retryAfter float64, count int) error {
	body, _ := json.Marshal(map[string]interface{}{
		"path":        path,
		"retry_after": retryAfter,
		"count":       count,
	})
	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%s/control/ratelimit", mockPort),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("control/ratelimit: status %d: %s", resp.StatusCode, b)
	}
	return nil
}

// getMockReqCount returns the number of REST requests mock-discord has
// received for the given path (any status), via /control/reqcount.
func getMockReqCount(mockPort, path string) (int, error) {
	u := fmt.Sprintf("http://127.0.0.1:%s/control/reqcount?path=%s", mockPort, url.QueryEscape(path))
	resp, err := http.Get(u)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var out struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.Count, nil
}
