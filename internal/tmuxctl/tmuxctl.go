// Package tmuxctl wraps tmux commands for managing devpass-code sessions.
// It provides functions to create windows, send keys, and capture pane text.
package tmuxctl

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Pane represents a tmux pane target (e.g. "mysession:0.0").
type Pane struct {
	Target string
}

// NewSession creates a new tmux session with the given name and returns the pane.
// The session starts with a detached window.
func NewSession(sessionName string) (*Pane, error) {
	cmd := exec.Command("tmux", "new-session", "-d", "-s", sessionName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("tmuxctl: new-session %q: %w: %s", sessionName, err, out)
	}
	return &Pane{Target: sessionName + ":0.0"}, nil
}

// Kill kills the tmux session.
func Kill(sessionName string) error {
	cmd := exec.Command("tmux", "kill-session", "-t", sessionName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmuxctl: kill-session %q: %w: %s", sessionName, err, out)
	}
	return nil
}

// SendKeys sends keystrokes to a pane.
func (p *Pane) SendKeys(keys string) error {
	cmd := exec.Command("tmux", "send-keys", "-t", p.Target, keys, "Enter")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tmuxctl: send-keys: %w: %s", err, out)
	}
	return nil
}

// SendRaw sends raw keystrokes (no Enter appended).
func (p *Pane) SendRaw(keys string) error {
	cmd := exec.Command("tmux", "send-keys", "-t", p.Target, keys, "")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tmuxctl: send-raw: %w: %s", err, out)
	}
	return nil
}

// CapturePaneText returns the visible text of the pane.
func (p *Pane) CapturePaneText() (string, error) {
	cmd := exec.Command("tmux", "capture-pane", "-t", p.Target, "-p")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("tmuxctl: capture-pane: %w", err)
	}
	return string(out), nil
}

// WaitForText polls the pane until the given substring appears or timeout expires.
func (p *Pane) WaitForText(substr string, timeout time.Duration, poll time.Duration) error {
	if poll == 0 {
		poll = 200 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		text, err := p.CapturePaneText()
		if err != nil {
			return err
		}
		if strings.Contains(text, substr) {
			return nil
		}
		time.Sleep(poll)
	}
	text, _ := p.CapturePaneText()
	return fmt.Errorf("tmuxctl: timeout waiting for %q in pane; last content:\n%s", substr, text)
}

// RunInPane starts a command inside the pane by sending it as keys.
func (p *Pane) RunInPane(command string) error {
	return p.SendKeys(command)
}
