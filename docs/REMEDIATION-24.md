# Remediation 24: live-CLI fixes (spawn deadlock, wrong teardown, probe latency)

Source: first real-CLI run on 2026-10-06 (journal 03:44-03:51,
`journalctl --user -u devpass-discord-relay`). No assertion was loosened, no
timeout raised, no test skipped. Logs are under `docs/flake-logs/` and quoted
verbatim below.

| # | Defect (live evidence) | Fix | Test evidence |
|---|------------------------|-----|---------------|
| 1 | Spawn deadlock: `handleSpawnFromDiscord` waited for the session identity before injecting the first prompt, but devpass-code 1.18.11 creates its session (and logs `message=created`) only after the first prompt (docs/BINDING-PROTOCOL.md). Journal: spawn at 03:49:24, `timed out or failed to verify session identity` at 03:50:25; `stderr-dp-1791258563432-3351683.log` has 0 `message=created` lines and the pane still showed the untouched "Ask anything..." screen. | `internal/mirror/watcher.go` `handleSpawnFromDiscord`: after dpmux starts, `inject.WaitReady` (readiness probe) -> `Injector.SendPrompt` (send-keys) -> only then `bindSessionByStderrLog` on the marker's `stderrLog` (bind timeout starts after the send). Single-created-id, pane-alive and DB-row cross-checks are unchanged (ambiguous or multiple ids still fail closed). Probe or send failure -> teardown + notice, never bind, never mirror. | `TestSpawn_PromptBeforeBind` (new; fails against the old order, see below); `TestInvariant1_*`, `TestSpawn_ChannelMessageTriggersDpmux` moved to the new knob and still pass. |
| 2 | Wrong teardown: journal 03:50:31 `tmux pane dp-1791258254443-3244563:0.0 died` (the LIVE bound session) while the failed spawn `dp-1791258563432-3351683` kept running (`tmux ls` still lists it). | Spawn failure calls `teardownSpawnedSession(sessionName)`: `tmux kill-session -t =<exact dp- name>` (the `=` disables prefix/fuzzy matching, so `dp-...-1` can never hit `dp-...-10`); for a window inside a host session only that window is killed. One failure notice is posted to the channel (no thread exists because nothing is bound). It is posted only if the spawned pane was still alive. | `TestSpawn_FailureTeardownSparesLiveSession` (new). |
| 3 | Probe latency: thread reply "ty" took 32 s to land. Post-turn panes have no "Ask anything..." so the probe ran its full 30 s cap and then the 2 s fallback. | `internal/inject/inject.go` `PaneReady`: ready = `Ask anything...` OR the persistent input row (`┃  Pl-Interactive ...` line) together with the `╹▀▀▀` input-bar border. 30 s cap and 2 s fallback unchanged. `WaitReady` also returns early (no fallback sleep) if the pane cannot be captured for ~1 s. | `TestPaneReady` (fresh pane, live post-turn capture, assistant footer alone is NOT ready, blank pane is not ready). |
| 4 | Fake CLI hid the deadlock by writing the session row at launch. | `cmd/fake-devpass-code/main.go`: `FAKE_DP_SESSION_ON_PROMPT=1` writes the row and the `message=created` stderr line only when the first prompt arrives. `cmd/dpmux/main.go` forwards the knob into the pane env. | `TestSpawn_PromptBeforeBind` runs the whole flow with it. |

## Observed TUI signatures (devpass-code 1.18.11, live capture 2026-10-06)

Captured in a disposable tmux (own socket, own `XDG_DATA_HOME`), `--auto`,
after sending `reply with just the word ok`. Same screen at +4 s ... +24 s:

```
  ┃
  ┃  reply with just the word ok
  ┃
     ok
     ▣  Pl-Interactive · GLM-5.3 · 2.7s
  ┃
  ┃
  ┃
  ┃  Pl-Interactive auto · GLM-5.3 LLM Gateway DevPass
  ╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀
   /tmp/devpass-code/live                                                           5.0K (0%) · $0.00  ctrl+p commands
```

"Ask anything..." is absent; the input row `┃  Pl-Interactive auto · <model> ...`
sits directly above the `╹▀▀▀` border. The fresh screen (also live, used for the
first-prompt probe) is:

```
     ┃
     ┃  Ask anything... "Fix broken tests"
     ┃
     ┃  Pl-Interactive auto · GLM-5.3 LLM Gateway DevPass
     ╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀
     tab agents  ctrl+p commands
```

Note: the assistant footer `▣  Pl-Interactive · GLM-5.3 · 2.7s` also contains
"Pl-Interactive". The matcher therefore requires the `┃` input-bar prefix and the
`╹▀▀▀` border, not the bare word. The pane while the model is still generating
was not separately captured; `PaneReady` only needs the input bar, which is drawn
in both states, so a probe during generation can succeed and the keys are then
queued by the TUI (same behaviour as the pre-existing fresh-pane probe).

## Red/green proof for the deadlock (verbatim)

`docs/flake-logs/phase24-promptbeforebind-against-old-deadlock.log`: the new test
run with `internal/mirror/watcher.go` temporarily restored to HEAD (old
bind-then-inject order; the new test and fake knob kept):

```
    spawn_test.go:779: first prompt never appeared in a spawned pane (deadlock: prompt withheld until identity)
--- FAIL: TestSpawn_PromptBeforeBind (40.28s)
FAIL
FAIL	github.com/devpass/devpass-discord-relay/test/integration	40.288s
```

With the fix (`docs/flake-logs/phase24-targeted.log`, `make integration T='TestSpawn_PromptBeforeBind|TestSpawn_FailureTeardownSparesLiveSession'`):

```
    spawn_test.go:783: ✓ prompt visible in pane dp-1791260110334-1228:0.0
    spawn_test.go:790: ✓ thread created: id=2
    spawn_test.go:816: ✓ prompt in DB under ses_1791260110416284_1, matching the created line
    spawn_test.go:828: ✓ reply mirrored; session bound from the created line
--- PASS: TestSpawn_PromptBeforeBind (0.75s)
    spawn_test.go:923: failed spawn session: dp-1791260111509-1294
    spawn_test.go:961: ✓ live pane + record survive; failed spawn pane gone; one notice
--- PASS: TestSpawn_FailureTeardownSparesLiveSession (5.40s)
ok  	github.com/devpass/devpass-discord-relay/test/integration	6.159s
```

Teardown test: the old code never tore the failed spawn down at all (the
journal shows it still running), so the old code fails the "failed spawn pane is
gone" assertion. The teardown-target assertion (live pane and record survive)
is the guard against any future over-broad kill.

## Probe unit test (`docs/flake-logs/phase24-panready-unit.log`)

```
=== RUN   TestPaneReady
--- PASS: TestPaneReady (0.00s)
PASS
ok  	github.com/devpass/devpass-discord-relay/internal/inject	0.004s
```

## Full suite (`docs/flake-logs/phase24-make-test.log`, `make test`, exit 0)

```
ok  	github.com/devpass/devpass-discord-relay/cmd/dpmux	0.385s
ok  	github.com/devpass/devpass-discord-relay/internal/discord	(cached)
ok  	github.com/devpass/devpass-discord-relay/internal/inject	0.004s
ok  	github.com/devpass/devpass-discord-relay/internal/mirror	1.396s
ok  	github.com/devpass/devpass-discord-relay/internal/state	0.004s
ok  	github.com/devpass/devpass-discord-relay/internal/telemetry	0.258s
ok  	github.com/devpass/devpass-discord-relay/internal/tmuxctl	0.011s
ok  	github.com/devpass/devpass-discord-relay/internal/transcript	0.320s
ok  	github.com/devpass/devpass-discord-relay/test/integration	73.284s
```

36 integration tests reported `--- PASS`, 0 `--- FAIL`.

## Honest notes

- Two existing spawn tests (`TestInvariant1_OwnTranscriptBinding`,
  `TestSpawn_ChannelMessageTriggersDpmux`) now set `FAKE_DP_SESSION_ON_PROMPT=1`
  so they exercise the real-CLI ordering. `TestInvariant1_ConcurrentForeignSessionRejected`
  keeps its launch-time delayed row and `..._MissingIdentityFailsClosed` has no
  identity at all; neither needed changes and both pass.
- `TestInvariant1_MissingIdentityFailsClosed` asserts zero Discord messages.
  Its stub pane exits on its own before the probe completes, so the failure
  path stays silent there (pane already dead: no notice). A spawn whose pane
  is alive at failure time posts exactly one notice.
- The spawn's attachments are downloaded under `staging/<dpmux session name>/`
  because the session id does not exist yet at send time.
- A single run of `internal/telemetry` `TestSendShapeAndDailyOnce` failed once
  under load (`telemetry posted more than once today`) in a parallel-free unit
  run, and passed on all three immediate re-runs and in the final `make test`.
  The telemetry package is untouched by this change.
- The host spawn `dp-1791258563432-3351683` from the live incident is still
  running and was deliberately left alone (outside this task's scope).
