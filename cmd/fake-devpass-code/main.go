// cmd/fake-devpass-code: test double for devpass-code v1.18.11.
//
// VERSION-PIN: mirrors devpass-code 1.18.11 behaviour as documented in
// docs/DISCOVERY.md and docs/TRANSCRIPT-SCHEMA.md.
//
// Behaviour:
//   - Writes SQLite schema to $XDG_DATA_HOME/devpass-code/opencode.db
//     (default: $HOME/.local/share/devpass-code/opencode.db)
//   - Renders a tmux-style pane whose ready signature is "Ask anything..."
//   - Honors --auto flag (suppresses permission prompts)
//   - Honors --session <id> / -s <id>: fails with
//     "Error: Session not found: <id>" if the session doesn't exist in DB
//   - Reads a script of turns from $FAKE_DP_SCRIPT (JSON array of turns)
//   - Fault knobs via env:
//     FAKE_DP_REFUSE_FIRST_RESUME=1      — refuse first --session resume
//     FAKE_DP_READY_DELAY_MS=<n>        — delay before showing ready prompt
//     FAKE_DP_TRANSCRIPT_DELAY_MS=<n>   — delay before writing session row to DB
//     FAKE_DP_SWALLOW_UNTIL_READY=1     — (documented; input is always stdin here)
//
// Script format ($FAKE_DP_SCRIPT):
//
//	[{"prompt": "ping", "reply": "Pong."}, ...]
//
// The process stays alive reading stdin for prompts and writing replies
// to stdout after inserting them into the DB, until EOF or SIGINT.
package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const devpassVersion = "1.18.11"

// scriptTurn is one entry in FAKE_DP_SCRIPT.
type scriptTurn struct {
	Prompt string `json:"prompt"`
	Reply  string `json:"reply"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// Parse flags
	var sessionID string
	var autoFlag bool
	var printLogs bool
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--auto":
			autoFlag = true
		case "--session", "-s":
			if i+1 < len(args) {
				i++
				sessionID = args[i]
			}
		case "--print-logs":
			printLogs = true
		case "--log-level":
			if i+1 < len(args) {
				i++ // consume the level value; we always log at INFO-equivalent verbosity
			}
		case "--version":
			fmt.Println(devpassVersion)
			return nil
		}
	}
	_ = autoFlag

	// Determine DB path
	dbPath, err := resolveDBPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return fmt.Errorf("fake-devpass-code: mkdir: %w", err)
	}

	// Apply FAKE_DP_TRANSCRIPT_DELAY_MS before opening/creating DB
	if d := envMS("FAKE_DP_TRANSCRIPT_DELAY_MS"); d > 0 {
		time.Sleep(d)
	}

	// Open (or create) the DB
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("fake-devpass-code: open db: %w", err)
	}
	defer db.Close()

	if err := applySchema(db); err != nil {
		return fmt.Errorf("fake-devpass-code: schema: %w", err)
	}

	// Handle --session resume
	if sessionID != "" {
		// Check FAKE_DP_REFUSE_FIRST_RESUME
		if os.Getenv("FAKE_DP_REFUSE_FIRST_RESUME") == "1" {
			// Refuse unconditionally on first call; clear the env so subsequent calls pass
			// (We can't actually clear env for a new process; just refuse.)
			fmt.Fprintf(os.Stderr, "Error: Session not found: %s\n", sessionID)
			os.Exit(1)
		}

		// Check if session exists
		var exists int
		row := db.QueryRow(`SELECT COUNT(*) FROM session WHERE id = ?`, sessionID)
		if err := row.Scan(&exists); err != nil || exists == 0 {
			fmt.Fprintf(os.Stderr, "Error: Session not found: %s\n", sessionID)
			os.Exit(1)
		}
	}

	// Ensure a session row exists.
	// With --auto and no explicit --session, resume the most recently created session
	// if one exists in the DB (simulates dpmux-spawned devpass-code resuming a tracked
	// session). Fall back to creating a new session only when the DB is empty.
	// FAKE_DP_FORCE_NEW_SESSION=1 skips the auto-resume and always creates a new session.
	activeSessionID := sessionID
	if activeSessionID == "" {
		if autoFlag && os.Getenv("FAKE_DP_FORCE_NEW_SESSION") != "1" {
			var existing string
			row := db.QueryRow(`SELECT id FROM session ORDER BY time_created DESC LIMIT 1`)
			if err := row.Scan(&existing); err == nil && existing != "" {
				activeSessionID = existing
			}
		}
		if activeSessionID == "" {
			activeSessionID = generateID("ses")
			if err := insertSession(db, activeSessionID); err != nil {
				return fmt.Errorf("fake-devpass-code: insert session: %w", err)
			}
			// Reproduce the phase-16-verified real devpass-code protocol: log the
			// newly created session id to stderr in the exact format the real CLI
			// emits (see docs/BINDING-PROTOCOL.md). Only actual row-creation events
			// are logged; resuming an existing session never logs this line.
			if printLogs {
				fmt.Fprintf(os.Stderr, "time=%s level=INFO service=session message=created id=%s\n",
					time.Now().UTC().Format(time.RFC3339Nano), activeSessionID)
			}
		}
	}

	// Apply ready delay
	readyDelay := envMS("FAKE_DP_READY_DELAY_MS")
	swallowUntilReady := os.Getenv("FAKE_DP_SWALLOW_UNTIL_READY") == "1"

	if readyDelay > 0 {
		if swallowUntilReady {
			// Drain stdin during the delay so keys sent to the tmux pane before the
			// ready signature appears are silently discarded.
			drainStdin(readyDelay)
		} else {
			time.Sleep(readyDelay)
		}
	}

	// Print the ready signature (matches DISCOVERY.md §4)
	fmt.Println("")
	fmt.Println("   ┃")
	fmt.Printf("   ┃  Ask anything... \"What is the tech stack of this project?\"\n")
	fmt.Println("   ┃")
	fmt.Printf("   ┃  Pl-Interactive · devpass-code %s\n", devpassVersion)
	fmt.Println("")
	fmt.Println("> ")
	permissionPrompts := 0
	if os.Getenv("FAKE_DP_PERMISSION_PROMPT") == "1" {
		permissionPrompts = 1
	}
	if n := envMS("FAKE_DP_PERMISSION_PROMPTS"); n > 0 {
		permissionPrompts = int(n)
	}
	for i := 0; i < permissionPrompts; i++ {
		// Test double for the documented pane-only approval prompt. Its response
		// parser deliberately consumes terminal arrow sequences until Enter.
		fmt.Printf("  △ Permission required (%d)\n", i+1)
		fmt.Println("    # Shell command")
		fmt.Println("  $ ls")
		fmt.Println("   Allow once   Allow always   Reject  ctrl+f fullscreen  ⇆ select  enter confirm")
		rejected, err := consumePickerResponse(os.Stdin)
		if err != nil {
			return err
		}
		if rejected {
			fmt.Println("Permission rejected")
		} else {
			fmt.Println("Permission approved")
		}
		fmt.Println("> ")
	}
	if os.Getenv("FAKE_DP_QUESTION_PICKER") == "1" {
		// The real 1.18.11 TUI did not offer this picker; this is a fake-only
		// contract for exercising the relay's menu transport.
		fmt.Println("? Choose a fruit")
		fmt.Println("> apple")
		fmt.Println("  pear")
		fmt.Println("enter to confirm")
		if _, err := consumePickerResponse(os.Stdin); err != nil {
			return err
		}
		fmt.Println("Question answered")
		fmt.Println("> ")
	}

	// Load script if provided
	var script []scriptTurn
	if sf := os.Getenv("FAKE_DP_SCRIPT"); sf != "" {
		data, err := os.ReadFile(sf)
		if err != nil {
			return fmt.Errorf("fake-devpass-code: read script: %w", err)
		}
		if err := json.Unmarshal(data, &script); err != nil {
			return fmt.Errorf("fake-devpass-code: parse script: %w", err)
		}
	}

	// Interactive loop: read prompts from stdin, produce replies
	scanner := bufio.NewScanner(os.Stdin)
	turnIdx := 0
	for scanner.Scan() {
		prompt := strings.TrimSpace(scanner.Text())
		if prompt == "" {
			continue
		}

		var reply string
		if turnIdx < len(script) {
			reply = script[turnIdx].Reply
			turnIdx++
		} else {
			reply = fmt.Sprintf("(ack turn %d)", turnIdx+1)
		}

		now := time.Now().UnixMilli()

		// Insert user message + part
		msgUserID := generateID("msg")
		if err := insertMessage(db, msgUserID, activeSessionID, "user", now); err != nil {
			return err
		}
		if err := insertPart(db, generateID("prt"), msgUserID, activeSessionID, now,
			fmt.Sprintf(`{"type":"text","text":%s}`, jsonStr(prompt))); err != nil {
			return err
		}

		// Simulate processing time
		time.Sleep(50 * time.Millisecond)
		nowReply := time.Now().UnixMilli()

		// Insert assistant message + parts (step-start, text, step-finish)
		msgAsstID := generateID("msg")
		if err := insertMessage(db, msgAsstID, activeSessionID, "assistant", nowReply); err != nil {
			return err
		}
		stepStart := nowReply
		if err := insertPart(db, generateID("prt"), msgAsstID, activeSessionID, stepStart,
			`{"type":"step-start"}`); err != nil {
			return err
		}
		textTime := stepStart + 2
		if err := insertPart(db, generateID("prt"), msgAsstID, activeSessionID, textTime,
			fmt.Sprintf(`{"type":"text","text":%s,"time":{"start":%d,"end":%d}}`,
				jsonStr(reply), textTime, textTime+100)); err != nil {
			return err
		}
		finishTime := textTime + 100
		if err := insertPart(db, generateID("prt"), msgAsstID, activeSessionID, finishTime,
			`{"reason":"stop","type":"step-finish","tokens":{"total":100,"input":10,"output":10},"cost":0.001}`); err != nil {
			return err
		}

		// Print reply to stdout
		fmt.Println(reply)
		fmt.Println("> ")
	}

	return nil
}

// consumePickerResponse waits for the final Enter from a tmux key recipe and
// reports whether the recipe moved away from the default selection.
func consumePickerResponse(in *os.File) (bool, error) {
	reader := bufio.NewReader(in)
	rejected := false
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return false, err
		}
		if b == 0x1b {
			next, err := reader.ReadByte()
			if err != nil {
				return false, err
			}
			if next == '[' {
				arrow, err := reader.ReadByte()
				if err != nil {
					return false, err
				}
				if arrow == 'C' {
					rejected = true
				}
			}
			continue
		}
		if b == '\n' || b == '\r' {
			return rejected, nil
		}
	}
}

func resolveDBPath() (string, error) {
	// Honour RELAY_DEVPASS_DB if set (used when launched by dpmux in a relay test).
	if v := os.Getenv("RELAY_DEVPASS_DB"); v != "" {
		return v, nil
	}
	xdg := os.Getenv("XDG_DATA_HOME")
	if xdg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("fake-devpass-code: home dir: %w", err)
		}
		xdg = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(xdg, "devpass-code", "opencode.db"), nil
}

func applySchema(db *sql.DB) error {
	_, err := db.Exec(`
	CREATE TABLE IF NOT EXISTS project (
		id text PRIMARY KEY,
		name text NOT NULL DEFAULT ''
	);
	CREATE TABLE IF NOT EXISTS session (
		id text PRIMARY KEY,
		project_id text NOT NULL,
		workspace_id text,
		parent_id text,
		slug text NOT NULL,
		directory text NOT NULL,
		path text,
		title text NOT NULL,
		version text NOT NULL,
		share_url text,
		summary_additions integer,
		summary_deletions integer,
		summary_files integer,
		summary_diffs text,
		metadata text,
		cost real DEFAULT 0 NOT NULL,
		tokens_input integer DEFAULT 0 NOT NULL,
		tokens_output integer DEFAULT 0 NOT NULL,
		tokens_reasoning integer DEFAULT 0 NOT NULL,
		tokens_cache_read integer DEFAULT 0 NOT NULL,
		tokens_cache_write integer DEFAULT 0 NOT NULL,
		revert text,
		permission text,
		agent text,
		model text,
		time_created integer NOT NULL,
		time_updated integer NOT NULL,
		time_compacting integer,
		time_archived integer,
		CONSTRAINT fk_session_project_id_project_id_fk FOREIGN KEY (project_id) REFERENCES project(id) ON DELETE CASCADE
	);
	CREATE TABLE IF NOT EXISTS message (
		id text PRIMARY KEY,
		session_id text NOT NULL,
		time_created integer NOT NULL,
		time_updated integer NOT NULL,
		data text NOT NULL,
		CONSTRAINT fk_message_session_id_session_id_fk FOREIGN KEY (session_id) REFERENCES session(id) ON DELETE CASCADE
	);
	CREATE TABLE IF NOT EXISTS part (
		id text PRIMARY KEY,
		message_id text NOT NULL,
		session_id text NOT NULL,
		time_created integer NOT NULL,
		time_updated integer NOT NULL,
		data text NOT NULL,
		CONSTRAINT fk_part_message_id_message_id_fk FOREIGN KEY (message_id) REFERENCES message(id) ON DELETE CASCADE
	);
	CREATE TABLE IF NOT EXISTS session_message (
		id text PRIMARY KEY,
		session_id text NOT NULL,
		message_id text NOT NULL
	);
	CREATE TABLE IF NOT EXISTS session_input (
		id text PRIMARY KEY,
		session_id text NOT NULL,
		data text NOT NULL
	);
	CREATE TABLE IF NOT EXISTS todo (
		id text PRIMARY KEY,
		session_id text NOT NULL,
		data text NOT NULL
	);
	INSERT OR IGNORE INTO project(id, name) VALUES('proj-fake','fake-project');
	`)
	return err
}

func insertSession(db *sql.DB, id string) error {
	now := time.Now().UnixMilli()
	// Embed RELAY_SPAWN_NONCE in the title so the relay can verify the binding
	// is its own.  Format: "nonce:<hex>" when the env var is present; otherwise
	// the title is "Fake Session" for compatibility with tests that don't use
	// the nonce-based binding path.
	title := "Fake Session"
	if nonce := os.Getenv("RELAY_SPAWN_NONCE"); nonce != "" {
		title = "nonce:" + nonce
	}
	_, err := db.Exec(
		`INSERT INTO session(id,project_id,slug,directory,title,version,time_created,time_updated)
		 VALUES(?,?,?,?,?,?,?,?)`,
		id, "proj-fake", "fake-session", workdir(), title, devpassVersion, now, now,
	)
	return err
}

func insertMessage(db *sql.DB, id, sessionID, role string, ts int64) error {
	data := fmt.Sprintf(`{"role":%s,"time":{"created":%d}}`, jsonStr(role), ts)
	_, err := db.Exec(
		`INSERT INTO message(id,session_id,time_created,time_updated,data) VALUES(?,?,?,?,?)`,
		id, sessionID, ts, ts, data,
	)
	return err
}

func insertPart(db *sql.DB, id, messageID, sessionID string, ts int64, data string) error {
	_, err := db.Exec(
		`INSERT INTO part(id,message_id,session_id,time_created,time_updated,data) VALUES(?,?,?,?,?,?)`,
		id, messageID, sessionID, ts, ts, data,
	)
	return err
}

var idCounter int64

func generateID(prefix string) string {
	idCounter++
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixMicro(), idCounter)
}

func workdir() string {
	wd, err := os.Getwd()
	if err != nil {
		return "/tmp"
	}
	return wd
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func envMS(key string) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Millisecond
}

// drainStdin reads and discards all stdin input for the given duration.
// Used by FAKE_DP_SWALLOW_UNTIL_READY to simulate a real devpass-code process
// that ignores keystrokes typed before the ready prompt appears.
// After d elapses, the drain goroutine exits cleanly and stdin is ready for the
// scanner to use normally.
func drainStdin(d time.Duration) {
	drained := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		buf := make([]byte, 256)
		for {
			select {
			case <-drained:
				return
			default:
			}
			// Use a short deadline so we can re-check drained frequently.
			if err := os.Stdin.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
				// SetReadDeadline not supported (e.g. pipe in unit tests) – just sleep.
				select {
				case <-drained:
					return
				case <-time.After(50 * time.Millisecond):
				}
				continue
			}
			os.Stdin.Read(buf) //nolint:errcheck
		}
	}()
	time.Sleep(d)
	close(drained)
	// Wait for drain goroutine to exit before returning, so the caller's scanner
	// does not race with a lingering read.
	<-exited
	// Reset deadline for normal blocking reads by the scanner.
	os.Stdin.SetReadDeadline(time.Time{}) //nolint:errcheck
}
