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
	if err := verifyBinary(localTmuxBin); err == nil {
		return localTmuxBin, nil
	}
	// Keep the executable name in one resolver-owned constant. Every production
	// caller reaches tmux through this function rather than invoking it directly.
	path, err := exec.LookPath(tmuxExecutableName)
	if err != nil {
		return "", fmt.Errorf("tmux not found: %w", err)
	}
	if err := verifyBinary(path); err != nil {
		return "", fmt.Errorf("tmux %q: %w", path, err)
	}
	return path, nil
}

const tmuxExecutableName = "tmux"

// localTmuxBin is a variable so resolver tests can exercise the managed-path
// fallback without writing to /usr/local/bin.
var localTmuxBin = "/usr/local/bin/tmux"

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
		wrapped := []string{"--user", "--scope", "--collect"}
		// systemd-run --user starts the scope from the manager environment, not
		// necessarily this dpmux process's environment.  Forward the values a
		// tmux birth needs explicitly, both to the transient scope and to the
		// tmux client itself.  In particular, RELAY_STATE_DIR contains the
		// durable pending marker that must be visible before the child starts.
		birthEnv := []string{"TMUX_TMPDIR", "RELAY_STATE_DIR", "HOME"}
		var commandEnv []string
		for _, key := range birthEnv {
			if value := os.Getenv(key); value != "" {
				assignment := key + "=" + value
				wrapped = append(wrapped, "--setenv="+assignment)
				commandEnv = append(commandEnv, assignment)
			}
		}
		wrapped = append(wrapped, "--")
		if len(commandEnv) > 0 {
			// Some user managers do not propagate --setenv into a transient
			// scope's client process, so make the client environment explicit too.
			wrapped = append(wrapped, "env")
			wrapped = append(wrapped, commandEnv...)
		}
		wrapped = append(wrapped, tmuxBin)
		wrapped = append(wrapped, args...)
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
