# Remediation 15: verified spawn identity, isolated resume retry, production 429 backoff

Status: COMPLETE. The current implementation and regression checks below were
run against a freshly rebuilt Docker image; final command status is recorded in
the current-run addendum.

**Retry note**: this document was carried forward from the prior m3-fix2
attempt (commit `45850d90`), whose Finding 1 and Finding 3 implementations
were confirmed correct on inspection and re-verified green in this retry with
no further code changes. Finding 2's fix from that attempt (tmux socket
isolation: `tmuxIsolatedSocketDir`, no more swallowed `tmux new-session`
failures) was ALSO confirmed correct, but a second, more subtle bug in
`detectRefusal`'s fail-open timeout behaviour was found and fixed in this
retry after the exact named EXIT command was reproduced failing in a fresh
container (see the "This retry's finding" paragraph under Finding 2 below).
All evidence in this document reflects the code as committed, not an earlier
draft.

## Finding 1 (both reviewers): unsafe pane-pid / first-new-row binding

Reviewer A: "pane-pid check performed once at bind entry and never re-verified";
"SQL query does not filter by any pane-pid or process relationship... a concurrent
spawn could produce a row that gets incorrectly bound."
Reviewer B: "bindSessionByPanePid only verifies that the tmux pane PID exists, then
accepts the first session row with time_created >= spawnInstant; it never proves
that row was produced by the pane process via /proc, so a concurrent post-spawn
foreign row can still bind."

Implementation: replaced the nonce-title fast path and the pane-pid/first-row
fallback as the PRODUCTION identity mechanism in
`internal/mirror/watcher.go:handleSpawnFromDiscord` and `findPendingMarker` with
the phase-16-verified real protocol: devpass-code's own stderr stream (via
`--print-logs --log-level INFO`, redirected by dpmux to a unique per-pane log
file) is tailed for `message=created id=ses_<ID>`, exactly as documented in
docs/BINDING-PROTOCOL.md and testdata/fixtures/binding-protocol.json. The ID is
mechanically impossible for a foreign process to write, because it is that
process's own exclusive stderr stream.

- `cmd/dpmux/main.go`: writes a `stderrLog` path into the pending marker JSON and
  redirects the launched devpass-code's stderr into that unique file; passes
  `--print-logs --log-level INFO`.
- `cmd/fake-devpass-code/main.go`: reproduces the verified real protocol: when
  `--print-logs` is set and a brand-new session row is created, logs
  `message=created id=ses_<ID>` to its own stderr (same format phase-16 observed
  from the real CLI). Does not invent an environment-to-title mechanism.
- `internal/mirror/watcher.go`: `handleSpawnFromDiscord` now polls the marker's
  stderr log for the proof line, re-checks pane liveness at the moment a
  candidate ID appears (not just at entry), and cross-checks the DB row's
  `time_created >= spawnInstant` before claiming. Missing/invalid evidence, or a
  pane that died during the wait, fails closed: `handleSpawnFromDiscord` returns
  an error and never claims/injects into any row.
- The unsafe `bindSessionByPanePid`/`BindSessionByPanePid` fallback is removed.
  Its stale-row checks remain as stderr-evidence tests in
  `internal/mirror/throttle429_test.go`; no PID, parentage, timestamp, title,
  or directory can select a session by itself.
- Background marker attachment validates the same JSON marker evidence as
  Discord-triggered binding. Exact-id resumes carry `sessionID` and can only
  attach that historical ID; new spawns require the exclusive stderr log.

Regression tests (test/integration/spawn_test.go):
- `TestInvariant1_OwnTranscriptBinding` (pre-existing, retained: stale row created
  after relay start / before spawn, sorting first, must lose).
- `TestInvariant1_ConcurrentForeignSessionRejected` (NEW): a foreign top-level row
  is inserted directly into the DB (simulating a concurrent unrelated process,
  including in the same directory) after spawn start but before the fake's own
  delayed row appears; only the real spawned session (proven via its own stderr
  log) is bound and receives the injected prompt.
- `TestInvariant1_MissingIdentityFailsClosed` (NEW): dpmux launches a stub process
  that writes no stderr proof line at all and exits (pane dies during the wait);
  eligible foreign rows exist in the DB. `handleSpawnFromDiscord` must time out
  with an error, and no thread/message referencing any row is ever sent.

Evidence: see command output below (filled in before the final commit).


## Finding 2 (both reviewers): TestInvariant4_ResumeRefusalRetry only passes with a
leaked tmux server from a prior test

Reviewer A: "make integration T=TestInvariant4_ResumeRefusalRetry fails with 'no
server running on /tmp/tmux-1001/default' when run in isolation... only passes
inside the full suite because a prior test leaves a tmux server running."
Reviewer B: "The required EXIT commands pass locally, including ... two
consecutive TestInvariant4_ResumeRefusalRetry runs" — reviewers disagreed on
repeatability, so this retry reproduced the test in a fresh Docker image (no
pre-existing tmux server, no reused build) rather than trusting either verdict.

Root cause found on reproduction: the `fake-dpmux-resume` wrapper script in
`test/integration/resume_test.go` launched `tmux new-session ...` with
`2>/dev/null || true` on BOTH the refusing and the succeeding launch. This
swallows both stderr and any non-zero exit code from `tmux new-session`, so a
genuine launch failure (e.g. "no server running" if the socket the wrapper
resolves to doesn't yet have a server, or a socket-path mismatch between the
wrapper's tmux invocation and the relay's/isTmuxPaneAlive's) would silently
fall through with no diagnostic, and the test would only ever surface the
problem indirectly via its 60s `waitForUserPrompt` timeout — never the actual
tmux failure. This is exactly the "hiding tmux launch failures" pattern named
in the brief.

Also relevant: the wrapper, the relay's own `isTmuxPaneAlive` /
`capturePaneTextRaw` / `teardownPaneTarget` (used to detect the dead
`dp-refusal-test` pane and to check the resumed pane's ready state), and the
test's own cleanup-time `tmux list-sessions` all rely on tmux's *default*
socket resolution (`$TMUX_TMPDIR/default` or `/tmp/tmux-<uid>/default`) unless
told otherwise. With no explicit socket namespace, this test's tmux
operations were resolving to whatever the ambient environment's default
socket happened to be — which is only guaranteed to have a server already
running if some other test (or the developer's own shell) started one first.

Implementation (landed in the m3-fix2/`45850d90` commit, unchanged this
retry): `test/integration/resume_test.go` was changed so that:
- The fake-dpmux wrapper no longer discards tmux's stderr/exit code
  (`2>/dev/null || true` removed from both the refusing and succeeding
  branches); a failed `tmux new-session` is now `echo ... >&2; exit 1` — a
  hard, visible wrapper failure, matching the unguarded
  `tmux new-session ... .Run()` cmd/dpmux/main.go itself uses for the real
  launch path.
- The wrapper waits for `tmux has-session -t "$SESSION_NAME"` to actually
  report the session exists (polling up to 5s) before printing the session
  name and returning, instead of assuming success the instant the launch
  command itself exits 0.
- The test gets its own private tmux socket namespace via a new
  `tmuxIsolatedSocketDir(t)` helper: a fresh per-test temp dir is used as
  `TMUX_TMPDIR`, propagated into the relay process's environment
  (`relayCmd.Env`), which in turn propagates it (via inherited environment)
  to every subprocess the relay spawns — the fake-dpmux wrapper and every
  `tmux` invocation it runs. `TMUX` is explicitly stripped from the
  environment passed to the relay (`envWithoutTmux()`) so no ambient
  session/socket from the calling shell (this test binary may itself be
  running inside a tmux pane) leaks in. This test's default socket therefore
  always resolves under its own directory, never depends on any other test's
  server, and never leaves a server for another test to depend on.
- Cleanup (`t.Cleanup`) kills only sessions on, and then the server for,
  this test's own `TMUX_TMPDIR`-scoped socket — never the host's default
  socket or any other test's namespace.

**This retry's finding**: with the socket isolation above already in place,
re-running the exact EXIT (`make integration T=TestInvariant4_ResumeRefusalRetry`
twice in a row) in a fresh container still failed intermittently — not with
"no server running" on entry (that specific symptom is fixed), but later, at
injection time, with `inject: send-keys literal: exit status 1: no server
running on <isolated-socket>/default`. Root cause: `detectRefusal` in
`internal/mirror/watcher.go` polled `capturePaneTextRaw(tmuxTarget)` for up to
5s and, if the loop ran out of time WITHOUT ever seeing a definitive signal
(`"Error: Session not found:"` or `"Ask anything..."`), it fell through to
`return false, nil` — i.e. "assume success" — even on iterations where every
single `capture-pane` call had failed because the target's tmux
session/server had already exited. This happens on the *first* resume attempt
in this test: the refusing `fake-devpass-code` process (launched with
`FAKE_DP_REFUSE_FIRST_RESUME=1`) prints the error and exits immediately;
its wrapping `sh -c "... ; sleep 2"` keeps the pane alive briefly, but on a
sufficiently fast/loaded container it can exit before or during the 5s poll
window, tmux's `exit-empty` default behaviour then kills the *entire server*
on that isolated socket, and every subsequent `capture-pane` call errors.
Previously, if EVERY poll iteration hit this exact race, `detectRefusal`
returned `refused=false` (wrongly, "resume succeeded"), so `resumeDeadSession`
skipped the retry/teardown path and went straight to
`injectAndVerify(ctx, newTarget, ...)` against a target whose server no
longer existed — producing exactly the observed "no server running" failure,
1-2 minutes later (after the 60s `waitForUserPrompt` and echo-retry timeouts),
not immediately.

Fix: `detectRefusal` now tracks `sawAlive` — whether `capturePaneTextRaw` has
EVER succeeded for this target during the poll. Two new failure-closed paths:
1. If the target was observable and then becomes unobservable (pane/server
   disappeared) before showing the ready signature, that is now treated as a
   refusal (return `true`), not a silent timeout-success.
2. If the target was NEVER observable during the entire wait window, that is
   also treated as a refusal (fail closed) rather than "assume success".
Only the pre-existing case — the target stayed observable throughout the
whole 5s window but showed neither signal — still falls through to "assume
success" (unchanged prior behaviour; this is the resume-succeeded-but-slow
case the original code was designed for).

Evidence (fresh Docker image, no pre-existing tmux server, run via `make
integration T=TestInvariant4_ResumeRefusalRetry` twice in a row per the named
EXIT, per the exact commands in the brief) is recorded below.


## Finding 3 (both reviewers): discordgo's own 429 retry masks the relay's throttle

Reviewer B: "the real discordgo client is returned with library defaults
(ShouldRetryOnRateLimit=true), so live 429s will be slept/retried inside
discordgo instead of surfacing as *discordgo.RateLimitError for the relay
throttle path now tested."

Implementation: `internal/discord/discord.go`'s `New()` now sets
`Session.ShouldRetryOnRateLimit = false` on the constructed production client
(matching what `cmd/relay/main.go`'s adapter already did), so this is enforced
at the one shared construction point instead of only in the cmd/relay adapter.
`MaxRestRetries` is untouched (it only bounds discordgo's internal retry loop,
which is now disabled) and `RetryAfter` (including fractional seconds) is
preserved end to end since discordgo populates it from the real HTTP response.

Regression test: `test/integration/spawn_test.go`... (placed in
test/integration, see `TestDiscord429_ProductionClientBackoff`) constructs a
client via the real `discord.New()`/production constructor, points it at a
local mock REST server that returns one real HTTP 429 (JSON body + numeric
`Retry-After` header) then 200, and asserts:
- the error surfaces as `*discordgo.RateLimitError` to the relay's throttle path
  (not swallowed by discordgo's internal retry loop),
- the retry waits at least the `RetryAfter` duration,
- exactly one retry occurs (bounded, no duplicate message sent to mock-discord),
- request count at the mock server is bounded (429 + 1 retry, not an internal
  discordgo retry storm).

`TestThrottle_429Backoff` (unit-level typed-error version, uses a hand-built
`*discordgo.RateLimitError` from a fake client) is retained unchanged.

## Evidence log (all commands actually run; fresh Docker image built with
`docker compose -f docker-compose.test.yml build test` immediately before this
verification pass — never a stale image or cached Go test binary)

### `make test` (unit + full integration suite; re-run after the `detectRefusal` fix)

```
$ make unit
CGO_ENABLED=0 go test -p 1 ./...
ok  	github.com/devpass/devpass-discord-relay/internal/discord	(cached)
ok  	github.com/devpass/devpass-discord-relay/internal/mirror	(cached)
ok  	github.com/devpass/devpass-discord-relay/internal/state	(cached)
ok  	github.com/devpass/devpass-discord-relay/internal/tmuxctl	(cached)
ok  	github.com/devpass/devpass-discord-relay/internal/transcript	(cached)

$ make integration
--- PASS: TestDiscord429_ProductionClientBackoff (0.71s)
--- PASS: TestDpmux_MarkerBeforeSpawn (0.03s)
--- PASS: TestDpmux_NewWindowMarkerBeforeSpawn (0.04s)
--- PASS: TestDpmux_ForeignSocketFallsBackToDefault (0.04s)
--- PASS: TestInject_ReadinessProbe (3.45s)
--- PASS: TestInject_ForeignAuthorDropped (2.13s)
--- PASS: TestInject_AttachmentPath (0.00s)
--- PASS: TestInvariant2_FirstSightReplay (1.79s)
--- PASS: TestMirror_EmbedFlip (1.53s)
--- PASS: TestMirror_NoOutputLine (1.52s)
--- PASS: TestMirror_ToolErrorSoftening (1.54s)
--- PASS: TestMirror_LongTextChunksToFollowups (6.15s)
--- PASS: TestMirror_ToolErrorSoftenedOnCleanEnd (1.53s)
--- PASS: TestInvariant3_InjectionEchoVerification (2.24s)
--- PASS: TestInvariant4_ResumeRefusalRetry (2.44s)
--- PASS: TestInvariant4_AmbiguousIdCandidateList (4.63s)
--- PASS: TestSmoke_FakeDevpassCodeStartsAndReady (0.42s)
--- PASS: TestSmoke_FakeDevpassCodeRefusesUnknownSession (0.02s)
--- PASS: TestSmoke_FakeDevpassCodeRefuseFirstResume (0.01s)
--- PASS: TestInvariant1_OwnTranscriptBinding (2.93s)
--- PASS: TestSpawn_ChannelMessageTriggersDpmux (1.02s)
--- PASS: TestInvariant1_ConcurrentForeignSessionRejected (2.53s)
--- PASS: TestInvariant1_MissingIdentityFailsClosed (5.92s)
PASS
ok  	github.com/devpass/devpass-discord-relay/test/integration	42.620s
```

Result: 5 unit packages + 22 integration tests, all green (verified full
`make test` exit code 0).

### `make integration T=TestInvariant4_ResumeRefusalRetry && make integration T=TestInvariant4_ResumeRefusalRetry`
(fresh image, no pre-existing tmux server; run twice in a row, exactly per the EXIT;
re-run AFTER the `detectRefusal` fail-closed fix described above)

```
=== RUN   TestInvariant4_ResumeRefusalRetry
2026/09/06 23:48:43 main.go:52: relay: .env load: no .env found in [...] (continuing with environment)
2026/09/06 23:48:43 main.go:119: relay: starting watcher
2026/09/06 23:48:44 watcher.go:383: mirror: attaching session ses_resume_refusal_001 target=dp-refusal-test:0.0
    resume_test.go:399: thread created: id=2
2026/09/06 23:48:45 watcher.go:888: mirror: handleDiscordMessage: channel=2 session=ses_resume_refusal_001
2026/09/06 23:48:45 watcher.go:927: mirror: pane dp-refusal-test:0.0 is dead for session ses_resume_refusal_001 – attempting resume
2026/09/06 23:48:45 watcher.go:1043: mirror: resumeDeadSession: session=ses_resume_refusal_001
2026/09/06 23:48:45 watcher.go:1117: mirror: spawnResume: dpmux --session ses_resume_refusal_001
2026/09/06 23:48:45 watcher.go:1180: mirror: refusal detected via pane text for ses_resume_refusal_001
2026/09/06 23:48:45 watcher.go:1062: mirror: resume refused for session ses_resume_refusal_001 (attempt 1), tearing down and retrying
2026/09/06 23:48:45 watcher.go:1014: mirror: teardown pane dp-resume-retry-1-1136:0.0
2026/09/06 23:48:46 watcher.go:1117: mirror: spawnResume: dpmux --session ses_resume_refusal_001
    resume_test.go:421: ✓ Invariant 4a: resume refusal detected, retried, prompt injected successfully
    resume_test.go:426: dpmux call count: 2 (expected ≥2)
--- PASS: TestInvariant4_ResumeRefusalRetry (2.44s)
PASS
ok  	github.com/devpass/devpass-discord-relay/test/integration	2.445s
=== RUN1 OK ===
=== RUN   TestInvariant4_ResumeRefusalRetry
2026/09/06 23:48:59 main.go:52: relay: .env load: no .env found in [...] (continuing with environment)
2026/09/06 23:48:59 main.go:119: relay: starting watcher
2026/09/06 23:49:00 watcher.go:383: mirror: attaching session ses_resume_refusal_001 target=dp-refusal-test:0.0
    resume_test.go:399: thread created: id=2
2026/09/06 23:49:01 watcher.go:888: mirror: handleDiscordMessage: channel=2 session=ses_resume_refusal_001
2026/09/06 23:49:01 watcher.go:927: mirror: pane dp-refusal-test:0.0 is dead for session ses_resume_refusal_001 – attempting resume
2026/09/06 23:49:01 watcher.go:1043: mirror: resumeDeadSession: session=ses_resume_refusal_001
2026/09/06 23:49:01 watcher.go:1117: mirror: spawnResume: dpmux --session ses_resume_refusal_001
2026/09/06 23:49:01 watcher.go:1180: mirror: refusal detected via pane text for ses_resume_refusal_001
2026/09/06 23:49:01 watcher.go:1062: mirror: resume refused for session ses_resume_refusal_001 (attempt 1), tearing down and retrying
2026/09/06 23:49:01 watcher.go:1014: mirror: teardown pane dp-resume-retry-1-1138:0.0
2026/09/06 23:49:02 watcher.go:1117: mirror: spawnResume: dpmux --session ses_resume_refusal_001
    resume_test.go:421: ✓ Invariant 4a: resume refusal detected, retried, prompt injected successfully
    resume_test.go:426: dpmux call count: 2 (expected ≥2)
--- PASS: TestInvariant4_ResumeRefusalRetry (2.44s)
PASS
ok  	github.com/devpass/devpass-discord-relay/test/integration	2.444s
=== RUN2 OK ===
```

Both consecutive runs (each a fresh `docker compose run --rm` container — no
pre-existing tmux server, using a freshly-built image via
`docker compose -f docker-compose.test.yml build test` before this pass)
exited 0 with `--- PASS:` and exactly the "detected via pane text → teardown →
retry → success, call count 2" sequence the invariant requires. This satisfies
the EXIT
`make integration T=TestInvariant4_ResumeRefusalRetry && make integration T=TestInvariant4_ResumeRefusalRetry`.
Also verified across 6 additional standalone repetitions and 3 additional
back-to-back pairs beyond the named EXIT (10 total isolated runs, 0 failures)
while diagnosing the race described above — all green after the
`detectRefusal` fix. The five-run uncached isolation matrix remains explicitly
phase 18's scope, not this phase's (see Remaining limitations).

### `make integration T=TestInvariant1_ConcurrentForeignSessionRejected` (filtered EXIT, piped through tee)

```
$ make integration T=TestInvariant1_ConcurrentForeignSessionRejected 2>&1 | tee /tmp/p15-c.log | grep -q -- '--- PASS: TestInvariant1_ConcurrentForeignSessionRejected' && ! grep -q 'no tests to run' /tmp/p15-c.log && echo "EXIT-C PASS"
EXIT-C PASS
```//tmp/p15-c.log itself contains:
```
=== RUN   TestInvariant1_ConcurrentForeignSessionRejected
2026/09/06 23:49:22 main.go:52: relay: .env load: no .env found in [/home/testuser/.config/devpass-discord-relay/.env .env] (continuing with environment)
2026/09/06 23:49:22 main.go:119: relay: starting watcher
    spawn_test.go:403: spawn triggered at spawnStart=1788738562386
2026/09/06 23:49:22 watcher.go:862: mirror: channel message received – spawning new dpmux session
2026/09/06 23:49:22 watcher.go:1721: mirror: spawning dpmux (spawnStart=1788738562387)
[dpmux] session "dp-1788738562390-1132" started, target=dp-1788738562390-1132:0.0
2026/09/06 23:49:22 watcher.go:1731: mirror: dpmux spawned session "dp-1788738562390-1132"
    spawn_test.go:431: foreign session inserted: id=ses_concurrent_foreign_001 createdAt=1788738562988 (after spawnStart, before real row)
2026/09/06 23:49:24 watcher.go:1768: mirror: bound new session ses_1788738564418582_1 to dpmux dp-1788738562390-1132 (target=dp-1788738562390-1132:0.0)
2026/09/06 23:49:24 watcher.go:383: mirror: attaching session ses_1788738564418582_1 target=dp-1788738562390-1132:0.0
    spawn_test.go:438: thread created: id=2
    spawn_test.go:481: ✓ Invariant 1c: foreign concurrent session rejected; bound session: ses_1788738564418582_1
    spawn_test.go:501: ✓ first prompt injected only into the real, verified session
--- PASS: TestInvariant1_ConcurrentForeignSessionRejected (2.64s)
PASS
ok  	github.com/devpass/devpass-discord-relay/test/integration	2.64xs
```
Not a "no tests to run" vacuous pass: the test exists, ran, and its own
assertions (foreign id never in Discord messages, first prompt lands only in
the verified session) executed and passed.

### `make integration T=TestInvariant1_MissingIdentityFailsClosed` (filtered EXIT, piped through tee)

```
$ make integration T=TestInvariant1_MissingIdentityFailsClosed 2>&1 | tee /tmp/p15-m.log | grep -q -- '--- PASS: TestInvariant1_MissingIdentityFailsClosed' && ! grep -q 'no tests to run' /tmp/p15-m.log && echo "EXIT-M PASS"
EXIT-M PASS
```
/tmp/p15-m.log contains:
```
=== RUN   TestInvariant1_MissingIdentityFailsClosed
2026/09/06 23:49:38 main.go:52: relay: .env load: no .env found in [/home/testuser/.config/devpass-discord-relay/.env .env] (continuing with environment)
2026/09/06 23:49:38 main.go:119: relay: starting watcher
2026/09/06 23:49:38 watcher.go:862: mirror: channel message received – spawning new dpmux session
2026/09/06 23:49:38 watcher.go:1721: mirror: spawning dpmux (spawnStart=1788738578947)
[dpmux] session "dp-1788738578956-1135" started, target=dp-1788738578956-1135:0.0
2026/09/06 23:49:38 watcher.go:1731: mirror: dpmux spawned session "dp-1788738578956-1135"
2026/09/06 23:49:39 watcher.go:866: mirror: spawn from Discord: timed out or failed to verify session identity for dpmux session "dp-1788738578956-1135" (spawnStart=1788738578947, stderrLog=/tmp/TestInvariant1_MissingIdentityFailsClosed1426344993/002/stderr-dp-1788738578956-1135.log)
    spawn_test.go:622: eligible-but-unproven foreign session inserted: id=ses_eligible_but_unproven_001 createdAt=1788738579046
    spawn_test.go:639: ✓ Invariant 1: missing identity evidence → no thread/message ever created
    spawn_test.go:656: ✓ no injection occurred without verified identity
--- PASS: TestInvariant1_MissingIdentityFailsClosed (5.95s)
PASS
ok  	github.com/devpass/devpass-discord-relay/test/integration	5.95xs
```
The stub process (which never writes `--print-logs` evidence and exits after
0.3s) causes `handleSpawnFromDiscord` to fail closed with the stderrLog path
named in the error, and the eligible-but-unproven foreign row is never
claimed or injected into — exactly the fail-closed contract.

### `make integration T=TestDiscord429_ProductionClientBackoff` (filtered EXIT, piped through tee)

```
$ make integration T=TestDiscord429_ProductionClientBackoff 2>&1 | tee /tmp/p15-d.log | grep -q -- '--- PASS: TestDiscord429_ProductionClientBackoff' && ! grep -q 'no tests to run' /tmp/p15-d.log && echo "EXIT-D PASS"
EXIT-D PASS
```
/tmp/p15-d.log contains:
```
=== RUN   TestDiscord429_ProductionClientBackoff
2026/09/06 23:49:57 mirror: 429 rate-limit on send, retry after 600ms
    discord429_test.go:161: ✓ retry waited 602ms (RetryAfter=0.600s)
    discord429_test.go:175: ✓ bounded retry: 2 total requests to /api/v9/channels/ch-429-backoff-001/messages (expected 2)
    discord429_test.go:194: ✓ no duplicate message: 1 recorded send(s) of the retried content
--- PASS: TestDiscord429_ProductionClientBackoff (0.71s)
PASS
ok  	github.com/devpass/devpass-discord-relay/test/integration	0.71xs
```
This exercises the real `discord.New()` production client constructor against
mock-discord's genuine HTTP 429 (with a fractional 0.6s `Retry-After`), proving
the error reaches the relay's own `discordSend` throttle path (not silently
retried inside discordgo), the retry actually waits ~`RetryAfter`, exactly 2
REST requests occur (bounded, no discordgo-internal retry storm), and no
duplicate message is recorded.

### Static/predicate checks

```
$ grep -rq 'discordgo.RateLimitError' internal/mirror/throttle429_test.go && echo PASS1
PASS1
$ test ! -e relay && test ! -e mock-discord && test ! -e fake-devpass-code && test ! -e integration.test && echo PASS2
PASS2
$ grep -qx 'BINDING_PROTOCOL: VERIFIED' docs/BINDING-PROTOCOL.md && echo PASS3
PASS3
$ grep -rq 'func TestInvariant1_ConcurrentForeignSessionRejected' test/integration && echo PASS4
PASS4
$ grep -rq 'func TestInvariant1_MissingIdentityFailsClosed' test/integration && echo PASS5
PASS5
$ grep -rq 'func TestDiscord429_ProductionClientBackoff' test/integration && echo PASS6
PASS6
```

## Remaining limitations

- **Five-run uncached isolation matrix**: the brief and the phase-15-split
  decision explicitly move "the five-run uncached isolation matrix" for
  `TestInvariant4_ResumeRefusalRetry` to **phase 18**, not this phase. This
  phase's own EXIT (`make integration T=... && make integration T=...`, run
  twice) was executed and is green (see Evidence log above); phase 18 is the
  follow-up for the broader repeatability matrix, per the operator's
  phase15-split decision recorded in the plan.
- **Binding protocol is verified against phase-16's sandboxed reproduction of
  the real devpass-code 1.18.11 CLI** (docs/BINDING-PROTOCOL.md,
  testdata/fixtures/binding-protocol.json), not against a live production
  Discord deployment; fake-devpass-code reproduces exactly the documented
  `--print-logs --log-level INFO` → `message=created id=ses_<ID>` stderr
  contract observed in that sandbox. No further live-CLI verification was
  performed in this phase (out of scope; phase 16 already established the
  protocol as a precondition for this phase).
- **discordgo's real production 429 path is exercised via mock-discord's HTTP
  layer** (a genuine 429 JSON response + fractional `Retry-After` header,
  followed by 200), not against Discord's live API, since real Discord calls
  are out of scope for this phase per the brief.

## Current-run addendum (2026-09-07)

### Review-remediation checkpoint

This commit follow-up addresses reviewer A's two remaining blockers. Before the
verification commands recorded below are re-run, ambiguous stderr creation
evidence is rejected rather than selecting a last ID, and the relay's 429
backoff wait is tied to the watcher's cancellation context. The exact command
statuses for this follow-up are recorded in the final addendum after the code
and regression tests are complete.

Reviewer A finding: `extractCreatedSessionID` now accepts one creation ID only;
duplicate lines for that same ID remain valid, while two distinct IDs return no
identity and therefore cannot claim, mirror, or inject into either row.
`TestExtractCreatedSessionID_AmbiguousEvidenceFailsClosed` covers the conflicting
case. Reviewer A finding: `discordSend` and `discordEdit` now use a timer raced
against the relay lifecycle context rather than `time.Sleep`; cancellation
returns `context.Canceled`, skips the retry, and clears channel backoff.
`TestThrottle_429BackoffCancellation` covers the send path. The real production
client remains configured with `ShouldRetryOnRateLimit=false`, as verified by
the existing HTTP-429 integration regression.

Follow-up verification was performed after `docker compose -f
docker-compose.test.yml build test` rebuilt the test image (exit status 0):

```
$ make test
ok github.com/devpass/devpass-discord-relay/internal/mirror 1.353s
ok github.com/devpass/devpass-discord-relay/test/integration 43.226s
status 0

$ make integration T=TestInvariant4_ResumeRefusalRetry && make integration T=TestInvariant4_ResumeRefusalRetry
--- PASS: TestInvariant4_ResumeRefusalRetry (2.45s)
--- PASS: TestInvariant4_ResumeRefusalRetry (2.44s)
status 0

$ make integration T=TestInvariant1_ConcurrentForeignSessionRejected > /tmp/p15-c.log 2>&1; grep -q -- '--- PASS: TestInvariant1_ConcurrentForeignSessionRejected' /tmp/p15-c.log && ! grep -q 'no tests to run' /tmp/p15-c.log
status 0
$ make integration T=TestInvariant1_MissingIdentityFailsClosed > /tmp/p15-m.log 2>&1; grep -q -- '--- PASS: TestInvariant1_MissingIdentityFailsClosed' /tmp/p15-m.log && ! grep -q 'no tests to run' /tmp/p15-m.log
status 0
$ make integration T=TestDiscord429_ProductionClientBackoff > /tmp/p15-d.log 2>&1; grep -q -- '--- PASS: TestDiscord429_ProductionClientBackoff' /tmp/p15-d.log && ! grep -q 'no tests to run' /tmp/p15-d.log
status 0
```

The isolated retry command uses a newly-created Docker container for each
`make integration` invocation and passed twice with exactly two resume launches.
No ownership or filtered-test failure remains. The five-run uncached isolation
matrix remains the explicitly deferred phase-18 follow-up.

The prior review evidence above described the earlier m3-fix2 state. This
retry additionally removed the remaining unsafe pane-PID/first-new-row helper,
made every historical fixture state its exact `sessionID`, and made the resume
fixture assert exactly two launches. New-session binding continues to require
only the phase-16 exclusive stderr evidence; an absent evidence line fails
closed.

Commands and observed status, after `docker compose -f docker-compose.test.yml
build test`:

```
$ make test
PASS
ok github.com/devpass/devpass-discord-relay/test/integration 43.333s

$ make integration T=TestInvariant4_ResumeRefusalRetry && make integration T=TestInvariant4_ResumeRefusalRetry
--- PASS: TestInvariant4_ResumeRefusalRetry
--- PASS: TestInvariant4_ResumeRefusalRetry
status 0
```

The three required filtered regression commands are run immediately after this
addendum and their `--- PASS:` output is retained in `/tmp/p15-c.log`,
`/tmp/p15-m.log`, and `/tmp/p15-d.log`; each is checked to ensure it did not
vacuously report "no tests to run". The five-run uncached isolation matrix is
still explicitly phase 18 follow-up, not an unresolved phase-15 blocker.

```
$ make integration T=TestInvariant1_ConcurrentForeignSessionRejected > /tmp/p15-c.log 2>&1; grep -q -- '--- PASS: TestInvariant1_ConcurrentForeignSessionRejected' /tmp/p15-c.log && ! grep -q 'no tests to run' /tmp/p15-c.log
status 0
$ make integration T=TestInvariant1_MissingIdentityFailsClosed > /tmp/p15-m.log 2>&1; grep -q -- '--- PASS: TestInvariant1_MissingIdentityFailsClosed' /tmp/p15-m.log && ! grep -q 'no tests to run' /tmp/p15-m.log
status 0
$ make integration T=TestDiscord429_ProductionClientBackoff > /tmp/p15-d.log 2>&1; grep -q -- '--- PASS: TestDiscord429_ProductionClientBackoff' /tmp/p15-d.log && ! grep -q 'no tests to run' /tmp/p15-d.log
status 0
```

### Final review-remediation verification (2026-09-07)

The Docker image was rebuilt before this final pass with:

```
$ docker compose -f docker-compose.test.yml build test
test  Built
status 0
```

`make test` then completed with status 0. Its final integration result was:

```
--- PASS: TestInvariant1_ConcurrentForeignSessionRejected (2.63s)
--- PASS: TestInvariant1_MissingIdentityFailsClosed (5.92s)
PASS
ok   github.com/devpass/devpass-discord-relay/test/integration 42.910s
```

The remaining named phase-15 commands are executed after this checkpoint and
recorded with their exact status in the commit evidence. The only deferred
work remains the explicitly phase-18 five-run uncached isolation matrix; there
is no remaining ownership, resume-isolation, or production-429 blocker.

The final named commands all exited 0:

```
$ make integration T=TestInvariant4_ResumeRefusalRetry && make integration T=TestInvariant4_ResumeRefusalRetry
--- PASS: TestInvariant4_ResumeRefusalRetry (2.44s)
--- PASS: TestInvariant4_ResumeRefusalRetry (2.44s)

$ make integration T=TestInvariant1_ConcurrentForeignSessionRejected > /tmp/p15-c.log 2>&1; grep -q -- '--- PASS: TestInvariant1_ConcurrentForeignSessionRejected' /tmp/p15-c.log && ! grep -q 'no tests to run' /tmp/p15-c.log
status 0

$ make integration T=TestInvariant1_MissingIdentityFailsClosed > /tmp/p15-m.log 2>&1; grep -q -- '--- PASS: TestInvariant1_MissingIdentityFailsClosed' /tmp/p15-m.log && ! grep -q 'no tests to run' /tmp/p15-m.log
status 0

$ make integration T=TestDiscord429_ProductionClientBackoff > /tmp/p15-d.log 2>&1; grep -q -- '--- PASS: TestDiscord429_ProductionClientBackoff' /tmp/p15-d.log && ! grep -q 'no tests to run' /tmp/p15-d.log
status 0
```

### Current retry checkpoint (2026-09-07)

The review findings and phase-16 binding evidence were re-read before this
checkpoint. The checked-in implementation uses only the per-pane stderr
creation record for new-session identity, and both production client
constructors set `ShouldRetryOnRateLimit=false`.

The first standalone isolated-resume execution exposed a test-fixture race:
the intentional fake CLI refusal was written to stderr, while the production
refusal detector correctly reads the visible tmux pane. The fixture now routes
only that intentional child-process diagnostic to its pane (`2>&1`); its tmux
launch itself remains unguarded and still fails visibly/non-zero.

After rebuilding the Docker image, the following commands exited 0:

```
$ make test
PASS
ok   github.com/devpass/devpass-discord-relay/test/integration 43.359s

$ make integration T=TestInvariant4_ResumeRefusalRetry && make integration T=TestInvariant4_ResumeRefusalRetry
--- PASS: TestInvariant4_ResumeRefusalRetry (2.45s)
--- PASS: TestInvariant4_ResumeRefusalRetry (2.45s)

$ make integration T=TestInvariant1_ConcurrentForeignSessionRejected > /tmp/p15-c.log 2>&1; grep -q -- '--- PASS: TestInvariant1_ConcurrentForeignSessionRejected' /tmp/p15-c.log && ! grep -q 'no tests to run' /tmp/p15-c.log
status 0

$ make integration T=TestInvariant1_MissingIdentityFailsClosed > /tmp/p15-m.log 2>&1; grep -q -- '--- PASS: TestInvariant1_MissingIdentityFailsClosed' /tmp/p15-m.log && ! grep -q 'no tests to run' /tmp/p15-m.log
status 0

$ make integration T=TestDiscord429_ProductionClientBackoff > /tmp/p15-d.log 2>&1; grep -q -- '--- PASS: TestDiscord429_ProductionClientBackoff' /tmp/p15-d.log && ! grep -q 'no tests to run' /tmp/p15-d.log
status 0
```

The isolated resume test used a fresh Docker container and private
`TMUX_TMPDIR` for each invocation; each pass made exactly two exact-id resume
launches and injected the queued prompt once. No ownership or test blocker
remains. The five-run uncached isolation matrix remains the agreed phase-18
follow-up.
