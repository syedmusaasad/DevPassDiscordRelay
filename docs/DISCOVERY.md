## 1. Transcript persistence
Conversations persist solely in SQLite (`~/.local/share/devpass-code/opencode.db`). There are NO per-conversation transcript files (JSONL or otherwise) generated on disk. The tables in SQLite include `session`, `message`, `part`, `session_message`, and `session_input`. A conversation ID is represented as a string starting with `ses_` (e.g. `ses_f95276072ffe7peHPCF13zXtqK`) stored in the `session` table's `id` column. Polling the SQLite DB shows rows appearing mid-turn. Because there are no discrete conversation files to watch, a poll-the-filesystem watcher design is NOT viable as-spec'd; it would need a DB-tailing adapter.
PREMISE-TRANSCRIPT-ON-DISK: FAIL

## 2. Resume
The resume flag is `-c, --continue` to continue the last session, and `-s, --session` to specify a session ID.
Attempting to resume a bogus ID with `devpass-code --session ses_doesnotexist000 2>&1` results in verbatim pane refusal:
```
Error: Session not found: ses_doesnotexist000
```
No NEW conversation ID appeared in storage after this failed resume attempt (the session count in SQLite remained at 19).

## 3. Trust flag
The auto-approve-tools flag is `--auto`.
When attempting to elicit a tool approval prompt by sending the verbatim prompt `"run \`ls\` in the current directory using your shell tool"` WITHOUT the flag, the command ran without prompting. Key hints for an approval prompt were unverified (as no prompt was shown, because the active `pl-interactive` agent explicitly allows the `bash *` pattern).

## 4. Readiness
The TUI accepts input when the pane text shows the prompt signature. At 4s after launch, the pane text signature was:
```
   ┃
   ┃  Ask anything... "What is the tech stack of this project?"
   ┃
   ┃  Pl-Interactive · Claude Fable 5.1 LLM Gateway DevPass · high
```
Keystrokes sent before this signature appeared (at 0.5s) were swallowed and ignored.

## 5. Interactive questions
Searched `--help` output, the plugin directory `~/.config/devpass-code/plugin/`, and the DB `part` tables for "ask" or "question" tools. While agent markdown files (like `pl-interactive.md`) specify permissions configured as `"ask"` (e.g. for `external_directory` or `.env` reads), we could not definitively reproduce a TUI option picker or find a dedicated question/ask tool. Thus, the exact keystroke recipe to answer interactive questions is unverified (searched help, plugins, and db parts).

## 6. Toolchain
- go: not found
- docker: not found
- podman: not found
- make: not found
- systemd-run: present (systemd 259)
- systemctl --user: running
- tmux: present (tmux 3.6)
- git: present (git version 2.53.0)
- GOFLAGS/proxy: works (curl to https://proxy.golang.org returned HTTP/2 200)

DEVPASS_CODE_VERSION: 1.18.11
