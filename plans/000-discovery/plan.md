# Plan 000: DevPassDiscordRelay Phase 0 discovery, gated before any build
WORKDIR: /root/devpass-discord-relay

DECISION discovery-first: the spec makes Phase 0 a hard gate ("stop and report if a
premise fails"). Pre-check shows devpass-code 1.18.11 keeps state in
~/.local/share/devpass-code/opencode.db (SQLite) with no transcript directory visible,
and the box has no go or docker. Milestones 1-6 are NOT planned until discovery lands
and the operator rules on the premises.
DECISION transcript-source: operator chose (b) after DISCOVERY.md reported
PREMISE-TRANSCRIPT-ON-DISK: FAIL. The transcript stream is opencode.db (SQLite, WAL)
tailed read-only; the transcript-vs-pane split is unchanged, only the transcript's
physical form. Gate .gate-discovery written on that ruling.

## Phase 1: discovery (researcher)
TIMEOUT: 1800
EXIT: test -s docs/DISCOVERY.md
EXIT: grep -q '^## 1. Transcript persistence' docs/DISCOVERY.md
EXIT: grep -q '^## 2. Resume' docs/DISCOVERY.md
EXIT: grep -q '^## 3. Trust flag' docs/DISCOVERY.md
EXIT: grep -q '^## 4. Readiness' docs/DISCOVERY.md
EXIT: grep -q '^## 5. Interactive questions' docs/DISCOVERY.md
EXIT: grep -q '^## 6. Toolchain' docs/DISCOVERY.md
EXIT: grep -qE '^PREMISE-TRANSCRIPT-ON-DISK: (PASS|FAIL)$' docs/DISCOVERY.md
EXIT: grep -q '^DEVPASS_CODE_VERSION: ' docs/DISCOVERY.md
EXIT: ! tmux ls 2>/dev/null | grep -q '^dp-discovery-'

Write /root/devpass-discord-relay/docs/DISCOVERY.md (create the directory). Every claim
sourced: a path, a command and its verbatim output, or a pane capture. Unverified items
are marked "unverified".

Subject: `devpass-code` at /usr/local/bin/devpass-code (`devpass-code --version` prints
1.18.11; record it as the line `DEVPASS_CODE_VERSION: <version>`). Known state paths to
inspect: ~/.local/share/devpass-code/ (opencode.db, opencode.db-wal, snapshot/, log/),
~/.config/devpass-code/, ~/.cache/. `sqlite3` CLI is absent; use
`python3 -c 'import sqlite3; ...'` opening with URI `file:...?mode=ro` (read-only,
never write to the DB). Also check `~/.config/devpass-code/plugin/` and
`devpass-code --help` for any transcript/export/session-list options.

Observation protocol (only way to run the TUI): tmux session named
`dp-discovery-<pid>` in cwd /tmp/devpass-code/discovery (mkdir it). Drive it with
`tmux send-keys` and read it with `tmux capture-pane -p`. Snapshot
`find ~/.local/share/devpass-code -newer <stamp>` and the DB row counts before and after
each experiment so you can say exactly what appeared on disk. Send one trivial prompt
("reply with the single word pong") and observe. `tmux kill-session` on every
dp-discovery-* session before you finish; the last EXIT checks none remain.

Sections, exact headings:

`## 1. Transcript persistence` — where conversations persist; whether ANY per-conversation
file (JSONL or otherwise) exists on disk vs. SQLite-only. If SQLite: the table list, the
schema of message/part/session tables, whether rows appear mid-turn (poll the DB while
the pong turn runs), and how a conversation id is represented. State whether a
poll-the-filesystem watcher design is viable as-spec'd or would need a DB-tailing
adapter. Then one line exactly `PREMISE-TRANSCRIPT-ON-DISK: PASS` or
`PREMISE-TRANSCRIPT-ON-DISK: FAIL` (FAIL = no per-conversation transcript file).

`## 2. Resume` — the resume flag/subcommand (from --help and a real attempt on the pong
conversation's id). Then attempt resume with a bogus id `ses_doesnotexist000`: verbatim
pane text of the refusal, and whether a NEW conversation id appeared in storage.

`## 3. Trust flag` — the auto-approve-tools flag or config key. Attempt to elicit a tool
approval prompt (prompt: "run `ls` in the current directory using your shell tool")
WITHOUT the flag and capture the prompt verbatim, including the key hints shown.

`## 4. Readiness` — pane text signature when the TUI accepts input (capture at 0.5s,
1s, 2s, 4s after launch; note the first capture that shows the prompt). Test whether
keys sent before that are swallowed.

`## 5. Interactive questions` — whether the TUI renders option pickers (search --help,
plugin dir, and the DB parts for a question/ask tool type); if reproducible, the
keystroke recipe. Otherwise mark unverified with what was searched.

`## 6. Toolchain` — presence/version of: go, docker, podman, make, systemd-run,
`systemctl --user` (does a user manager run for root here?), tmux (3.6 expected),
git. Note whether `GOFLAGS`/proxy access works (`curl -sI https://proxy.golang.org`).
Do not install anything.

Do not create, modify, or commit anything outside docs/DISCOVERY.md and
/tmp/devpass-code/discovery. Final lines: the path written and the PREMISE line.

## Phase 2: operator rules on premises (gate)
GATE: .gate-discovery
