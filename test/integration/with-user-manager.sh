#!/bin/sh
# Give integration tests a disposable user manager. This runs only in Docker's
# throwaway test HOME and never exposes or contacts the host manager.
set -eu

runtime="${XDG_RUNTIME_DIR:-/tmp/testuser-runtime}"
mkdir -p "$runtime"
chmod 700 "$runtime"
export XDG_RUNTIME_DIR="$runtime"
export DBUS_SESSION_BUS_ADDRESS="unix:path=$runtime/bus"

dbus-daemon --session --address="$DBUS_SESSION_BUS_ADDRESS" --fork --nopidfile
systemd --user --log-target=console >/tmp/testuser-systemd.log 2>&1 &
manager_pid=$!
cleanup() {
  kill "$manager_pid" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

ready=0
for _ in $(seq 1 50); do
  if systemctl --user show-environment >/tmp/testuser-manager-probe.log 2>&1; then
    ready=1
    break
  fi
  sleep 0.1
done
if [ "$ready" -ne 1 ]; then
  cat /tmp/testuser-manager-probe.log >&2 || true
  cat /tmp/testuser-systemd.log >&2 || true
  exit 1
fi

exec "$@"
