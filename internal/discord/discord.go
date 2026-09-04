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
	}
	s, err := discordgo.New("Bot " + cfg.Token)
	if err != nil {
		return nil, err
	}
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
