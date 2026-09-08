// Package inject sends Discord thread replies into a live devpass-code tmux pane.
//
// Flow:
//  1. Only messages from DISCORD_USER_ID are honoured; bots and other authors are dropped.
//  2. Readiness probe: capture-pane polls for "Ask anything..." up to 30 s;
//     falls back to a fixed 2 s sleep if the pane never becomes ready.
//  3. Image attachments are downloaded to <stateDir>/staging/<sessionID>/ with
//     filenames sanitised of exotic whitespace.  Their local path is appended to the
//     injected text.
//  4. `tmux send-keys -t <target> -l <text>` then Enter.
//  5. `!title <t>` renames the Discord thread.
package inject

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/devpass/devpass-discord-relay/internal/tmuxctl"
)

const (
	readinessProbeTimeout = 30 * time.Second
	readinessProbePoll    = 200 * time.Millisecond
	readinessFallback     = 2 * time.Second
	readinessSignature    = "Ask anything..."
)

// ThreadRenamer is an optional interface a Discord client may implement to
// rename a thread.
type ThreadRenamer interface {
	RenameThread(threadID, name string) error
}

// Injector injects text into a live tmux pane.
type Injector struct {
	StateDir string // root state dir; staging lives at <StateDir>/staging/<sessionID>/
}

// New creates an Injector.
func New(stateDir string) *Injector {
	return &Injector{StateDir: stateDir}
}

// ShouldAcceptAuthor returns true if the message author should be honoured.
// authorID is the message author's Discord user ID; allowedUserID is
// DISCORD_USER_ID from the environment.  Empty allowedUserID means "accept all".
// Bot messages (isBot=true) are always dropped.
func ShouldAcceptAuthor(authorID string, isBot bool, allowedUserID string) bool {
	if isBot {
		return false
	}
	if allowedUserID == "" {
		return true
	}
	return authorID == allowedUserID
}

// Inject performs the full inject sequence for one Discord thread reply:
//  1. Download any image attachments.
//  2. Wait for pane readiness (or fall back).
//  3. Send the (augmented) text to the pane.
//
// Parameters:
//
//	target      – tmux pane target, e.g. "mysession:0.0"
//	sessionID   – devpass session ID (used for staging dir)
//	content     – message text
//	attachments – list of (filename, url) pairs for image attachments
func (inj *Injector) Inject(target, sessionID, content string, attachments []Attachment) error {
	// Download attachments
	extraPaths, err := inj.downloadAttachments(sessionID, attachments)
	if err != nil {
		log.Printf("inject: attachment download partial error: %v", err)
	}

	// Build the final text
	text := content
	if len(extraPaths) > 0 {
		if text != "" {
			text += "\n"
		}
		text += strings.Join(extraPaths, "\n")
	}

	// Readiness probe
	waitForReady(target)

	// Send to pane
	return sendKeysLiteral(target, text)
}

// Attachment is a named URL for a file attachment.
type Attachment struct {
	Filename string
	URL      string
}

// downloadAttachments downloads each attachment into the staging dir and
// returns the list of local paths.
func (inj *Injector) downloadAttachments(sessionID string, attachments []Attachment) ([]string, error) {
	if len(attachments) == 0 {
		return nil, nil
	}
	stagingDir := filepath.Join(inj.StateDir, "staging", sessionID)
	if err := os.MkdirAll(stagingDir, 0755); err != nil {
		return nil, fmt.Errorf("inject: mkdir staging: %w", err)
	}
	var paths []string
	var lastErr error
	for _, att := range attachments {
		localPath, err := downloadFile(stagingDir, att.Filename, att.URL)
		if err != nil {
			lastErr = err
			log.Printf("inject: download %s: %v", att.Filename, err)
			continue
		}
		paths = append(paths, localPath)
	}
	return paths, lastErr
}

// DownloadAttachments downloads attachments into <stateDir>/staging/<sessionID>/ and
// returns the local file paths. Exported for testing.
func (inj *Injector) DownloadAttachments(sessionID string, attachments []Attachment) ([]string, error) {
	return inj.downloadAttachments(sessionID, attachments)
}

// downloadFile downloads a URL into dir/<sanitised-filename> and returns the path.
func downloadFile(dir, filename, rawURL string) (string, error) {
	safe := sanitiseFilename(filename)
	if safe == "" {
		safe = "attachment"
	}
	dest := filepath.Join(dir, safe)

	resp, err := http.Get(rawURL) //nolint:noctx
	if err != nil {
		return "", fmt.Errorf("inject: get %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	f, err := os.Create(dest)
	if err != nil {
		return "", fmt.Errorf("inject: create %s: %w", dest, err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return "", fmt.Errorf("inject: write %s: %w", dest, err)
	}
	return dest, nil
}

// sanitiseFilename removes exotic whitespace and control characters from a filename.
func sanitiseFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		// Allow normal printable ASCII + safe unicode letters/digits/punctuation
		if r == os.PathSeparator || r == 0 {
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		// Replace any whitespace that isn't a plain space with underscore
		if unicode.IsSpace(r) && r != ' ' {
			b.WriteRune('_')
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// waitForReady probes the tmux pane for the readiness signature up to 30 s;
// falls back to a fixed 2 s sleep.
func waitForReady(target string) {
	deadline := time.Now().Add(readinessProbeTimeout)
	for time.Now().Before(deadline) {
		text, err := capturePaneText(target)
		if err == nil && strings.Contains(text, readinessSignature) {
			return
		}
		time.Sleep(readinessProbePoll)
	}
	// Fallback
	time.Sleep(readinessFallback)
}

// capturePaneText captures the visible text of a tmux pane.
func capturePaneText(target string) (string, error) {
	tmuxBin, err := tmuxctl.ResolveTmuxBin()
	if err != nil {
		return "", err
	}
	out, err := exec.Command(tmuxBin, "capture-pane", "-t", target, "-p").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// sendKeysLiteral sends text to a tmux pane using -l (literal) mode, then Enter.
func sendKeysLiteral(target, text string) error {
	tmuxBin, err := tmuxctl.ResolveTmuxBin()
	if err != nil {
		return err
	}
	cmd := exec.Command(tmuxBin, "send-keys", "-t", target, "-l", text)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("inject: send-keys literal: %w: %s", err, out)
	}
	// Press Enter
	cmd2 := exec.Command(tmuxBin, "send-keys", "-t", target, "", "Enter")
	if out, err := cmd2.CombinedOutput(); err != nil {
		return fmt.Errorf("inject: send-keys enter: %w: %s", err, out)
	}
	return nil
}

// HandleTitleCommand checks if content is a !title <t> command and returns the
// title and true if so, or ("", false) otherwise.
func HandleTitleCommand(content string) (title string, ok bool) {
	s := strings.TrimSpace(content)
	if !strings.HasPrefix(s, "!title ") {
		return "", false
	}
	t := strings.TrimSpace(strings.TrimPrefix(s, "!title "))
	if t == "" {
		return "", false
	}
	return t, true
}
