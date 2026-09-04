# Plan 001: DevPassDiscordRelay built in six milestones on a DB-tailed transcript
WORKDIR: /root/devpass-discord-relay

DECISION transcript-source: operator chose (b) after docs/DISCOVERY.md reported
PREMISE-TRANSCRIPT-ON-DISK: FAIL. The transcript stream is devpass-code's SQLite store
(~/.local/share/devpass-code/opencode.db, WAL mode) tailed READ-ONLY. "New transcript
file" in the spec means "new row in the session table created after spawn start";
"tail the transcript" means poll message/part rows for that session id. The
transcript-vs-pane split is unchanged: content from the DB, prompts/readiness/refusal
from capture-pane. Never invert it.
DECISION toolchain-install: box had no go/docker/make (DISCOVERY.md §6). Phase 1 installs
Go (official tarball to /usr/local/go), make, and docker.io so the spec's static binary
and Docker integration suite are buildable. Nothing else is installed system-wide.
DECISION sqlite-driver: modernc.org/sqlite (pure Go) so the binary stays static
(CGO_ENABLED=0). Open with URI file:<path>?mode=ro; never write to opencode.db.
DECISION discovered-facts: devpass-code 1.18.11. Trust flag `--auto`. Resume `--session
<id>`. Refusal pane text `Error: Session not found: <id>` (no new session row is
created). Readiness signature: the pane contains `Ask anything...`. Keys sent before it
are swallowed. Conversation ids are `ses_<...>`.
DECISION host-swap: phase 4 burned 3 attempts, every one an OOM SIGKILL (3.8 GB RAM, no
swap, other sessions on the host). Operator approved a 4 GB /swapfile (persisted in
/etc/fstab) and `go test -p 1` in the Makefile; neither weakens a test. Run resumed.
DECISION publish-target: operator asked for the finished repo to be pushed to GitHub as
DevPassDiscordRelay under account syedmusaasad. Neither gh nor an SSH key for
github.com is present on this box, so the operator supplies credentials via the gate.

## Phase 1: toolchain (fast-worker)
TIMEOUT: 1200
EXIT: /usr/local/go/bin/go version | grep -q 'go1.2'
EXIT: command -v make && command -v docker
EXIT: docker info >/dev/null 2>&1
EXIT: test -s docs/TOOLCHAIN.md

Install: Go from https://go.dev/dl/ (latest stable linux-amd64 tarball, verify sha256
from the download page's checksum, extract to /usr/local/go), and `apt-get install -y
make docker.io` (then `systemctl enable --now docker`). Add
`/etc/profile.d/golang.sh` exporting PATH=$PATH:/usr/local/go/bin. Write
docs/TOOLCHAIN.md: versions installed and the exact commands used. Idempotent: if a
tool is already present at an acceptable version, skip it and say so. Do not commit.

## Phase 2: transcript schema and prompt recipes (researcher)
AFTER:
TIMEOUT: 1800
EXIT: test -s docs/TRANSCRIPT-SCHEMA.md
EXIT: test -s testdata/fixtures/pong-session.json
EXIT: grep -q '^## Approval prompt' docs/TRANSCRIPT-SCHEMA.md
EXIT: grep -q '^## Question picker' docs/TRANSCRIPT-SCHEMA.md
EXIT: grep -q '^## Turn boundaries' docs/TRANSCRIPT-SCHEMA.md
EXIT: ! tmux ls 2>/dev/null | grep -q '^dp-discovery-'

Read docs/DISCOVERY.md first. Write docs/TRANSCRIPT-SCHEMA.md, the contract the DB
adapter and the fake CLI are both built to. Read the DB only via python3 sqlite3 with
URI `file:/root/.local/share/devpass-code/opencode.db?mode=ro`.

Required sections (exact headings):
`## Tables` — full `CREATE TABLE` text of session, message, part, session_message,
session_input and any table referencing a session id. Column meanings; which columns
carry timestamps (units) and monotonic ordering; where parent/subagent relation lives
(needed to ignore subagent sessions).
`## Event mapping` — for each spec event type (user message, assistant text, tool call,
tool result, turn complete) the exact row shape: role value, part type value, JSON
field names, with one verbatim example row each taken from the DB. How harness- or
agent-injected preamble/system parts appear in a user message (these are what invariant
2 must parse per PART).
`## Turn boundaries` — what marks assistant-turn completion (a column on message? a
part type? finish reason?), observed by polling the DB every 250 ms during a live pong
turn in a disposable tmux session; include the poll log showing rows appearing mid-turn.
`## Approval prompt` — run devpass-code in a disposable tmux session with
XDG_CONFIG_HOME=/tmp/devpass-code/discovery/xdg-config and XDG_DATA_HOME=
/tmp/devpass-code/discovery/xdg-data (copy auth.json into the data dir if needed; never
edit the real config), with a config whose permission for bash is "ask" and no
`--auto`. Prompt: "run `ls` using your shell tool". Capture the approval prompt
verbatim with its key hints, and the keystrokes that approve / deny. Then confirm
`--auto` suppresses it. Record how the pending approval appears (or not) in the DB.
`## Question picker` — with the same isolated config, prompt: "ask me to choose between
apple and pear using your question tool, then stop". Capture the picker verbatim, its
DB representation, and the keystroke recipe to select option 2. If no picker exists in
1.18.11, state that with the evidence.
`## Isolation` — confirm XDG_DATA_HOME relocates opencode.db (path observed). This is
what the test suite will rely on to never touch the real HOME.

Also write testdata/fixtures/pong-session.json: a JSON dump (all rows, all columns) of
one complete real session that contains a user prompt, a tool call+result, and
assistant text. Kill every dp-discovery-* tmux session before finishing. Do not
commit. Final lines: the two paths written.

## Phase 3: scaffold, fake CLI, mock Discord, harness (implementer)
AFTER: 1, 2
TIMEOUT: 3600
ATTEMPTS: 3
EXIT: test -f go.mod && test -x dpmux && test -f Makefile && test -f docker-compose.test.yml
EXIT: make unit
EXIT: make integration
EXIT: git log -1 --format=%s | grep -qxF 'scaffold: module, fake devpass-code, mock Discord, test harness'

PATH must include /usr/local/go/bin. `git init` if needed; .gitignore covers bin/, .env,
*.db*. Module `github.com/devpass/devpass-discord-relay`, CGO_ENABLED=0, deps:
github.com/bwmarrin/discordgo, modernc.org/sqlite.

Layout: cmd/relay (watcher), cmd/fake-devpass-code (test double), cmd/mock-discord
(test double), internal/transcript (DB adapter: read-only tail, per docs/
TRANSCRIPT-SCHEMA.md), internal/tmuxctl, internal/discord, internal/state, dpmux (bash),
test/integration (Go tests tagged `integration` run inside Docker), testdata/.

Fake devpass-code: mimics 1.18.11 to the letter of docs/DISCOVERY.md and
docs/TRANSCRIPT-SCHEMA.md: writes the same SQLite schema under $XDG_DATA_HOME, renders a
pane whose ready signature is `Ask anything...`, honors `--auto`, `--session <id>`
(refusal text `Error: Session not found: <id>`), reads a script of turns from
$FAKE_DP_SCRIPT (JSON), fault knobs via env: FAKE_DP_REFUSE_FIRST_RESUME=1,
FAKE_DP_READY_DELAY_MS, FAKE_DP_TRANSCRIPT_DELAY_MS (delay the session row),
FAKE_DP_SWALLOW_UNTIL_READY=1. Version-pin comment in its header.

Mock Discord: gateway websocket (HELLO/IDENTIFY/READY/heartbeat/dispatch MESSAGE_CREATE
and INTERACTION_CREATE) + REST (create message, edit, create thread, typing, upload,
rate-limit injection on demand) with a control HTTP endpoint tests use to inject events
and read everything the relay sent. discordgo must connect to it via overridable
endpoint vars in internal/discord.

Makefile targets: `build` (static binary to bin/relay), `unit` (`go test ./...`),
`integration` (`docker compose -f docker-compose.test.yml run --rm test`; honors
`T=<name>` → `-run`), `test` (= unit + integration). The integration image has tmux,
runs as a non-root user with a throwaway HOME, never mounts the developer's HOME.
Ship one passing smoke integration test (fake CLI starts in tmux, ready signature
appears, session row appears) so the harness is proven. Commit exactly:
`scaffold: module, fake devpass-code, mock Discord, test harness`.

## Phase 4: milestone 1, mirror only (implementer)
TIMEOUT: 3600
ATTEMPTS: 3
REVIEW: cross
EXIT: make test
EXIT: grep -rq 'func TestInvariant2_FirstSightReplay' test/integration
EXIT: git log -1 --format=%s | grep -qxF 'm1: mirror terminal sessions into Discord threads'

dpmux: `dpmux [args]` → session `dp-<unixtime>-<pid>` on the default per-uid socket,
passes `--auto` unless `--untrusted` appears anywhere in args; writes
~/.devpass-discord-relay/pending-<session-name> containing the tmux target before
spawning; inside a pane on the same default socket → new-window in the host session,
target `<host-session>:=<window-name>`; other socket → fresh default-socket session.
`dpmux ls` (suppress window lines duplicating their session name). Relay binary path,
DB path, state dir all overridable by env (RELAY_DEVPASS_DB, RELAY_STATE_DIR).

Watcher: config from .env resolved $DEVPASS_RELAY_ENV → ~/.config/devpass-discord-relay/
.env → ./.env. Polls the DB ~1s for new session rows; pairs with pending markers
(unmatched sessions ignored, subagent sessions ignored); creates one thread per session
in the configured channel; tails message/part rows; streams assistant text via
throttled edits (≤5 edits/5s per channel, 2000-char chunking at natural boundaries,
429 backoff); echoes terminal-typed user prompts truncated to 300 chars; one tool
embed updated in place (rows flip done/failed); typing loop re-armed under 10s;
"turn finished without output" line when a turn ends with no text; tool error then
clean turn end softened in place; muted summary line (duration · tool count).
Liveness polls the full stored target; on death post a session-ended marker, flip the
thread-root status emoji, keep the sessions.json claim. sessions.json in the state dir.
First-sight replay: a completed first exchange already in the DB at attach still
mirrors; preamble parts are classified per PART, never on joined text.

Tests: unit for chunking, throttle, part classification (a preamble-bearing user
message must not count as a second prompt); integration `TestInvariant2_FirstSightReplay`
(fake CLI completes a full turn before the relay starts; the thread shows it) plus
mirror tests for the embed flip, no-output line, and softening. Commit exactly:
`m1: mirror terminal sessions into Discord threads`.

## Phase 5: milestone 2, inject into a live session (implementer)
TIMEOUT: 2400
ATTEMPTS: 3
EXIT: make test
EXIT: grep -rq 'func TestInject_ReadinessProbe' test/integration
EXIT: grep -rq 'func TestInject_ForeignAuthorDropped' test/integration
EXIT: git log -1 --format=%s | grep -qxF 'm2: inject Discord thread replies into the live pane'

Thread reply → readiness probe (capture-pane for `Ask anything...` up to 30s, else
fixed 2s fallback) → `tmux send-keys -t <target> -l <text>` then Enter. Only
DISCORD_USER_ID's messages are honored; all other authors including bots dropped in
the handler before any work. `!title <t>` renames the thread. Image attachments
download to <state>/staging/<session-id>/ with filenames sanitized of exotic whitespace;
the local path is appended to the injected text. Agent-emitted image parts upload to the
thread. Tests: `TestInject_ReadinessProbe` (FAKE_DP_SWALLOW_UNTIL_READY=1 +
FAKE_DP_READY_DELAY_MS=3000; the prompt still lands exactly once),
`TestInject_ForeignAuthorDropped`, an attachment path test. Commit exactly:
`m2: inject Discord thread replies into the live pane`.

## Phase 6: milestone 3, spawn from Discord (implementer)
AFTER: 13
TIMEOUT: 2400
ATTEMPTS: 3
REVIEW: cross
EXIT: make test
EXIT: grep -rq 'func TestInvariant1_OwnTranscriptBinding' test/integration
EXIT: git log -1 --format=%s | grep -qxF 'm3: spawn a new session from a Discord message'

Message in the relay channel outside any thread → spawn `dpmux` (systemd-run wrapping
comes in Phase 9; leave a TODO comment naming Phase 9) → wait for a session row whose
creation time is after the spawn's start instant AND matching this spawn's pending
marker → bind → inject as first prompt. Never glob/lexicographic; never a
process-start baseline snapshot. Test `TestInvariant1_OwnTranscriptBinding`: a stale
session row whose id sorts first and was created after relay start but before the
spawn must lose to the genuinely new one. Commit exactly:
`m3: spawn a new session from a Discord message`.

## Phase 7: milestone 4, resume and refusal handling (implementer)
TIMEOUT: 3600
ATTEMPTS: 3
REVIEW: cross
EXIT: make test
EXIT: grep -rq 'func TestInvariant3_InjectionEchoVerification' test/integration
EXIT: grep -rq 'func TestInvariant4_ResumeRefusalRetry' test/integration
EXIT: grep -rq 'func TestInvariant4_AmbiguousIdCandidateList' test/integration
EXIT: git log -1 --format=%s | grep -qxF 'm4: resume dead sessions, retry refusals, verify injection echo'

Reply in a thread whose session died → re-spawn with `--session <id>`, queue the
message, inject after readiness, then watch the DB ≤30s for the echo; if absent,
re-send the identical keystrokes exactly once into the same window and suppress the
duplicate echo in the thread. Refusal detection by BOTH signals: pane contains
`Error: Session not found:` OR the first new session row carries an id other than the
requested one. On refusal: teardown exactly like `!quit` (kill -TERM the pane's process
group, then kill-window), retry the same resume once silently; second failure → one
explicit notice that a fresh session was started; never silently bind to the wrong id.
Ambiguous id → numbered candidate list (provider + last-active age), wait for
`!resume <n>`. `!quit` tears down but keeps the claim. Tests named in the EXITs, using
FAKE_DP_REFUSE_FIRST_RESUME=1 and a swallowed-first-input script. Commit exactly:
`m4: resume dead sessions, retry refusals, verify injection echo`.

## Phase 8: milestone 5, approvals and questions (implementer)
TIMEOUT: 3600
ATTEMPTS: 3
REVIEW: cross
EXIT: make test
EXIT: grep -rq 'func TestApproval_AutoDenyTimeout' test/integration
EXIT: grep -rq 'func TestInvariant5_RecipesVersionPinned' internal
EXIT: grep -rq 'verified against devpass-code 1.18.11' internal/tmuxctl
EXIT: git log -1 --format=%s | grep -qxF 'm5: approval buttons, question menus, auto-deny timeout'

Pane-detected approval prompt (text per docs/TRANSCRIPT-SCHEMA.md §Approval prompt) →
Discord message with Approve/Deny buttons; click drives the pinned keystrokes; unanswered
→ auto-deny after APPROVAL_TIMEOUT_SECONDS (default 300) and the message is edited to
say so. Question pickers (if §Question picker found one; otherwise implement against
the fake CLI's picker and mark the recipe `unverified against real TUI` in the same
comment) → select menu. Every keystroke recipe lives in internal/tmuxctl/recipes.go,
each with a comment containing `verified against devpass-code 1.18.11`;
`TestInvariant5_RecipesVersionPinned` (unit) parses that file and fails if any recipe
lacks the pin. Commit exactly:
`m5: approval buttons, question menus, auto-deny timeout`.

## Phase 9: milestone 6, restart survival, install, lifecycle (implementer)
TIMEOUT: 3600
ATTEMPTS: 3
REVIEW: cross
EXIT: make test
EXIT: grep -rq 'func TestRestartSurvival_DisposableUnit' test/integration
EXIT: grep -rq 'func TestTTL_NeverSweepsLiveWindow' internal/state
EXIT: bash -n install.sh && bash -n dpmux && test -s systemd/devpass-discord-relay.service
EXIT: grep -q '%h' systemd/devpass-discord-relay.service
EXIT: git log -1 --format=%s | grep -qxF 'm6: restart survival, TTL sweep, telemetry, installer, unit file'

Wrap every tmux invocation that may birth the server (new-session, new-window when
no server) in `systemd-run --user --scope --collect`; read-only commands unwrapped;
log the isolation mode at startup; clean fallback when systemd-run or the user manager
is absent. Binary resolution: RELAY_TMUX_BIN → /usr/local/bin/tmux if executable and
`-V` succeeds → PATH; never bare PATH-first; same pattern for devpass-code
(RELAY_DEVPASS_BIN). `TestRestartSurvival_DisposableUnit`: inside the integration
container (or skipped with an explicit reason if no user manager is reachable, and then
ALSO provided as `make restart-survival-host` running against a disposable
`systemd-run --user --unit relay-test-<pid>` on the host with an isolated
XDG_DATA_HOME): start relay as a unit, spawn a session, restart the unit, the tmux
window is still alive. sessions.json TTL sweep at boot and every 24h,
SESSION_TTL_DAYS default 30, never removes a record whose window is alive
(`TestTTL_NeverSweepsLiveWindow`). Telemetry: daily POST {sha256(username)[:12],
version} to TELEMETRY_URL if set; quiet on failure; nothing else.
install.sh: idempotent (done steps exit success), builds, installs binary + dpmux to
~/.local/bin, unit to ~/.config/systemd/user/ using %h, backs up and restores .env,
enables the service. `dpmux update` (git pull to tag `latest`, rebuild, prompt to
restart) and `dpmux uninstall` (backs up .env). Commit exactly:
`m6: restart survival, TTL sweep, telemetry, installer, unit file`.

## Phase 10: operator documents (document-writer)
TIMEOUT: 1800
SURFACE: README.md operator-doc
SURFACE: SETUP.md operator-doc
REVIEW: cross
EXIT: test -s README.md && test -s SETUP.md && test -s CONTRIBUTING.md && test -s CHANGELOG.md
EXIT: grep -q 'capture-pane' README.md && grep -q 'opencode.db' README.md
EXIT: grep -qi 'MESSAGE CONTENT' SETUP.md
EXIT: grep -q 'make test' CONTRIBUTING.md && grep -q 'T=' CONTRIBUTING.md
EXIT: git log -1 --format=%s | grep -qxF 'docs: README, SETUP, CONTRIBUTING, CHANGELOG'

Sources: the code, docs/DISCOVERY.md, docs/TRANSCRIPT-SCHEMA.md, the plan preamble
DECISIONs. README.md: one-paragraph philosophy (tmux session is the source of truth,
Discord is mirror + remote), an ASCII architecture diagram, an explicit
"Transcript vs pane" section (DB-tailed content vs capture-pane state-only, and why the
transcript is opencode.db on this version), the Discord structural difference (bots
cannot thread in DMs → your own private guild, privacy consequence), and a "Security
posture" section with the spec's points verbatim in substance: runs on your machine
under your account, per-user token in .env never committed and backed up across
reinstalls, single-user gating at the event layer, outbound only, content goes only to
your own private guild. SETUP.md: developer-portal walkthrough (application, bot,
token, MESSAGE CONTENT intent, invite to your guild, channel id, user id, .env keys).
CONTRIBUTING.md: `make unit`, `make integration`, `make test`, `T=<name>` filter, the
fake CLI knobs, the mock Discord control endpoint, the rule that tests never touch the
real HOME or real Discord. CHANGELOG.md: 0.1.0 with the six milestones. Stage only these
four files; commit exactly: `docs: README, SETUP, CONTRIBUTING, CHANGELOG`.

## Phase 11: ready to publish (gate)
GATE: .gate-publish

## Phase 12: publish to GitHub (implementer)
TIMEOUT: 900
EXIT: git remote get-url origin | grep -qE 'github.com[:/]syedmusaasad/DevPassDiscordRelay(\.git)?$'
EXIT: git fetch -q origin && git diff --quiet HEAD origin/main
EXIT: git ls-remote --tags origin | grep -q 'refs/tags/latest$'

Read the credential file /root/devpass-discord-relay/plans/001-build/.gate-publish: line
1 is `token=<GitHub PAT with repo scope>`. Never echo, log, or commit the token; never
write it into any file under the repo. Confirm the working tree is clean and
`make unit` passes before pushing. Create the repository if it does not exist:
`curl -sf -H "Authorization: Bearer $TOKEN" https://api.github.com/user/repos -d
'{"name":"DevPassDiscordRelay","private":false}'` (a 422 "already exists" is success).
Set `git remote add origin https://github.com/syedmusaasad/DevPassDiscordRelay.git`
(or set-url if origin exists), ensure the branch is named `main`, push via a one-shot
credential helper (`git -c credential.helper='!f() { echo username=syedmusaasad; echo
password=$TOKEN; }; f' push -u origin main`), then tag the HEAD `latest` (this is what
`dpmux update` pulls) and push the tag. Afterwards `shred -u` the gate file's token
line is NOT permitted (the engine owns the sentinel); instead leave it and write
nothing else. Final lines: `git ls-remote --tags origin`.

## Phase 13: m1 review remediation (implementer)
AFTER: 5
TIMEOUT: 2400
ATTEMPTS: 3
REVIEW: cross
EXIT: make test
EXIT: grep -rq 'func TestMirror_LongTextChunksToFollowups' test/integration
EXIT: grep -rq 'func TestMirror_ToolErrorSoftenedOnCleanEnd' test/integration
EXIT: grep -rq 'func TestThrottle_429Backoff' internal
EXIT: grep -rq 'func TestDpmux_MarkerBeforeSpawn' test/integration
EXIT: git log -1 --format=%s | grep -qxF 'm1-fix: chunk long text, soften tool errors, honor 429, marker before spawn'

Cross review of phase 4 (verdicts in /root/.system/runs/run_20260904T054447_438b65/
phase-4/review/reviewer-a/review.md and reviewer-b/review.md; read both) found spec gaps
that landed. Fix each, with the named test:
1. internal/mirror/watcher.go truncates assistant text to one 2000-char chunk. Post ALL
   chunks: first message edited in place while streaming; when it would exceed 2000,
   split at the last paragraph/sentence/space boundary and continue in a follow-up
   message that becomes the new edit target. `TestMirror_LongTextChunksToFollowups`
   (fake CLI emits a 5000-char reply; the thread holds >=3 messages whose concatenation
   equals the reply).
2. Tool error followed by a clean step-finish is never softened. Implement in-place:
   error row flips to a warning glyph and the embed footer reads "turn completed".
   `TestMirror_ToolErrorSoftenedOnCleanEnd`.
3. 429 from Discord is treated as an ordinary error. Parse retry_after from the
   discordgo RateLimitError / response body, sleep it, retry the same edit/send once;
   feed the throttle. `TestThrottle_429Backoff` (unit, mock returning one 429 then 200).
4. cmd/dpmux writes pending-<name> AFTER tmux new-session. Write it BEFORE spawning
   (the brief and the spec say so). `TestDpmux_MarkerBeforeSpawn` (integration: run
   dpmux with RELAY_DEVPASS_BIN pointing at a stub that records whether the marker
   existed at its start).
5. cmd/dpmux inside-pane detection: only use new-window when $TMUX names the default
   per-uid socket (/tmp/tmux-<uid>/default or $TMUX_TMPDIR equivalent); any other
   socket → fresh session on the default socket.
6. Liveness must check the full target: `tmux list-panes -t <target>` (window-level),
   not `has-session`.
7. `dpmux ls`: list only dp-* sessions and their windows, suppressing window lines that
   duplicate their session name. Drop the `_ = strconv.Itoa` hack.
Do not touch phase-5 (inject) behavior beyond what these fixes require. Commit exactly:
`m1-fix: chunk long text, soften tool errors, honor 429, marker before spawn`.
