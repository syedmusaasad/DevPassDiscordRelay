# Contributing Guide

Build, test, and verify DevPassDiscordRelay using isolated local harnesses.

## Test Commands

Run the test suite using Makefile targets:

- `make unit`: Runs unit tests locally using `go test -p 1 ./...`.
- `make integration`: Builds the test container and executes integration tests inside Docker via `docker compose -f docker-compose.test.yml run --rm test`.
- `make test`: Executes the complete test sequence by running `make unit` followed by `make integration`.
- `make restart-survival-host`: Runs the daemon lifecycle survival test against the reachable host systemd user manager.

Filter specific integration tests with the `T=` parameter:

```bash
make integration T=TestInject_ReadinessProbe
```

You can pass any regular expression or test function name to `T=`.

## Test Isolation Rules

All automated tests adhere to strict environment isolation boundaries:

- Tests never touch the real HOME directory. Integration tests execute inside Docker as non-root user `testuser` with throwaway `$HOME=/home/testuser`.
- Tests never touch real Discord services. Discord API calls connect to the local mock Discord double. No outbound network requests reach Discord public servers.
- Database access uses isolated paths. Tests set `XDG_DATA_HOME` or `RELAY_DEVPASS_DB` to temporary directories, leaving production SQLite stores untouched.

## Fake CLI Test Knobs

The `cmd/fake-devpass-code` binary replicates `devpass-code` version 1.18.11 behavior for deterministic testing. Control its behavior using the following environment variables:

- `FAKE_DP_REFUSE_FIRST_RESUME=1`: Forces refusal on the first `--session` resume attempt, returning `Error: Session not found`.
- `FAKE_DP_READY_DELAY_MS=<n>`: Delays displaying the `Ask anything...` ready signature by the specified milliseconds.
- `FAKE_DP_TRANSCRIPT_DELAY_MS=<n>`: Delays inserting the session database row by the specified milliseconds.
- `FAKE_DP_SWALLOW_UNTIL_READY=1`: Drops keystrokes received before the prompt reaches readiness.
- `FAKE_DP_SCRIPT=<path>`: Specifies a path to a JSON file containing predetermined turns, formatted as `[{"prompt": "...", "reply": "..."}]`.
- `FAKE_DP_FORCE_NEW_SESSION=1`: Skips auto-resume and generates a fresh session identifier.
- `FAKE_DP_PERMISSION_PROMPT=1`: Renders interactive permission approval prompts in the pane.

## Mock Discord Control Endpoints

The `cmd/mock-discord` binary provides a test double for Discord Gateway and REST API operations. The server exposes control endpoints over HTTP on port 9876:

- `POST /control/dispatch`: Injects a Discord gateway event, such as `MESSAGE_CREATE` or `INTERACTION_CREATE`. The endpoint holds dispatch until the connected relay completes IDENTIFY, receives READY, and begins heartbeating.
- `GET /control/sent`: Returns a JSON array of all REST requests sent by the relay.
- `POST /control/ratelimit`: Configures synthetic HTTP 429 rate limit responses for a given endpoint path with a specified `retry_after` duration and response count.
- `GET /control/reqcount`: Retrieves total request counts received across endpoint paths.
- `DELETE /control/sent`: Clears the captured request history between test assertions.
