# Phase 21 remediation

## Marker visibility under `systemd-run --scope`

**Root cause:** `NewBirthCommand` forwarded only `TMUX_TMPDIR` when it wrapped a
fresh tmux-server birth in `systemd-run --user --scope`.  The manager launched
the tmux client without dpmux's `RELAY_STATE_DIR`, so the process in the new
server could inspect a different/default state directory and miss its pending
marker.  The marker itself was also written in place, which exposed a
create/truncate window to an immediately born child.

**Fix:** pending markers are now written to a unique temporary file, fsynced,
atomically renamed into place, and followed by a parent-directory fsync before
any tmux birth.  The scope now forwards `TMUX_TMPDIR`, `RELAY_STATE_DIR`, and
`HOME` through both `--setenv` and the executed tmux client's explicit `env`.
`TestDpmux_MarkerBeforeSpawn_SystemdScope` probes `systemd-run --user`, skips
only when that probe is unavailable, forces the scope path, and requires its
child to report `marker_found=true` with the expected state directory.

## Readiness-probe injection

**Root cause:** the readiness test dispatched as soon as a Discord thread was
created, using a fixed 300 ms sleep to guess when the relay gateway had finished
its READY handshake.  A dispatch queued before a connected gateway consumer
could be lost, leaving no prompt to observe; this was misdiagnosed as a prompt
readiness race.

**Fix:** `TestInject_ReadinessProbe` now waits until the relay has durably
recorded its claimed session in `sessions.json` before dispatching.  The fake
CLI still swallows all input until its delayed `Ask anything...` signature, so
the test continues to prove the actual readiness contract rather than hiding it
with a longer timeout.

## Restart-survival proof

**Root cause:** the test previously stopped and replaced a transient relay unit
when `systemctl --user restart` failed.  Pane survival after a replacement did
not prove survival across a restart of the original unit.

**Fix:** a failed restart of the original disposable user unit is now an
immediate test failure; no substitute unit is started.

## Three consecutive full-suite runs

The commands below were run consecutively after the fixes.

1. `make test > /tmp/p21-t1.log 2>&1` — PASS
2. `make test > /tmp/p21-t2.log 2>&1` — PASS
3. `make test > /tmp/p21-t3.log 2>&1` — PASS

## Update after phase 22 (readiness supersession)

The readiness-probe fix above (waiting for `sessions.json`) was later found
insufficient: the test could still dispatch a gateway event before the relay
had an IDENTIFY/READY consumer. Phase 22 (commits after 5e40049) replaced it
with a relay-exposed gateway readiness signal and a mock-discord that holds
dispatches until a session is READY; see docs/REMEDIATION-22.md for the
authoritative root cause and evidence. The marker-under-scope and strict
restart fixes from 5e40049 are unchanged.

## Re-verification addendum: TestDpmux_NewWindowMarkerBeforeSpawn host-session flake

Root cause: the test called `tmux -L default kill-server` and immediately ran
`new-session`. kill-server returns before the old server process has exited, so
new-session occasionally failed with "server exited unexpectedly" (seen once in
the first re-verification run, 0.01s). Fix: after kill-server the test now waits
(bounded, fatal on expiry) until no server answers `list-sessions` on the socket
before creating the host session. No assertion was loosened and no timeout on
the marker check was raised. After the fix the three marker tests passed in 5
consecutive `make integration` runs.
