SHELL := /bin/bash
export PATH := /usr/local/go/bin:$(PATH)
export CGO_ENABLED := 0
export TMPDIR := $(CURDIR)/.tmp

BIN       := bin/relay
MODULE    := github.com/devpass/devpass-discord-relay
GOOS      ?= linux
GOARCH    ?= amd64

# Optional: run a specific integration test by name
T ?=

.PHONY: all build unit integration test clean restart-survival-host

all: build

## build: compile static binaries to bin/
build:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) \
	  go build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/relay
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) \
	  go build -trimpath -ldflags="-s -w" -o bin/dpmux ./cmd/dpmux
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) \
	  go build -trimpath -ldflags="-s -w" -o bin/fake-devpass-code ./cmd/fake-devpass-code
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) \
	  go build -trimpath -ldflags="-s -w" -o bin/mock-discord ./cmd/mock-discord

## unit: run all unit tests (no integration tag)
unit:
	@mkdir -p $(TMPDIR)
	CGO_ENABLED=0 go test -p 1 ./...

## integration: run integration tests in Docker
integration: build
	@if [ -n "$(T)" ]; then \
	  docker compose -f docker-compose.test.yml run --rm test go test -p 1 -tags integration -v -run "$(T)" ./test/integration/...; \
	else \
	  docker compose -f docker-compose.test.yml run --rm test go test -p 1 -tags integration -v ./test/integration/...; \
	fi

## test: run unit and integration
test: unit integration

## clean: remove build artifacts
clean:
	rm -rf bin/

## restart-survival-host: run relay as a disposable user unit, spawn through dpmux, restart relay, and prove the pane survives
restart-survival-host: build
	@unit="relay-test-$$$$"; data="$$(mktemp -d)"; state="$$data/state"; \
	  trap 'systemctl --user stop "$$unit" >/dev/null 2>&1 || true; tmux kill-server >/dev/null 2>&1 || true; rm -rf "$$data"' EXIT; \
	  mkdir -p "$$state"; XDG_DATA_HOME="$$data" ./bin/fake-devpass-code --auto </dev/null >/dev/null; \
	  systemd-run --user --unit "$$unit" env XDG_DATA_HOME="$$data" RELAY_STATE_DIR="$$state" RELAY_DEVPASS_DB="$$data/devpass-code/opencode.db" RELAY_DEVPASS_BIN="$(CURDIR)/bin/fake-devpass-code" FAKE_DP_FORCE_NEW_SESSION=1 DISCORD_TOKEN=host-proof DISCORD_CHANNEL_ID=host-proof DISCORD_API_URL=http://127.0.0.1:9 ./bin/relay; sleep 1; \
	  XDG_DATA_HOME="$$data" RELAY_STATE_DIR="$$state" RELAY_DEVPASS_DB="$$data/devpass-code/opencode.db" RELAY_DEVPASS_BIN="$(CURDIR)/bin/fake-devpass-code" FAKE_DP_FORCE_NEW_SESSION=1 ./bin/dpmux >/dev/null; session="$$(tmux list-sessions -F '#{session_name}' | grep '^dp-' | tail -1)"; test -n "$$session"; \
	  systemctl --user restart "$$unit"; tmux has-session -t "$$session"; \
	  echo "restart survival: PASS (relay lifecycle; dpmux birth scope survived)"
