SHELL := /bin/bash
export PATH := /usr/local/go/bin:$(PATH)
export CGO_ENABLED := 0

BIN       := bin/relay
MODULE    := github.com/devpass/devpass-discord-relay
GOOS      ?= linux
GOARCH    ?= amd64

# Optional: run a specific integration test by name
T ?=

.PHONY: all build unit integration test clean

all: build

## build: compile static relay binary to bin/relay and bin/dpmux
build:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) \
	  go build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/relay
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) \
	  go build -trimpath -ldflags="-s -w" -o bin/dpmux ./cmd/dpmux

## unit: run all unit tests (no integration tag)
unit:
	CGO_ENABLED=0 go test -p 1 ./...

## integration: run integration tests in Docker
integration:
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
