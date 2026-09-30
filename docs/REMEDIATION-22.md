# Remediation 22: gateway readiness gate, flake-free readiness probe

## Root cause

`TestInject_ReadinessProbe` failed intermittently: the thread and session were
recorded but the prompt never reached the DB. Two independent defects were found
by reproduction (25 direct runs of the compiled test binary at HEAD 5e40049
produced 2 failures, r4 and r18; a 120-run loaded loop reproduced it again).

1. **Gateway events dispatched before the relay had a READY consumer.**
   `mock-discord` `/control/dispatch` pushed onto a shared buffered channel
   with no check that a gateway session was connected and READY, so an event
   dispatched during HELLO/IDENTIFY/READY could be consumed by (or lost to) a
   connection that was not yet listening. The relay also treated a failed
   `Session.Open()` as non-fatal ("continuing without inbound events"), so it
   could run deaf to inbound events forever, and it exposed no readiness state.
   Waiting for `sessions.json` (phase 21) does not prove a gateway consumer.
   Reproduction of the delivery race: `TestGateway_DispatchBeforeReadyIsDelivered`
   (dispatch immediately after relay start, no readiness wait).
2. **Fake CLI SQLITE_BUSY.** With the relay and test reading the same DB, the
   fake `fake-devpass-code` occasionally died at startup with
   `database is locked (5) (SQLITE_BUSY)` (captured in
   `docs/flake-logs/fake-devpass-code-exit-k3.log`), which produced exactly the
   observed symptom: relay logs `tmux pane dp-inject-rp:0.0 died` and the prompt
   is never echoed (`docs/flake-logs/flake-fail-k3-stderr.log`, and the original
   failure `docs/flake-logs/flake-fail-r4-before-fix.log`). Honest note: this
   second defect was found while chasing reviewer-b's hypothesis; the fail
   in k3 is this one, not the gateway race.

## Fix

- `cmd/relay/main.go`: `gateway-ready` file under `RELAY_STATE_DIR` written on the
  discordgo READY event and again on RESUMED after an automatic reconnect (log line
  `relay: gateway ready (READY)` / `(RESUMED)`), removed on disconnect.
  `Open()` is retried with backoff (6 attempts); if it never succeeds the relay
  returns an error and exits non-zero instead of running deaf.
- `cmd/mock-discord/main.go`: `/control/dispatch` holds the event until a
  connection has sent IDENTIFY, been sent READY **and sent its first heartbeat**
  (bounded 10s wait, 503 with a clear error if none). discordgo's `Open()` returns
  after reading READY but before its listen goroutine runs; the heartbeat loop and
  listen loop start together, so the first post-READY heartbeat is the earliest
  point at which the client is provably consuming. Only such connections consume
  events. `MOCK_READY_DELAY_MS` (mock only) delays READY after IDENTIFY so a test
  can force dispatch strictly before READY.
- `cmd/fake-devpass-code/main.go`: SQLite open uses `busy_timeout(10000)` (fake
  only; no production or test assertion changed).
- Tests: one shared helper `waitForRelayReady(t, stateDir)` (inject_test.go) is
  called before every `dispatchEvent` in inject, resume, spawn and
  restart-survival tests. New `TestGateway_DispatchBeforeReadyIsDelivered`
  dispatches with no readiness wait, **fails hard if the relay's `gateway-ready` file
  exists at dispatch time or if the mock returns the dispatch before READY**
  (`MOCK_READY_DELAY_MS=1500` makes that precondition true), and asserts exactly one
  delivery. `TestGateway_DispatchWithoutSessionFailsClearly` covers the 503 path.
  `FAKE_DP_SWALLOW_UNTIL_READY=1` / `FAKE_DP_READY_DELAY_MS=3000` and all waits
  are unchanged.

## Results (logs in docs/flake-logs/, quoted verbatim)

### Dispatch-before-READY test discriminates the hold

- Hold disabled in `cmd/mock-discord/main.go` (the `n > 0` release condition
  temporarily forced true, then restored), run with
  `make integration T=TestGateway_DispatchBeforeReadyIsDelivered`:
  `docs/flake-logs/hold-disabled-dispatch-before-ready-FAIL.log`
  - line 77: `inject_test.go:910: dispatch returned before the gateway was READY (err=<nil>); mock did not hold it`
  - line 78: `--- FAIL: TestGateway_DispatchBeforeReadyIsDelivered (0.36s)`
- Hold enabled (committed code): `docs/flake-logs/final22-gateway.log`
  - line 83: `--- PASS: TestGateway_DispatchBeforeReadyIsDelivered (4.21s)`
  - line 85: `ok  	github.com/devpass/devpass-discord-relay/test/integration	4.214s`
- Earlier history, kept honest: the first version of this test did not force the
  precondition, and passed even against the old mock
  (`old-mock-new-relay-dispatch-before-ready-1.log` line 7:
  `--- PASS: TestGateway_DispatchBeforeReadyIsDelivered (2.63s)`). Reviewers were
  right that it proved nothing; the current test asserts its own precondition.
  `after-fix-gateway-dispatch.log` line 86 is that earlier-version pass and is not
  cited as evidence of discrimination.

### Readiness probe, 10 consecutive runs

`make integration T=TestInject_ReadinessProbe`, logs `final22-readiness-1..10.log`;
every log contains `--- PASS: TestInject_ReadinessProbe (3.45s)` or `(3.46s)`
(line 80 or 81 depending on the run). 10/10 passed. One attempt at run 7 failed
before any test ran with a Docker image-export error
(`infra-docker-snapshot-error-readiness-7.log`, line 71:
`failed to prepare extraction snapshot ... parent snapshot ... does not exist: not found`);
this is a Docker/containerd storage fault, not a test result, and run 7 was repeated
once (`final22-readiness-7.log` line 81 PASS). Earlier `after-fix-readiness-r1..r4.log`
also pass (line 80, `3.45s`/`3.46s`).

### Full suite, 5 consecutive `make test`

`final22-make-test-1..5.log`: each ends `ok  	github.com/devpass/devpass-discord-relay/test/integration`
(line 314, 314, 314, 315, 314; 65.389s, 65.355s, 65.037s, 65.174s, 64.947s). 5/5 passed,
run as `for i in 1 2 3 4 5; do make test > log || break; done`.

### SQLITE_BUSY in the fake CLI (second cause of the original flake)

`fake-devpass-code-exit-k3.log` line 1: `database is locked (5) (SQLITE_BUSY)`;
`flake-fail-k3-stderr.log` line 8: `mirror: tmux pane dp-inject-rp:0.0 died`.
Fixed in the fake with `busy_timeout(10000)`. 40 loaded runs after the fix:
`after-fix-40-loaded-runs.result` contains only `done`.

## Marker flake: root cause (version-probe race) and fix

Earlier `make test` runs failed on marker tests:

- `make-test-fail-marker-before-spawn-run1.log` line 97:
  `dpmux_test.go:80: child started before pending marker existed: "marker_found=false"`;
  line 98: `--- FAIL: TestDpmux_MarkerBeforeSpawn (1.06s)`.
- `make-test-fail-run1.log` line 98: `dpmux_test.go:176: scope child did not inherit dpmux marker environment: got "marker_found=false state_dir=/workspace/dpmux-marker-3711638905 ...`;
  line 99: `--- FAIL: TestDpmux_MarkerBeforeSpawn_SystemdScope (1.06s)`.

Root cause: `verifyDevpassBin` in `cmd/dpmux/main.go` (line 604) runs
`exec.Command(path, "-V")` on `RELAY_DEVPASS_BIN` **before** dpmux writes the pending
marker. The test stubs treated every invocation as the spawned child: the probe ran
first, found no marker, and wrote `marker_found=false` into the log file; the test
read that file before (or instead of) the real child overwrote it. Not a dpmux
ordering bug and not a cold-container effect: the real child always saw the marker.

Evidence that settled it (stub with a snapshot diagnostic, three reproductions):
`marker-probe-race-fail-1.log` line 5 (`stub entry snapshot`, written by the stub
invocation that ran with the marker present) lists `pending-dp-1790722816310-1129`
in `enter_ls`, while line 6 shows the test read `"marker_found=false"`. Both can be
true only if two stub invocations ran: an earlier one that saw no marker and wrote
`false` (the `-V` probe, which runs before the marker exists) and the real child that
ran later and saw it. The test polled the log file, read the probe's `false` first,
and failed. `marker-probe-race-fail-2.log` and `-3.log` have the same shape (lines 5-6).

Fix (test stubs only; no assertion weakened, marker-ordering assertions unchanged):
the three marker stubs in `test/integration/dpmux_test.go` now do
`case "$1" in -V|--version) exit 0 ;; esac` before sampling the marker, so only the
actual child records marker state. The temporary failure-only `.diag` output was
removed. Verification after the fix: `final22-marker-1..6.log`, six consecutive runs
of `TestDpmux_MarkerBeforeSpawn`, `_SystemdScope` and `TestDpmux_NewWindowMarkerBeforeSpawn`,
each with three `--- PASS:` lines and `ok`; and the five `make test` runs above.

Residual: none known. The marker flake is attributed to the version-probe race; if it
recurs with the guard in place, treat that as a new defect.
