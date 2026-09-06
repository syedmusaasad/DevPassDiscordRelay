// cmd/relay: main entry point for the devpass-discord relay.
//
// Configuration via .env file resolved:
//
//	$DEVPASS_RELAY_ENV → ~/.config/devpass-discord-relay/.env → ./.env
//
// Required env vars:
//
//	DISCORD_TOKEN       Bot token
//	DISCORD_CHANNEL_ID  Channel to post session threads into
//
// Optional env vars:
//
//	RELAY_DEVPASS_DB    Path to opencode.db (default: $XDG_DATA_HOME/devpass-code/opencode.db)
//	RELAY_STATE_DIR     State dir (default: ~/.devpass-discord-relay)
//	DISCORD_API_URL     Override Discord API URL (for testing)
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/devpass/devpass-discord-relay/internal/mirror"
	_ "modernc.org/sqlite"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	if err := runRelay(ctx); err != nil {
		log.Fatalf("relay: %v", err)
	}
}

func runRelay(ctx context.Context) error {
	// Load .env config
	if err := loadEnv(); err != nil {
		log.Printf("relay: .env load: %v (continuing with environment)", err)
	}

	token := os.Getenv("DISCORD_TOKEN")
	channelID := os.Getenv("DISCORD_CHANNEL_ID")
	if token == "" || channelID == "" {
		return fmt.Errorf("DISCORD_TOKEN and DISCORD_CHANNEL_ID must be set")
	}

	dbPath, err := resolveDBPath()
	if err != nil {
		return err
	}

	stateDir, err := resolveStateDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}

	// Open the DB read-only via sqlite URI
	dsn := fmt.Sprintf("file:%s?mode=ro", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open db %s: %w", dbPath, err)
	}
	defer db.Close()

	// Create Discord client
	apiURL := os.Getenv("DISCORD_API_URL")
	dc, err := newDiscordGatewayClient(token, apiURL)
	if err != nil {
		return fmt.Errorf("discord client: %w", err)
	}

	cfg := mirror.Config{
		DiscordToken:  token,
		DiscordAPIURL: apiURL,
		ChannelID:     channelID,
		AllowedUserID: os.Getenv("DISCORD_USER_ID"),
		DBPath:        dbPath,
		StateDir:      stateDir,
		DpmuxBin:      os.Getenv("RELAY_DPMUX_BIN"), // optional override for dpmux binary path
	}
	if v := os.Getenv("RELAY_SPAWN_BIND_TIMEOUT_MS"); v != "" {
		if ms, parseErr := strconv.Atoi(v); parseErr == nil && ms > 0 {
			cfg.SpawnBindTimeout = time.Duration(ms) * time.Millisecond
		}
	}

	w, err := mirror.NewWatcher(cfg, db, dc)
	if err != nil {
		return fmt.Errorf("watcher: %w", err)
	}

	// Open gateway connection to receive MESSAGE_CREATE events.
	// A failure here is non-fatal: the relay can still mirror sessions to Discord
	// (outgoing direction works without a gateway connection); only incoming
	// Discord-to-pane injection will be unavailable until the gateway reconnects.
	if err := dc.s.Open(); err != nil {
		log.Printf("relay: discord gateway open: %v (continuing without inbound events)", err)
	} else {
		defer dc.s.Close()
	}

	log.Println("relay: starting watcher")
	w.Run(ctx)
	log.Println("relay: shutting down")
	return nil
}

// ── Discord client adapter ─────────────────────────────────────────────────────

// discordAdapter adapts discordgo.Session to mirror.DiscordClient.
type discordAdapter struct {
	s *discordgo.Session
}

func newDiscordGatewayClient(token, apiURL string) (*discordAdapter, error) {
	if apiURL != "" {
		// Override all package-level endpoint vars that were computed at init time.
		// Updating only EndpointAPI is insufficient because EndpointChannels and other
		// vars are initialized from the old EndpointAPI value.
		discordgo.EndpointDiscord = apiURL + "/"
		discordgo.EndpointAPI = apiURL + "/api/v" + discordgo.APIVersion + "/"
		// Re-derive vars that are read directly (not via functions).
		discordgo.EndpointChannels = discordgo.EndpointAPI + "channels/"
		discordgo.EndpointGateway = discordgo.EndpointAPI + "gateway"
		discordgo.EndpointGuilds = discordgo.EndpointAPI + "guilds/"
		discordgo.EndpointUsers = discordgo.EndpointAPI + "users/"
		discordgo.EndpointWebhooks = discordgo.EndpointAPI + "webhooks/"
	}
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	// Disable discordgo's internal rate-limit retry so *discordgo.RateLimitError
	// is returned to callers.  The relay handles 429 back-off itself in discordSend
	// and discordEdit, feeding the result into the Throttle for future pacing.
	s.ShouldRetryOnRateLimit = false
	return &discordAdapter{s: s}, nil
}

func (d *discordAdapter) SendMessage(channelID, content string) (mirror.MsgRef, error) {
	msg, err := d.s.ChannelMessageSend(channelID, content)
	if err != nil {
		return mirror.MsgRef{}, err
	}
	return mirror.MsgRef{ID: msg.ID, ChannelID: msg.ChannelID}, nil
}

func (d *discordAdapter) EditMessage(channelID, messageID, content string) error {
	_, err := d.s.ChannelMessageEdit(channelID, messageID, content)
	return err
}

func (d *discordAdapter) CreateThread(channelID, messageID, name string) (mirror.ThreadRef, error) {
	ch, err := d.s.MessageThreadStartComplex(channelID, messageID, &discordgo.ThreadStart{
		Name: name,
		Type: discordgo.ChannelTypeGuildPublicThread,
	})
	if err != nil {
		return mirror.ThreadRef{}, err
	}
	return mirror.ThreadRef{ID: ch.ID}, nil
}

func (d *discordAdapter) SendTyping(channelID string) error {
	return d.s.ChannelTyping(channelID)
}

func (d *discordAdapter) RenameThread(threadID, name string) error {
	_, err := d.s.ChannelEdit(threadID, &discordgo.ChannelEdit{Name: name})
	return err
}

func (d *discordAdapter) OnMessage(fn mirror.MessageHandler) {
	d.s.AddHandler(func(_ *discordgo.Session, ev *discordgo.MessageCreate) {
		if ev.Message == nil {
			return
		}
		me := mirror.MessageEvent{
			ChannelID: ev.ChannelID,
			Content:   ev.Content,
		}
		if ev.Author != nil {
			me.AuthorID = ev.Author.ID
			me.AuthorIsBot = ev.Author.Bot
		}
		for _, att := range ev.Attachments {
			if att != nil {
				me.Attachments = append(me.Attachments, mirror.MessageAttachment{
					Filename: att.Filename,
					URL:      att.URL,
				})
			}
		}
		fn(me)
	})
}

// ── Config helpers ─────────────────────────────────────────────────────────────

// loadEnv loads a .env file. Resolution order:
// 1. $DEVPASS_RELAY_ENV
// 2. ~/.config/devpass-discord-relay/.env
// 3. ./.env
func loadEnv() error {
	var paths []string
	if v := os.Getenv("DEVPASS_RELAY_ENV"); v != "" {
		paths = append(paths, v)
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "devpass-discord-relay", ".env"))
	}
	paths = append(paths, ".env")

	for _, p := range paths {
		if err := readDotEnv(p); err == nil {
			return nil
		}
	}
	return fmt.Errorf("no .env found in %v", paths)
}

// readDotEnv reads a .env file and sets environment variables (does not overwrite).
func readDotEnv(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		// Remove surrounding quotes
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') ||
			(val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
	return nil
}

// resolveDBPath returns the opencode.db path.
func resolveDBPath() (string, error) {
	if v := os.Getenv("RELAY_DEVPASS_DB"); v != "" {
		return v, nil
	}
	xdg := os.Getenv("XDG_DATA_HOME")
	if xdg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("home dir: %w", err)
		}
		xdg = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(xdg, "devpass-code", "opencode.db"), nil
}

// resolveStateDir returns the relay state directory.
func resolveStateDir() (string, error) {
	if v := os.Getenv("RELAY_STATE_DIR"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".devpass-discord-relay"), nil
}
