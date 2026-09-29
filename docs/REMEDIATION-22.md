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
  discordgo READY event (log line `relay: gateway ready`), removed on disconnect.
  `Open()` is retried with backoff (6 attempts); if it never succeeds the relay
  returns an error and exits non-zero instead of running deaf.
- `cmd/mock-discord/main.go`: `/control/dispatch` holds the event until a
  connection has sent IDENTIFY and been sent READY (bounded 10s wait, 503 with a
  clear error if none); only READY connections consume events.
- `cmd/fake-devpass-code/main.go`: SQLite open uses `busy_timeout(10000)` (fake
  only; no production or test assertion changed).
- Tests: one shared helper `waitForRelayReady(t, stateDir)` (inject_test.go) is
  called before every `dispatchEvent` in inject, resume, spawn and
  restart-survival tests. New `TestGateway_DispatchBeforeReadyIsDelivered`
  dispatches with no readiness wait and asserts exactly one delivery.
  `FAKE_DP_SWALLOW_UNTIL_READY=1` / `FAKE_DP_READY_DELAY_MS=3000` and all waits
  are unchanged.

## Results (logs in docs/flake-logs/, quoted verbatim)

- `after-fix-gateway-dispatch.log`:
  `--- PASS: TestGateway_DispatchBeforeReadyIsDelivered (2.73s)`
- `after-fix-readiness-r1..r4.log` (`make integration T=TestInject_ReadinessProbe`):
  each shows `--- PASS: TestInject_ReadinessProbe (3.4xs)`; 4/4 passed.
- 40 consecutive direct runs of the compiled test binary under 3 busy-loop CPU
  hogs after the fix: 0 failures (`after-fix-40-loaded-runs.result` is `done`
  with no `failed` lines).
- Reproduction limitation: with the old mock, the dispatch-before-ready test
  also passes on a fast host (`old-mock-new-relay-dispatch-before-ready-1.log`,
  3/3 PASS), so the gateway race is a latent window, not one I could trigger on
  demand; the reproduced flake was the SQLITE_BUSY fake exit above.

## Evidence-gate follow-up: repeated `make test` runs

The first landing did not run `make test`. Running the 5x predicate afterwards
exposed a second, different intermittent failure in the marker tests (not the
readiness probe, not the gateway):

- `docs/flake-logs/make-test-fail-marker-before-spawn-run1.log` line 98:
  `dpmux_test.go:80: child started before pending marker existed: "marker_found=false"`
  then `--- FAIL: TestDpmux_MarkerBeforeSpawn (1.06s)`.
- `docs/flake-logs/make-test-fail-run1.log` line 98-99:
  `scope child did not inherit dpmux marker environment: got "marker_found=false state_dir=/workspace/dpmux-marker-3711638905 home=/home/testuser", want "marker_found=true ..."`
  then `--- FAIL: TestDpmux_MarkerBeforeSpawn_SystemdScope (1.06s)`.
  The state dir was correct; only the marker glob came back empty.

Honest status: I could not reproduce this in isolation (40x and 30x `-count`
runs of the marker test, 12 fresh-container runs, 6 full-suite direct runs and
3 `make integration` runs: 0 failures; logs `marker-repro-x40.log`,
`marker-repro-x30-with429.log`, `marker-fresh-container-*.log`,
`full-repro-*.log`). It surfaced only in the first `make test` after a
rebuild, i.e. a cold container. I did not identify a root cause and did not
change production code for it. I did add failure-only diagnostics to the two
marker stubs (a `.diag` file listing the state dir as the child sees it,
included in the `t.Fatalf` message); no assertion was changed or loosened.

Final gate, run after the diagnostics change, as the exact predicate loop
(`for i in 1 2 3 4 5; do make test ...`): 5/5 passed, logs
`docs/flake-logs/make-test-pass-final-1..5.log`, each ending
`ok  github.com/devpass/devpass-discord-relay/test/integration`.
Earlier in the same session the loop failed on run 1 twice (the two logs above),
so the marker flake is a known residual: rare, cold-start, undiagnosed.
The `.diag` output will identify it if it recurs.
