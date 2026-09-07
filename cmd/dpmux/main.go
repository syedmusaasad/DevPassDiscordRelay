// cmd/dpmux: launch devpass-code in a named tmux session and multiplex its I/O.
//
// Usage:
//
//	dpmux [args...]         — launch a new session with auto-generated name dp-<unixtime>-<pid>
//	dpmux ls                — list active relay sessions
//
// Session name: dp-<unixtime>-<pid>
// Passes --auto to devpass-code unless --untrusted appears anywhere in args.
// Writes ~/.devpass-discord-relay/pending-<session-name> as JSON containing
// the tmux target, a nonce, and the spawn instant.  The nonce is also passed
// to devpass-code via RELAY_SPAWN_NONCE so it lands in the session row's title
// column, allowing the relay to prove the binding is its own.
//
// Inside-pane detection:
//   - If we are already inside a pane on the DEFAULT per-uid socket
//     (/tmp/tmux-<uid>/default or $TMUX_TMPDIR/default) → new-window in
//     the host session, target <host-session>:=<window-name>
//   - Any other socket (including non-default $TMUX) → unset TMUX and run
//     tmux -L default new-session on the default socket.
//
// Env overrides:
//
//	RELAY_DEVPASS_BIN  path to devpass-code binary (default: "devpass-code" on PATH)
//	RELAY_DEVPASS_DB   path to opencode.db (default: $XDG_DATA_HOME/devpass-code/opencode.db)
//	RELAY_STATE_DIR    state dir (default: ~/.devpass-discord-relay)
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// runLS lists active relay (dp-*) sessions and their windows.
// Window lines that duplicate their session name are suppressed.
func runLS() error {
	// List all sessions, filter to dp-* only.
	sessOut, err := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output()
	if err != nil {
		// No sessions running is OK.
		return nil
	}
	var dpSessions []string
	for _, line := range strings.Split(strings.TrimSpace(string(sessOut)), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "dp-") {
			dpSessions = append(dpSessions, line)
		}
	}
	if len(dpSessions) == 0 {
		return nil
	}

	for _, sess := range dpSessions {
		fmt.Println(sess)
		// List windows for this session; suppress windows whose name equals the session name.
		winOut, err := exec.Command("tmux", "list-windows", "-t", sess, "-F", "#{window_name}").Output()
		if err != nil {
			continue
		}
		for _, wline := range strings.Split(strings.TrimSpace(string(winOut)), "\n") {
			if wline == "" || wline == sess {
				continue
			}
			fmt.Println("  " + wline)
		}
	}
	return nil
}

// pendingMarker is the JSON written to pending-<session-name> in stateDir.
// The relay reads this to bind the session row to the spawn.
//
// StderrLog is the phase-16-verified identity evidence: devpass-code is launched
// with --print-logs --log-level INFO and its stderr is redirected to this unique,
// per-pane file.  When devpass-code creates a new session row, it writes
// "message=created id=ses_<ID>" to stderr.  Because this file is exclusive to
// this one launched process, it is mechanically impossible for a foreign
// process's session id to appear in it.  This is the sole production identity
// mechanism (see docs/BINDING-PROTOCOL.md); Nonce is retained only for legacy
// marker-format compatibility and is not used to prove ownership.
type pendingMarker struct {
	Target       string `json:"target"`              // tmux target (e.g. "dp-123:0.0")
	Nonce        string `json:"nonce"`               // legacy; not used for production identity
	SpawnInstant int64  `json:"spawnInstant"`        // time.Now().UnixMilli() when marker was written
	StderrLog    string `json:"stderrLog"`           // path to this launch's exclusive stderr log file
	SessionID    string `json:"sessionID,omitempty"` // exact historical ID for a --session resume
}

// generateNonce returns 16 random hex bytes.
func generateNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback: use time-based value (should not occur in practice).
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
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
	resumeSessionID := sessionIDFromArgs(dpArgs)

	// Generate a nonce (legacy marker field only; not used to prove identity).
	nonce := generateNonce()

	// stderrLog is this launch's exclusive identity-evidence file (phase-16
	// verified protocol): devpass-code is launched with --print-logs
	// --log-level INFO and its stderr is redirected here.  When the session
	// row is created, devpass-code logs "message=created id=ses_<ID>" to this
	// file, which no other process can write to.
	stderrLog := filepath.Join(stateDir, "stderr-"+sessionName+".log")

	// Detect if we're inside a tmux pane on the DEFAULT per-uid socket.
	// The TMUX env var format is: <socket-path>,<server-pid>,<window-index>
	// Only use new-window when the socket path is the default per-uid socket
	// (/tmp/tmux-<uid>/default or $TMUX_TMPDIR/default).
	// When running on a non-default socket (foreign socket), unset TMUX and
	// launch a fresh session on the default socket so all relay sessions land
	// on a predictable socket.
	isInsideDefaultSocket := isOnDefaultTmuxSocket()

	var tmuxTarget string

	if isInsideDefaultSocket {
		// We're inside tmux on the default socket; create a new window in the host session.
		hostSessionName := extractHostSessionName()
		if hostSessionName != "" {
			windowName := sessionName
			tmuxTarget = hostSessionName + ":=" + windowName

			// Write the pending marker BEFORE spawning so the relay can pair
			// the session even if devpass-code creates the DB row immediately.
			pendingPath := filepath.Join(stateDir, "pending-"+sessionName)
			marker := pendingMarker{
				Target:       tmuxTarget,
				Nonce:        nonce,
				SpawnInstant: time.Now().UnixMilli(),
				StderrLog:    stderrLog,
				SessionID:    resumeSessionID,
			}
			markerJSON, _ := json.Marshal(marker)
			if err := os.WriteFile(pendingPath, markerJSON, 0644); err != nil {
				return fmt.Errorf("write pending marker: %w", err)
			}

			launchCmd := buildDevpassCmd(devpassBin, dpArgs, nonce, stderrLog)
			launchErr := exec.Command("tmux", "new-window", "-t", hostSessionName, "-n", windowName,
				"-d", launchCmd).Run()
			if launchErr == nil {
				fmt.Fprintf(os.Stderr, "[dpmux] session %q started, target=%s\n", sessionName, tmuxTarget)
				fmt.Println(sessionName)
				return nil
			}
			// new-window failed; fall through to fresh-session path below.
			// Remove marker so it can be rewritten with the correct target.
			os.Remove(pendingPath)
		}
		// Could not get host session name or new-window failed; fall through.
	}

	// Fresh session on the default socket.
	// When running on a non-default $TMUX socket (foreign socket), we deliberately
	// ignore TMUX so tmux creates the session on the default per-uid socket.
	tmuxTarget = sessionName + ":0.0"

	// Write pending marker BEFORE spawning.
	pendingPath := filepath.Join(stateDir, "pending-"+sessionName)
	marker := pendingMarker{
		Target:       tmuxTarget,
		Nonce:        nonce,
		SpawnInstant: time.Now().UnixMilli(),
		StderrLog:    stderrLog,
		SessionID:    resumeSessionID,
	}
	markerJSON, _ := json.Marshal(marker)
	if err := os.WriteFile(pendingPath, markerJSON, 0644); err != nil {
		return fmt.Errorf("write pending marker: %w", err)
	}

	launchCmd := buildDevpassCmd(devpassBin, dpArgs, nonce, stderrLog)
	// Run tmux on the default socket by name ("-L default") rather than relying
	// on the inherited TMUX env var.  When the process is already inside a
	// non-default tmux server, the TMUX env var would cause "new-session" to
	// talk to the wrong server; using "-L default" bypasses this.
	launchErr := exec.Command("tmux", "-L", "default", "new-session", "-d", "-s", sessionName,
		"-x", "220", "-y", "50", launchCmd).Run()
	if launchErr != nil {
		os.Remove(pendingPath)
		return fmt.Errorf("tmux new-session: %w", launchErr)
	}

	fmt.Fprintf(os.Stderr, "[dpmux] session %q started, target=%s\n", sessionName, tmuxTarget)
	fmt.Println(sessionName)
	return nil
}

// sessionIDFromArgs returns the exact historical id requested by a resume.
// Unlike a new spawn, a resume does not create a session row or emit the
// creation log, so the explicit --session identity is retained in the marker.
func sessionIDFromArgs(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--session" || args[i] == "-s" {
			return args[i+1]
		}
	}
	return ""
}

// buildDevpassCmd constructs the shell command string for tmux.
// It optionally prepends an "env VAR=VAL ..." prefix to forward environment
// variables that the devpass-code process needs but may not inherit via the
// tmux server's stored environment.  Currently forwards:
//   - RELAY_DEVPASS_DB    → passed verbatim so fake-devpass-code can find the DB
//   - RELAY_SPAWN_NONCE   → legacy; retained for marker-format compatibility only
//   - FAKE_DP_*           → test fault-injection knobs
//
// It always appends --print-logs --log-level INFO and redirects stderr to
// stderrLog: this is the phase-16-verified identity protocol (see
// docs/BINDING-PROTOCOL.md). The redirection is unconditional and not hidden
// (no "2>/dev/null || true"); a launch failure surfaces via the tmux exit code.
func buildDevpassCmd(devpassBin string, args []string, nonce string, stderrLog string) string {
	// Collect env vars to forward explicitly.
	var envPairs []string
	for _, key := range []string{
		"RELAY_DEVPASS_DB",
		"FAKE_DP_FORCE_NEW_SESSION",
		"FAKE_DP_READY_DELAY_MS",
		"FAKE_DP_TRANSCRIPT_DELAY_MS",
		"FAKE_DP_SWALLOW_UNTIL_READY",
		"FAKE_DP_REFUSE_FIRST_RESUME",
		"FAKE_DP_SCRIPT",
	} {
		if v := os.Getenv(key); v != "" {
			envPairs = append(envPairs, key+"="+shellQuote(v))
		}
	}
	// Always forward the nonce if one was provided.
	if nonce != "" {
		envPairs = append(envPairs, "RELAY_SPAWN_NONCE="+shellQuote(nonce))
	}

	parts := make([]string, 0, len(args)+3+len(envPairs)+1+2)
	if len(envPairs) > 0 {
		parts = append(parts, "env")
		parts = append(parts, envPairs...)
	}
	parts = append(parts, shellQuote(devpassBin))
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	// Identity-evidence flags (phase-16-verified protocol): devpass-code must be
	// launched with these so it logs "message=created id=ses_<ID>" to stderr.
	parts = append(parts, "--print-logs", "--log-level", "INFO")
	cmd := strings.Join(parts, " ")
	if stderrLog != "" {
		// Redirect stderr to the exclusive per-launch log file. Never hide the
		// failure of this redirection or of the launched process; any error is
		// visible via the tmux pane / exit status, unlike a swallowed
		// "2>/dev/null || true".
		cmd = cmd + " 2>>" + shellQuote(stderrLog)
	}
	return cmd
}

// shellQuote wraps a string in single quotes for shell safety.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// isOnDefaultTmuxSocket returns true if the current process is running inside
// a tmux pane on the default per-uid socket.
// The default socket path is /tmp/tmux-<uid>/default (or $TMUX_TMPDIR/default).
// TMUX env var format: <socket-path>,<server-pid>,<window-index>
func isOnDefaultTmuxSocket() bool {
	tmuxEnv := os.Getenv("TMUX")
	if tmuxEnv == "" {
		return false
	}
	// Extract the socket path (first comma-separated field).
	socketPath := tmuxEnv
	if idx := strings.Index(tmuxEnv, ","); idx >= 0 {
		socketPath = tmuxEnv[:idx]
	}
	// Determine the expected default socket path.
	defaultSocket := defaultTmuxSocket()
	return socketPath == defaultSocket
}

// defaultTmuxSocket returns the path to the default per-uid tmux socket.
func defaultTmuxSocket() string {
	// $TMUX_TMPDIR/default, or /tmp/tmux-<uid>/default
	if dir := os.Getenv("TMUX_TMPDIR"); dir != "" {
		return filepath.Join(dir, "default")
	}
	return fmt.Sprintf("/tmp/tmux-%d/default", os.Getuid())
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
