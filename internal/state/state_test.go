package state_test

import (
	"testing"

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
