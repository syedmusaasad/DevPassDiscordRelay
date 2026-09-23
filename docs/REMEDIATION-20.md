# Integrity and update-path remediation

| Finding | Fix | Evidence |
| --- | --- | --- |
| Marker ordering gates were weakened | Both dpmux marker tests use a child stub that records `marker_found` at its own startup and fail unless it was true. | `TestDpmux_MarkerBeforeSpawn` and `TestDpmux_NewWindowMarkerBeforeSpawn` each assert `marker_found=true`. |
| Isolation evidence did not match captured output | Five fresh, uncached Docker runs were captured in `docs/isolation-logs/run-1.log` through `run-5.log`; matrix PASS lines are copied verbatim. | Each log contains `--- PASS: TestInvariant4_ResumeRefusalRetry`; matching lines appear in `ISOLATION-MATRIX.md`. |
| Resolver evidence cited a no-op test | Removed `TestImport` and added a resolver test for explicit override, usable managed-path fallback, and PATH fallback with executable stubs. | `TestResolveTmuxBin`. |
| Approval ACK test did not distinguish recipes | The fake CLI emits distinct approved/rejected pane text. Separate approve and deny tests assert callback ACK, their expected text, and absence of the opposite text. | `TestInteraction_ApproveDrivesApproveRecipe`; `TestInteraction_DenyDrivesDenyRecipe`. |
| Resolver split the tmux literal | tmuxctl owns the literal `tmux` in its resolver PATH fallback. | `internal/tmuxctl/command.go`; `TestResolveTmuxBin`. |
| Spawn integration query changes were out of scope | Kept the descending queries because the tests genuinely require the session that owns the newly injected prompt, not an unrelated concurrently-created session. | The pre-remediation `make test` failure selected a non-owner and failed both `TestInvariant1_OwnTranscriptBinding` and `TestSpawn_ChannelMessageTriggersDpmux`; newest prompt-owner lookup makes both assert the actual injected session. |
| Update ignored the release tag | `dpmux update` fetches tags, checks out detached `latest`, rebuilds, reinstalls binaries, then prompts on a TTY or prints the restart command otherwise. | `TestDpmuxUpdate_ChecksOutLatestTag` creates a bare remote where `latest` differs from branch tip and asserts checkout at the tagged commit. |
