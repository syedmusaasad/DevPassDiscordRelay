BINDING_PROTOCOL: VERIFIED

## Mechanism
The genuine, per-launch link from the real devpass-code TUI process to its conversation ID is established by capturing the TUI's structured logs via standard error redirection.

1. **CLI support**: `devpass-code` officially supports the `--print-logs` and `--log-level INFO` flags, which cause it to emit structural logfmt-style lines to `stderr`.
2. **Session Creation**: devpass-code does NOT create a session row in the SQLite database immediately upon launch. It waits until the user (or the injected prompt) sends the first message. When the row is created, the TUI process explicitly logs this event to stderr containing the new session ID: `message=created id=ses_<ID> ...`.
3. **Disambiguation**: By having `dpmux` redirect the TUI's stderr to a unique, per-pane log file (e.g., `2> ~/.devpass-discord-relay/stderr-<sessionName>.log`), the relay creates a mathematically sound, 1:1 binding between the TUI process and the session. It is mechanically impossible for the process to log a foreign session ID into its own exclusive stderr stream, perfectly isolating it from any concurrent launches.
4. **Resumes**: For `!resume <n>`, the exact session ID is already known and passed to the TUI via `--session <id>`. No discovery is required for resumes.
5. **No Architectural Changes**: This mechanism preserves the normal project working directory, requires no plugins, does not rely on PID-plus-time inference, and works out-of-the-box with the real installed 1.18.11 CLI.

## Sandbox Verification
Verified inside a completely isolated `XDG_DATA_HOME` environment with a mock `auth.json`:
1. Started TUI in a disposable tmux socket with:
   `devpass-code --log-level INFO --print-logs 2> stderr.log`
2. No DB row or log entry appeared initially.
3. Sent `hello` via `tmux send-keys`.
4. Extracted ID from stderr:
   `grep "message=created id=ses_" stderr.log`
5. Verified the ID perfectly matched the row in `opencode.db`.
