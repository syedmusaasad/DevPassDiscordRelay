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

## restart-survival-host: run the relay lifecycle proof against the reachable host user manager
restart-survival-host: build
	@PATH="$(CURDIR)/bin:$$PATH" go test -p 1 -count=1 -tags integration -v -run '^TestRestartSurvival_DisposableUnit$$' ./test/integration/...
