#!/usr/bin/env bash
# install.sh installs the relay for the current user without touching secrets.
set -euo pipefail

ROOT="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
BIN_DIR="$HOME/.local/bin"
CONFIG_DIR="$HOME/.config/devpass-discord-relay"
UNIT_DIR="$HOME/.config/systemd/user"
ENV_FILE="$CONFIG_DIR/.env"
ENV_BACKUP="$ENV_FILE.install.bak"

mkdir -p "$BIN_DIR" "$CONFIG_DIR" "$UNIT_DIR"
if [[ -f "$ENV_FILE" ]]; then
  cp -p "$ENV_FILE" "$ENV_BACKUP"
fi

make -C "$ROOT" build
install -m 0755 "$ROOT/bin/relay" "$BIN_DIR/relay"
install -m 0755 "$ROOT/bin/dpmux" "$BIN_DIR/dpmux"
install -m 0644 "$ROOT/systemd/devpass-discord-relay.service" "$UNIT_DIR/devpass-discord-relay.service"

# A build/install must never replace credentials. Restore the exact original
# content after all installation steps, including a rerun of this script.
if [[ -f "$ENV_BACKUP" ]]; then
  cp -p "$ENV_BACKUP" "$ENV_FILE"
fi

systemctl --user daemon-reload
systemctl --user enable --now devpass-discord-relay.service
printf 'installed relay and dpmux in %s\n' "$BIN_DIR"
