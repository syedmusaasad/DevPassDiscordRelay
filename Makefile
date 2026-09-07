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

## restart-survival-host: verify a disposable user unit does not kill tmux sessions
restart-survival-host: build
	@unit="relay-test-$$$$"; session="$$unit-tmux"; data="$$(mktemp -d)"; \
	  trap 'systemctl --user stop "$$unit" >/dev/null 2>&1 || true; tmux kill-session -t "$$session" >/dev/null 2>&1 || true; rm -rf "$$data"' EXIT; \
	  systemd-run --user --unit "$$unit" --collect --remain-after-exit env XDG_DATA_HOME="$$data" tmux new-session -d -s "$$session" 'sleep 60'; \
	  systemctl --user restart "$$unit" >/dev/null; \
	  tmux has-session -t "$$session"; \
	  echo "restart survival: PASS"
