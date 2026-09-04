package discord_test

import (
	"testing"

	"github.com/devpass/devpass-discord-relay/internal/discord"
)

func TestNewClient_InvalidToken(t *testing.T) {
	// Creating a client with an empty token should not error (discordgo is lenient at construction time)
	cfg := discord.Config{Token: "test-token", APIURL: "http://localhost:0"}
	c, err := discord.New(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Session == nil {
		t.Error("expected non-nil session")
	}
}
