package state_test

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/devpass/devpass-discord-relay/internal/mirror"
	"github.com/devpass/devpass-discord-relay/internal/state"
)

func TestStoreSetGet(t *testing.T) {
	s := state.New()
	ss := &state.SessionState{
		SessionID:       "ses_test",
		DiscordThreadID: "thread1",
	}
	s.Set(ss)
	got := s.Get("ses_test")
	if got == nil {
		t.Fatal("expected session state, got nil")
	}
	if got.SessionID != "ses_test" {
		t.Errorf("got %q want ses_test", got.SessionID)
	}
}

// TestTTL_NeverSweepsLiveWindow guards restart retention: age alone is never
// enough to discard a session record while its tmux target remains alive.
func TestTTL_NeverSweepsLiveWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	old := time.Now().Add(-31 * 24 * time.Hour).UnixMilli()
	if err := os.WriteFile(path, []byte(`[{"session_id":"ses_live","tmux_target":"relay:0.0","updated_at":`+strconv.FormatInt(old, 10)+`}]`), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := mirror.LoadSessions(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SweepExpired(30*24*time.Hour, func(target string) bool { return target == "relay:0.0" }, time.Now()); err != nil {
		t.Fatal(err)
	}
	if store.Get("ses_live") == nil {
		t.Fatal("TTL sweep removed a record whose window is alive")
	}
}

func TestStoreDelete(t *testing.T) {
	s := state.New()
	s.Set(&state.SessionState{SessionID: "ses_a"})
	s.Delete("ses_a")
	if s.Get("ses_a") != nil {
		t.Error("expected nil after delete")
	}
}

func TestStoreAll(t *testing.T) {
	s := state.New()
	s.Set(&state.SessionState{SessionID: "ses_1"})
	s.Set(&state.SessionState{SessionID: "ses_2"})
	all := s.All()
	if len(all) != 2 {
		t.Errorf("got %d want 2", len(all))
	}
}
