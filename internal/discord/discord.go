// Package discord wraps discordgo with configurable endpoints for testing.
// The EndpointURL and GatewayURL vars can be overridden to point at mock-discord.
package discord

import (
	"github.com/bwmarrin/discordgo"
)

// Config holds connection configuration.
type Config struct {
	Token      string
	GatewayURL string // override gateway URL (e.g. ws://localhost:PORT)
	APIURL     string // override REST API URL (e.g. http://localhost:PORT)
}

// Client wraps a discordgo.Session.
type Client struct {
	Session *discordgo.Session
	cfg     Config
}

// New creates a new Discord client with the given config.
// Set cfg.GatewayURL and cfg.APIURL to override endpoints for testing.
func New(cfg Config) (*Client, error) {
	if cfg.APIURL != "" {
		// Override the global REST endpoint before creating the session.
		// discordgo derives all API paths from EndpointDiscord at package level.
		discordgo.EndpointDiscord = cfg.APIURL + "/"
		discordgo.EndpointAPI = cfg.APIURL + "/api/v" + discordgo.APIVersion + "/"
		// EndpointChannels (and everything derived from it, e.g.
		// EndpointChannelMessages/EndpointChannelMessageThread) is a package
		// variable computed once from the OLD EndpointAPI at discordgo's
		// package-init time. Re-deriving only EndpointAPI above is not
		// sufficient: ChannelMessageSend/Edit/CreateThread would otherwise
		// still target the real discord.com REST API. Every endpoint var that
		// callers below actually use must be re-derived explicitly.
		discordgo.EndpointChannels = discordgo.EndpointAPI + "channels/"
		discordgo.EndpointGuilds = discordgo.EndpointAPI + "guilds/"
		discordgo.EndpointUsers = discordgo.EndpointAPI + "users/"
		discordgo.EndpointWebhooks = discordgo.EndpointAPI + "webhooks/"
		discordgo.EndpointGateway = discordgo.EndpointAPI + "gateway"
	}
	s, err := discordgo.New("Bot " + cfg.Token)
	if err != nil {
		return nil, err
	}
	// Disable discordgo's internal rate-limit retry/sleep loop so that a real
	// 429 response surfaces to the caller as *discordgo.RateLimitError instead
	// of being silently slept-and-retried inside discordgo. MaxRestRetries only
	// bounds a *separate* 502 retry path and does not disable this 429 loop
	// (see discordgo's RequestWithLockedBucket: `if cfg.ShouldRetryOnRateLimit {
	// ... time.Sleep(rl.RetryAfter) ... } else { err = &RateLimitError{...} }`).
	// The relay's own bounded, context-cancellable retry + channel-throttle
	// code (discordSend/discordEdit in internal/mirror) is what must own 429
	// handling in production, not discordgo.
	s.ShouldRetryOnRateLimit = false
	return &Client{Session: s, cfg: cfg}, nil
}

// SendMessage sends a message to a channel.
func (c *Client) SendMessage(channelID, content string) (*discordgo.Message, error) {
	return c.Session.ChannelMessageSend(channelID, content)
}

// EditMessage edits an existing message.
func (c *Client) EditMessage(channelID, messageID, content string) (*discordgo.Message, error) {
	return c.Session.ChannelMessageEdit(channelID, messageID, content)
}

// CreateThread creates a thread from a message.
func (c *Client) CreateThread(channelID, messageID, name string) (*discordgo.Channel, error) {
	return c.Session.MessageThreadStartComplex(channelID, messageID, &discordgo.ThreadStart{
		Name: name,
		Type: discordgo.ChannelTypeGuildPublicThread,
	})
}

// SendTyping sends a typing indicator to a channel.
func (c *Client) SendTyping(channelID string) error {
	return c.Session.ChannelTyping(channelID)
}
