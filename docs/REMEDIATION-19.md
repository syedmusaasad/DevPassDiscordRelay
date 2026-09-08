# Milestone 5/6 review remediation

| Finding | Fix | Evidence |
| --- | --- | --- |
| Restart survival | `TestRestartSurvival_DisposableUnit` probes and logs user-manager capability, starts the relay in a disposable user unit, spawns through relay → dpmux, restarts the relay unit, and checks the pane. Manager-less containers log the probe failure and defer proof to the host target. | The named integration test is run twice. In this Docker image `systemd-run` is unavailable; host target output: `Running as unit: relay-test-1132920.service`, dpmux reports `systemd-user-scope`, then `restart survival: PASS (relay lifecycle; dpmux birth scope survived)`. |
| Host proof recipe | `restart-survival-host` starts `relay` as `relay-test-<pid>`, births a dpmux session with isolated data/socket paths, restarts relay, and checks that session. | Target prints `restart survival: PASS (relay lifecycle; dpmux birth scope survived)`. |
| Interaction ACKs | The adapter issues a deferred message-update callback before watcher keystroke handling. | `TestInteraction_Ack` records callback type 6 in mock Discord and observes the pane resolution. |
| Per-prompt controls | Prompt-post state resets after an interaction is finalized or the prompt leaves the pane. | `TestApproval_SecondPromptGetsButtons` finalizes prompt one then observes prompt two controls. |
| Component throttle/backoff | Component sends/edits use the watcher throttle and the same bounded 429 retry helpers as ordinary writes. | Unit throttle coverage and component-path helper implementation. |
| tmux resolution | Watcher liveness/capture/teardown and injector capture/send resolve tmux through `tmuxctl.ResolveTmuxBin`. | Resolver unit tests plus production invocation audit. |
| Telemetry shape | Telemetry uses `sha256(username)[:12]`, sends only `{username,version}`, stamps successful UTC-day sends, and quietly ignores failures. | `internal/telemetry` unit tests cover exact body, daily-once, and quiet failure. |
| Isolation startup log | Relay logs the chosen systemd scope or direct fallback at watcher startup. | Startup log includes `relay: tmux isolation mode:`. |
| Installed dpmux update | `install.sh` writes an install-source marker; `dpmux update` reads it, otherwise clones a cached source, fetches/pulls/rebuilds, and prompts from a tty or prints restart guidance otherwise. | `TestUpdateSourceUsesInstallMarker` uses a fake HOME marker. |
