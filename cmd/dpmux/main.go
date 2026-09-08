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

	"github.com/devpass/devpass-discord-relay/internal/tmuxctl"
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
	if len(args) == 1 && args[0] == "update" {
		return runUpdate()
	}
	if len(args) == 1 && args[0] == "uninstall" {
		return runUninstall()
	}

	return runLaunch(args)
}

func runUpdate() error {
	root, err := updateSource()
	if err != nil {
		return err
	}
	// The install marker names a real checkout. Update its checked-out branch;
	// do not assume a fictitious "latest" tag exists.
	commands := [][]string{{"fetch", "--tags"}, {"pull", "--ff-only"}}
	for _, args := range commands {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
	}
	cmd := exec.Command("make", "build")
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rebuild: %w", err)
	}
	if err := installUpdatedBinaries(root); err != nil {
		return err
	}
	promptRestart()
	return nil
}

func installUpdatedBinaries(root string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	destDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}
	for _, name := range []string{"relay", "dpmux"} {
		data, err := os.ReadFile(filepath.Join(root, "bin", name))
		if err != nil {
			return fmt.Errorf("read rebuilt %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(destDir, name), data, 0755); err != nil {
			return fmt.Errorf("install rebuilt %s: %w", name, err)
		}
	}
	return nil
}

// updateSource reads the checkout recorded by install.sh. If an installed
// binary has no usable source, clone a fresh working tree instead of depending
// on the caller's current directory.
func updateSource() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	marker := filepath.Join(home, ".config", "devpass-discord-relay", "install-source")
	if data, readErr := os.ReadFile(marker); readErr == nil {
		root := strings.TrimSpace(string(data))
		if root != "" {
			if _, statErr := os.Stat(filepath.Join(root, ".git")); statErr == nil {
				return root, nil
			}
		}
	}
	root := filepath.Join(home, ".cache", "devpass-discord-relay", "source")
	if _, statErr := os.Stat(filepath.Join(root, ".git")); statErr != nil {
		if err := os.MkdirAll(filepath.Dir(root), 0755); err != nil {
			return "", err
		}
		cmd := exec.Command("git", "clone", "https://github.com/syedmusaasad/DevPassDiscordRelay.git", root)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("clone update source: %w", err)
		}
	}
	return root, nil
}

func promptRestart() {
	const instruction = "Update complete. Restart with: systemctl --user restart devpass-discord-relay"
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		fmt.Fprintln(os.Stderr, instruction)
		return
	}
	fmt.Fprint(os.Stderr, instruction+" now? [y/N] ")
	var answer string
	_, _ = fmt.Fscanln(os.Stdin, &answer)
	if strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes") {
		_ = exec.Command("systemctl", "--user", "restart", "devpass-discord-relay").Run()
	}
}

func runUninstall() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	configDir := filepath.Join(home, ".config", "devpass-discord-relay")
	envPath := filepath.Join(configDir, ".env")
	if data, err := os.ReadFile(envPath); err == nil {
		backup := envPath + ".uninstall.bak"
		if err := os.WriteFile(backup, data, 0600); err != nil {
			return fmt.Errorf("backup .env: %w", err)
		}
	}
	_ = exec.Command("systemctl", "--user", "disable", "--now", "devpass-discord-relay.service").Run()
	_ = os.Remove(filepath.Join(home, ".config", "systemd", "user", "devpass-discord-relay.service"))
	_ = os.Remove(filepath.Join(home, ".local", "bin", "relay"))
	_ = os.Remove(filepath.Join(home, ".local", "bin", "dpmux"))
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	fmt.Fprintln(os.Stderr, "Uninstalled; .env backup retained at "+envPath+".uninstall.bak")
	return nil
}

func repoRoot() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("locate repository: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// runLS lists active relay (dp-*) sessions and their windows.
// Window lines that duplicate their session name are suppressed.
func runLS() error {
	// List all sessions, filter to dp-* only.
	tmuxBin, err := tmuxctl.ResolveTmuxBin()
	if err != nil {
		return err
	}
	sessOut, err := exec.Command(tmuxBin, "list-sessions", "-F", "#{session_name}").Output()
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
		winOut, err := exec.Command(tmuxBin, "list-windows", "-t", sess, "-F", "#{window_name}").Output()
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
	tmuxBin, err := tmuxctl.ResolveTmuxBin()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[dpmux] tmux isolation mode: %s\n", tmuxctl.IsolationMode())

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
	// New relay-triggered conversations must not inherit a prior CLI session.
	// `--auto` alone may resume the most recent row, which is not a spawn.
	if resumeSessionID := sessionIDFromArgs(dpArgs); resumeSessionID == "" && os.Getenv("RELAY_NEW_SESSION") == "1" {
		os.Setenv("FAKE_DP_FORCE_NEW_SESSION", "1")
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
			launch, _ := tmuxctl.NewBirthCommand(tmuxBin, "new-window", "-t", hostSessionName, "-n", windowName,
				"-d", launchCmd)
			launchErr := launch.Run()
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
	if _, err := os.Stat(pendingPath); err != nil {
		return fmt.Errorf("verify pending marker: %w", err)
	}

	launchCmd := buildDevpassCmd(devpassBin, dpArgs, nonce, stderrLog)
	// Run tmux on the default socket by name ("-L default") rather than relying
	// on the inherited TMUX env var.  When the process is already inside a
	// non-default tmux server, the TMUX env var would cause "new-session" to
	// talk to the wrong server; using "-L default" bypasses this.
	launchCmdExec, _ := tmuxctl.NewBirthCommand(tmuxBin, "new-session", "-d", "-s", sessionName,
		"-x", "220", "-y", "50", launchCmd)
	launchErr := launchCmdExec.Run()
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
		"RELAY_STATE_DIR",
		"RELAY_DEVPASS_DB",
		"XDG_DATA_HOME",
		"RELAY_NEW_SESSION",
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
	tmuxBin, err := tmuxctl.ResolveTmuxBin()
	if err != nil {
		return ""
	}
	out, err := exec.Command(tmuxBin, "display-message", "-p", "#S").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// resolveDevpassBin finds the devpass-code binary.
func resolveDevpassBin() (string, error) {
	if v := os.Getenv("RELAY_DEVPASS_BIN"); v != "" {
		if err := verifyDevpassBin(v); err != nil {
			return "", fmt.Errorf("RELAY_DEVPASS_BIN %q: %w", v, err)
		}
		return v, nil
	}
	if err := verifyDevpassBin("/usr/local/bin/devpass-code"); err == nil {
		return "/usr/local/bin/devpass-code", nil
	}
	p, err := exec.LookPath("devpass-code")
	if err != nil {
		return "", fmt.Errorf("devpass-code not found on PATH (set RELAY_DEVPASS_BIN): %w", err)
	}
	if err := verifyDevpassBin(p); err != nil {
		return "", fmt.Errorf("devpass-code %q: %w", p, err)
	}
	return p, nil
}

func verifyDevpassBin(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode()&0111 == 0 {
		return fmt.Errorf("not executable")
	}
	if err := exec.Command(path, "-V").Run(); err == nil {
		return nil
	}
	return exec.Command(path, "--version").Run()
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
