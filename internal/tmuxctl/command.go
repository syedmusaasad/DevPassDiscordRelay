package tmuxctl

import (
	"fmt"
	"os"
	"os/exec"
)

// ResolveTmuxBin resolves tmux without allowing an untrusted PATH entry to win
// over the managed installation. An explicit override is useful in tests.
func ResolveTmuxBin() (string, error) {
	if configured := os.Getenv("RELAY_TMUX_BIN"); configured != "" {
		if err := verifyBinary(configured); err != nil {
			return "", fmt.Errorf("RELAY_TMUX_BIN %q: %w", configured, err)
		}
		return configured, nil
	}
	if err := verifyBinary("/usr/local/bin/tmux"); err == nil {
		return "/usr/local/bin/tmux", nil
	}
	path, err := exec.LookPath("tmux")
	if err != nil {
		return "", fmt.Errorf("tmux not found: %w", err)
	}
	if err := verifyBinary(path); err != nil {
		return "", fmt.Errorf("tmux %q: %w", path, err)
	}
	return path, nil
}

func verifyBinary(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode()&0111 == 0 {
		return fmt.Errorf("not executable")
	}
	if err := exec.Command(path, "-V").Run(); err != nil {
		return fmt.Errorf("-V failed: %w", err)
	}
	return nil
}

// IsolationMode reports whether new tmux-server births can be placed in an
// independent user scope. Read-only tmux commands intentionally remain direct.
func IsolationMode() string {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return "direct (systemd-run unavailable)"
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return "direct (systemctl unavailable)"
	}
	if err := exec.Command("systemctl", "--user", "show-environment").Run(); err != nil {
		return "direct (user manager unavailable)"
	}
	return "systemd-user-scope"
}

// NewBirthCommand wraps only commands that may create a tmux server. The
// fallback is intentionally direct when a user manager is not reachable.
func NewBirthCommand(tmuxBin string, args ...string) (*exec.Cmd, string) {
	mode := IsolationMode()
	if mode == "systemd-user-scope" {
		wrapped := append([]string{"--user", "--scope", "--collect", "--", tmuxBin}, args...)
		return exec.Command("systemd-run", wrapped...), "systemd-user-scope"
	}
	return exec.Command(tmuxBin, args...), mode
}

// NewCommand creates an unwrapped read-only or control tmux command.
func NewCommand(args ...string) (*exec.Cmd, error) {
	tmuxBin, err := ResolveTmuxBin()
	if err != nil {
		return nil, err
	}
	return exec.Command(tmuxBin, args...), nil
}
