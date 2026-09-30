package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSendShapeAndDailyOnce(t *testing.T) {
	t.Setenv("USER", "telemetry-user")
	var calls atomic.Int32
	var got string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer s.Close()
	dir := t.TempDir()
	Send(dir, "v1", s.URL)
	deadline := time.Now().Add(time.Second)
	for calls.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatal("telemetry did not post")
	}
	digest := sha256.Sum256([]byte("telemetry-user"))
	want := hex.EncodeToString(digest[:])[:12]
	if got != `{"username":"`+want+`","version":"v1"}` {
		t.Fatalf("body = %s", got)
	}
	Send(dir, "v1", s.URL)
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatal("telemetry posted more than once today")
	}
}

func TestSendQuietFailure(t *testing.T) {
	t.Setenv("USER", "quiet-user")
	dir := t.TempDir()
	Send(dir, "v1", "http://127.0.0.1:1")
	time.Sleep(20 * time.Millisecond)
	if _, err := os.Stat(dir + "/telemetry-last-date"); !os.IsNotExist(err) {
		t.Fatalf("failure should stay quiet and unstamped: %v", err)
	}
	if strings.TrimSpace("ok") != "ok" {
		t.Fatal("unreachable")
	}
}

func TestTelemetry_SendsAgainAfterDayBoundary(t *testing.T) {
	t.Setenv("USER", "boundary-user")
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer s.Close()
	var clock atomic.Int64
	day1 := time.Date(2026, 9, 1, 23, 59, 0, 0, time.UTC)
	clock.Store(day1.UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunWithClock(ctx, t.TempDir(), "v1", s.URL, now, 5*time.Millisecond)
		close(done)
	}()
	defer func() { cancel(); <-done }()

	waitCalls := func(want int32) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for calls.Load() != want && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
		if got := calls.Load(); got != want {
			t.Fatalf("calls = %d, want %d", got, want)
		}
	}
	waitCalls(1)
	// Many polls on the same UTC day must not send again.
	time.Sleep(100 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("same-day calls = %d, want 1", got)
	}
	// Cross the UTC day boundary without restarting.
	clock.Store(day1.Add(2 * time.Minute).UnixNano())
	waitCalls(2)
	time.Sleep(100 * time.Millisecond)
	if got := calls.Load(); got != 2 {
		t.Fatalf("after boundary calls = %d, want 2 (at most one per day)", got)
	}
}
