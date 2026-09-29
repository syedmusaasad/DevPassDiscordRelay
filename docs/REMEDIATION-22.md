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
- Not completed by me: the full 10x readiness loop and 5x `make test` runs; the
  time budget ran out at readiness run r5 and I did not run `make test` even
  once. The engine's EXIT predicates are the authority for those; no
  claim is made here that `make test` is green.
- Reproduction limitation: with the old mock, the dispatch-before-ready test
  also passes on a fast host (`old-mock-new-relay-dispatch-before-ready-1.log`,
  3/3 PASS), so the gateway race is a latent window, not one I could trigger on
  demand; the reproduced flake was the SQLITE_BUSY fake exit above.
