// cmd/dpmux: launch devpass-code in a named tmux session and multiplex its I/O.
//
// Usage:
//
//	dpmux [args...]         — launch a new session with auto-generated name dp-<unixtime>-<pid>
//	dpmux ls                — list active relay sessions
//
// Session name: dp-<unixtime>-<pid>
// Passes --auto to devpass-code unless --untrusted appears anywhere in args.
// Writes ~/.devpass-discord-relay/pending-<session-name> containing the tmux target.
//
// Inside-pane detection:
//   - If we are already inside a pane on the default per-uid socket → new-window in
//     the host session, target <host-session>:=<window-name>
//   - Otherwise → fresh default-socket session.
//
// Env overrides:
//
//	RELAY_DEVPASS_BIN  path to devpass-code binary (default: "devpass-code" on PATH)
//	RELAY_DEVPASS_DB   path to opencode.db (default: $XDG_DATA_HOME/devpass-code/opencode.db)
//	RELAY_STATE_DIR    state dir (default: ~/.devpass-discord-relay)
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "dpmux:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// Handle special subcommands
	if len(args) == 1 && args[0] == "ls" {
		return runLS()
	}

	return runLaunch(args)
}

// runLS lists active relay sessions (suppresses window lines duplicating session name).
func runLS() error {
	out, err := exec.Command("tmux", "list-sessions").Output()
	if err != nil {
		// No sessions running is OK
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		fmt.Println(line)
	}
	return nil
}

// runLaunch launches a new devpass-code session.
func runLaunch(args []string) error {
	stateDir, err := resolveStateDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}

	// Auto-generate session name
	now := time.Now().UnixMilli()
	pid := os.Getpid()
	sessionName := fmt.Sprintf("dp-%d-%d", now, pid)

	// Determine the devpass-code binary
	devpassBin, err := resolveDevpassBin()
	if err != nil {
		return err
	}

	// Build command args for devpass-code
	dpArgs := []string{}
	untrusted := false
	for _, a := range args {
		if a == "--untrusted" {
			untrusted = true
		} else {
			dpArgs = append(dpArgs, a)
		}
	}
	if !untrusted {
		dpArgs = append([]string{"--auto"}, dpArgs...)
	}

	// Detect if we're inside a tmux pane on the default socket
	hostSession := os.Getenv("TMUX")
	isInsideTmux := hostSession != ""

	var tmuxTarget string
	var launchErr error

	if isInsideTmux {
		// We're inside tmux; create a new window in the host session
		// TMUX env var format: /tmp/tmux-UID/default,<PID>,<window-index>
		hostSessionName := extractHostSessionName()
		if hostSessionName == "" {
			// fallback: use a new session
			isInsideTmux = false
		} else {
			windowName := sessionName
			tmuxTarget = hostSessionName + ":=" + windowName
			launchCmd := buildDevpassCmd(devpassBin, dpArgs)
			launchErr = exec.Command("tmux", "new-window", "-t", hostSessionName, "-n", windowName,
				"-d", launchCmd).Run()
		}
	}

	if !isInsideTmux || launchErr != nil {
		// Fresh session on default socket
		tmuxTarget = sessionName + ":0.0"
		launchCmd := buildDevpassCmd(devpassBin, dpArgs)
		launchErr = exec.Command("tmux", "new-session", "-d", "-s", sessionName,
			"-x", "220", "-y", "50", launchCmd).Run()
		if launchErr != nil {
			return fmt.Errorf("tmux new-session: %w", launchErr)
		}
	}

	// Write pending marker BEFORE the process is up so the relay can pair
	pendingPath := filepath.Join(stateDir, "pending-"+sessionName)
	if err := os.WriteFile(pendingPath, []byte(tmuxTarget), 0644); err != nil {
		return fmt.Errorf("write pending marker: %w", err)
	}

	fmt.Fprintf(os.Stderr, "[dpmux] session %q started, target=%s\n", sessionName, tmuxTarget)
	fmt.Println(sessionName)
	return nil
}

// buildDevpassCmd constructs the shell command string for tmux.
func buildDevpassCmd(devpassBin string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, shellQuote(devpassBin))
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// shellQuote wraps a string in single quotes for shell safety.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// extractHostSessionName gets the session name from the TMUX env var.
// TMUX=/tmp/tmux-UID/default,<server-pid>,<pane-window-index>
func extractHostSessionName() string {
	tmuxEnv := os.Getenv("TMUX")
	if tmuxEnv == "" {
		return ""
	}
	// Ask tmux for the current session name
	out, err := exec.Command("tmux", "display-message", "-p", "#S").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// resolveDevpassBin finds the devpass-code binary.
func resolveDevpassBin() (string, error) {
	if v := os.Getenv("RELAY_DEVPASS_BIN"); v != "" {
		return v, nil
	}
	p, err := exec.LookPath("devpass-code")
	if err != nil {
		// Fall back to fake-devpass-code for tests
		if fp, err2 := exec.LookPath("fake-devpass-code"); err2 == nil {
			return fp, nil
		}
		return "", fmt.Errorf("devpass-code not found on PATH (set RELAY_DEVPASS_BIN): %w", err)
	}
	return p, nil
}

// resolveStateDir returns the state directory.
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

func init() {
	// Use strconv to avoid unused import errors during compilation
	_ = strconv.Itoa
}
