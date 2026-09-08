# Milestone 5/6 review remediation

| Finding | Fix | Evidence |
| --- | --- | --- |
| Restart survival | `TestRestartSurvival_DisposableUnit` probes and logs user-manager capability, starts `relay` in `systemd-run --user --unit relay-test-<pid>` with isolated `XDG_DATA_HOME` and `TMUX_TMPDIR`, dispatches a relay spawn, restarts (or replaces a transient) relay unit, then captures the same pane. A manager-less integration container logs its probe failure. | The named integration test runs twice; the host lifecycle target below supplies the substitute proof when the test image has no user manager. |
| Host proof recipe | `restart-survival-host` builds the binaries then invokes that same disposable-unit lifecycle test on the host's reachable user manager. It starts **relay** as the unit, enters through relay's Discord spawn path to dpmux/pending marker, restarts the relay unit, and captures the original pane through its isolated tmux resolver. | `make restart-survival-host` completed with `--- PASS: TestRestartSurvival_DisposableUnit`. |
| Interaction ACKs | The adapter issues a deferred message-update callback before watcher keystroke handling. | `TestInteraction_Ack` records callback type 6 in mock Discord and observes the pane resolution. |
| Per-prompt controls | Prompt-post state resets after an interaction is finalized or the prompt leaves the pane. | `TestApproval_SecondPromptGetsButtons` finalizes prompt one then observes prompt two controls. |
| Component throttle/backoff | Component sends/edits use the watcher throttle and the same bounded 429 retry helpers as ordinary writes. | Unit throttle coverage and component-path helper implementation. |
| tmux resolution | Watcher liveness/capture/teardown and injector capture/send resolve tmux through `tmuxctl.ResolveTmuxBin`. | Resolver unit tests plus production invocation audit. |
| Telemetry shape | Telemetry uses `sha256(username)[:12]`, sends only `{username,version}`, stamps successful UTC-day sends, and quietly ignores failures. | `internal/telemetry` unit tests cover exact body, daily-once, and quiet failure. |
| Isolation startup log | Relay logs the chosen systemd scope or direct fallback at watcher startup. | Startup log includes `relay: tmux isolation mode:`. |
| Installed dpmux update | `install.sh` writes an install-source marker; `dpmux update` reads it, otherwise clones a cached source, fetches/pulls/rebuilds, and prompts from a tty or prints restart guidance otherwise. | `TestUpdateSourceUsesInstallMarker` uses a fake HOME marker. |
