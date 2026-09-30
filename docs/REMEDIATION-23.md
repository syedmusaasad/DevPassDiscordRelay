# Remediation 23: pre-publish deployment and security fixes

Findings come from the phase-9 reviewer-a and phase-10 reviewer-a/b reviews. No
assertion was loosened, no test skipped, no timeout raised. Logs are under
`docs/flake-logs/` and quoted verbatim below.

| # | Finding | Fix | Test evidence |
|---|---------|-----|---------------|
| 1 | Relay fail-open when DISCORD_USER_ID unset | `cmd/relay/main.go` returns an error naming DISCORD_USER_ID before opening anything (non-zero exit, one log line); `inject.ShouldAcceptAuthor` returns false for an empty configured ID | `TestRelay_RefusesWithoutUserID` (unset/empty/blank), `TestShouldAcceptAuthor_EmptyIDRejectsAll` |
| 2 | Unit cannot find dpmux on the user manager PATH | `systemd/devpass-discord-relay.service`: `Environment=RELAY_DPMUX_BIN=%h/.local/bin/dpmux`, `Environment=PATH=%h/.local/bin:/usr/local/bin:/usr/bin:/bin` | `TestInstalledUnit_ResolvesDpmux` |
| 3 | `dpmux update` overwrote a running binary (ETXTBSY) | `replaceFile` in `cmd/dpmux/main.go`: temp file in the same dir, fsync, chmod, rename | `TestDpmuxUpdate_ReplacesRunningBinary` plus a negative control |
| 4 | `git fetch --tags` ignores a moved `latest` tag | `git fetch --tags --force` | `TestDpmuxUpdate_FollowsMovedLatestTag`; `TestDpmuxUpdate_ChecksOutLatestTag` still passes |
| 5 | Telemetry only at startup | `telemetry.Run`/`RunWithClock` poll every 10 min with an injectable clock and send once per UTC day; relay starts it as a goroutine tied to ctx | `TestTelemetry_SendsAgainAfterDayBoundary` |
| 6 | Docs overclaim | README (no non-root claim; fail-closed user gate; telemetry disclosed), SETUP (`~/.config/devpass-discord-relay/.env` is the service file; repo-root `.env` is a dev fallback), CONTRIBUTING (REFUSE_FIRST_RESUME refuses every `--session` launch while set), CHANGELOG (telemetry wording) | EXIT greps |

## Test notes (honest details)

- **Finding 1.** Every existing integration relay launch now sets
  `DISCORD_USER_ID` (three launches lacked it: one `mirror_test.go` launch, the
  `startRelay` helper, and one `inject_test.go` launch). That is required by
  the new fail-closed behaviour, not a weakening: none of those tests asserts on
  author filtering. `TestRelay_RefusesWithoutUserID` runs the relay with a
  clean env and cwd so no `.env` fallback can supply the value.
- **Finding 2.** `TestInstalledUnit_ResolvesDpmux` runs the real `install.sh` in a
  temp HOME. install.sh gained three test-only env hooks
  (`RELAY_INSTALL_SRC_BIN`, `RELAY_INSTALL_SKIP_BUILD`,
  `RELAY_INSTALL_SKIP_SYSTEMD`; all unset in normal use) so it installs a stub
  dpmux and does not touch a user manager. The test parses the *installed unit
  file's* `Environment=` and `ExecStart=` lines (expanding `%h`), overrides PATH
  to `/usr/bin:/bin`, and runs the relay under `systemd-run --user`. The stub
  records `$0`; the test requires it to equal the installed
  `~/.local/bin/dpmux`. It ran (not skipped) in the Docker disposable user
  manager. It skips only if `systemd-run --user` is unavailable, logging the
  probe failure.
- **Finding 3.** The first draft of the running-binary test used `sleep`, which
  is a multicall binary here, exited immediately and proved nothing (it also
  passed against the old code). It now copies `sh` to both install paths, keeps
  each running (`sh -c 'read _'`), asserts the precondition that the running file
  cannot be opened for writing, and checks no temp file is left behind. Negative
  control against the old `os.WriteFile` install: fails with `text file busy`
  (`phase23-update-negative-control.log`).
- **Finding 4.** Before the fix, `FollowsMovedLatestTag` fails at
  `git fetch --tags` (git rejects the clobbering of the existing tag).
- **Finding 5.** The test starts `RunWithClock` at 23:59 UTC, waits for one send,
  confirms repeated polls on the same day send nothing, advances the clock two
  minutes past midnight UTC without restarting, and asserts exactly a second send.

## Evidence (verbatim)

### Targeted integration (`make integration T=...`), `docs/flake-logs/phase23-integration-targeted.log`

```
=== RUN   TestRelay_RefusesWithoutUserID
=== RUN   TestRelay_RefusesWithoutUserID/unset
    deploy_test.go:64: exit=1 output="2026/09/30 16:28:36 main.go:55: relay: .env load: no .env found in [/tmp/TestRelay_RefusesWithoutUserIDunset2057430/001/.config/devpass-discord-relay/.env .env] (continuing with environment)\n2026/09/30 16:28:36 main.go:48: relay: DISCORD_USER_ID must be set: refusing to start without a single allowed Discord user"
=== RUN   TestRelay_RefusesWithoutUserID/empty
    deploy_test.go:64: exit=1 output="2026/09/30 16:28:36 main.go:55: relay: .env load: no .env found in [/tmp/TestRelay_RefusesWithoutUserIDempty2784086555/001/.config/devpass-discord-relay/.env .env] (continuing with environment)\n2026/09/30 16:28:36 main.go:48: relay: DISCORD_USER_ID must be set: refusing to start without a single allowed Discord user"
=== RUN   TestRelay_RefusesWithoutUserID/blank
    deploy_test.go:64: exit=1 output="2026/09/30 16:28:36 main.go:55: relay: .env load: no .env found in [/tmp/TestRelay_RefusesWithoutUserIDblank2583229347/001/.config/devpass-discord-relay/.env .env] (continuing with environment)\n2026/09/30 16:28:36 main.go:48: relay: DISCORD_USER_ID must be set: refusing to start without a single allowed Discord user"
--- PASS: TestRelay_RefusesWithoutUserID (0.01s)
    --- PASS: TestRelay_RefusesWithoutUserID/unset (0.01s)
    --- PASS: TestRelay_RefusesWithoutUserID/empty (0.00s)
    --- PASS: TestRelay_RefusesWithoutUserID/blank (0.00s)
=== RUN   TestInstalledUnit_ResolvesDpmux
--- PASS: TestInstalledUnit_ResolvesDpmux (1.34s)
PASS
ok  	github.com/devpass/devpass-discord-relay/test/integration	1.359s
```

### TestShouldAcceptAuthor_EmptyIDRejectsAll, `docs/flake-logs/phase23-inject.log`

```
=== RUN   TestShouldAcceptAuthor_EmptyIDRejectsAll
--- PASS: TestShouldAcceptAuthor_EmptyIDRejectsAll (0.00s)
PASS
ok  	github.com/devpass/devpass-discord-relay/internal/inject	0.003s
```

### dpmux update tests, `docs/flake-logs/phase23-update.log`

```
=== RUN   TestDpmuxUpdate_ChecksOutLatestTag
--- PASS: TestDpmuxUpdate_ChecksOutLatestTag (0.07s)
=== RUN   TestDpmuxUpdate_FollowsMovedLatestTag
--- PASS: TestDpmuxUpdate_FollowsMovedLatestTag (0.12s)
=== RUN   TestDpmuxUpdate_ReplacesRunningBinary
--- PASS: TestDpmuxUpdate_ReplacesRunningBinary (0.07s)
PASS
ok  	github.com/devpass/devpass-discord-relay/cmd/dpmux	0.266s
```

### Negative control, old `os.WriteFile` install, `docs/flake-logs/phase23-update-negative-control.log`

```
    main_test.go:156: update with running binaries: install rebuilt relay: open /tmp/TestDpmuxUpdate_ReplacesRunningBinary2829669078/001/.local/bin/relay: text file busy
--- FAIL: TestDpmuxUpdate_ReplacesRunningBinary (0.08s)
FAIL
FAIL	github.com/devpass/devpass-discord-relay/cmd/dpmux	0.082s
FAIL
```

### TestTelemetry_SendsAgainAfterDayBoundary, `docs/flake-logs/phase23-telemetry.log`

```
=== RUN   TestTelemetry_SendsAgainAfterDayBoundary
--- PASS: TestTelemetry_SendsAgainAfterDayBoundary (0.21s)
PASS
ok  	github.com/devpass/devpass-discord-relay/internal/telemetry	0.214s
```

### Full `make test`, `docs/flake-logs/phase23-make-test.log` (exit 0, zero SKIPs)

```
ok  	github.com/devpass/devpass-discord-relay/cmd/dpmux	0.349s
ok  	github.com/devpass/devpass-discord-relay/internal/discord	(cached)
ok  	github.com/devpass/devpass-discord-relay/internal/inject	(cached)
ok  	github.com/devpass/devpass-discord-relay/internal/mirror	1.390s
ok  	github.com/devpass/devpass-discord-relay/internal/state	0.004s
ok  	github.com/devpass/devpass-discord-relay/internal/telemetry	0.256s
ok  	github.com/devpass/devpass-discord-relay/internal/tmuxctl	0.005s
ok  	github.com/devpass/devpass-discord-relay/internal/transcript	0.316s
--- PASS: TestRelay_RefusesWithoutUserID (0.03s)
--- PASS: TestInstalledUnit_ResolvesDpmux (1.42s)
PASS
ok  	github.com/devpass/devpass-discord-relay/test/integration	66.496s
```
