package telemetry

import (
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
