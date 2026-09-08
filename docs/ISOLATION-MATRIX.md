# Phase 18 isolation matrix

The integration image was rebuilt **uncached** before this matrix:

```text
docker compose -f docker-compose.test.yml build --no-cache
```

Each run used a fresh `docker compose ... run --rm` container and began with
`unset TMUX; rm -rf /tmp/tmux-*`, so there was no pre-existing tmux server in
its namespace. `-count=1` disabled Go test-cache reuse.

Command for every run:

```text
docker compose -f docker-compose.test.yml run --rm test sh -c 'unset TMUX; rm -rf /tmp/tmux-*; go test -p 1 -count=1 -tags integration -v -run "^TestInvariant4_ResumeRefusalRetry$" ./test/integration/...'
```

RUN 1/5: PASS

Command: `docker compose -f docker-compose.test.yml run --rm test sh -c 'unset TMUX; rm -rf /tmp/tmux-*; go test -p 1 -count=1 -tags integration -v -run "^TestInvariant4_ResumeRefusalRetry$" ./test/integration/...'`

Exit code: 0. `--- PASS: TestInvariant4_ResumeRefusalRetry (2.74s)`

RUN 2/5: PASS

Command: `docker compose -f docker-compose.test.yml run --rm test sh -c 'unset TMUX; rm -rf /tmp/tmux-*; go test -p 1 -count=1 -tags integration -v -run "^TestInvariant4_ResumeRefusalRetry$" ./test/integration/...'`

Exit code: 0. `--- PASS: TestInvariant4_ResumeRefusalRetry (2.74s)`

RUN 3/5: PASS

Command: `docker compose -f docker-compose.test.yml run --rm test sh -c 'unset TMUX; rm -rf /tmp/tmux-*; go test -p 1 -count=1 -tags integration -v -run "^TestInvariant4_ResumeRefusalRetry$" ./test/integration/...'`

Exit code: 0. `--- PASS: TestInvariant4_ResumeRefusalRetry (2.75s)`

RUN 4/5: PASS

Command: `docker compose -f docker-compose.test.yml run --rm test sh -c 'unset TMUX; rm -rf /tmp/tmux-*; go test -p 1 -count=1 -tags integration -v -run "^TestInvariant4_ResumeRefusalRetry$" ./test/integration/...'`

Exit code: 0. `--- PASS: TestInvariant4_ResumeRefusalRetry (2.74s)`

RUN 5/5: PASS

Command: `docker compose -f docker-compose.test.yml run --rm test sh -c 'unset TMUX; rm -rf /tmp/tmux-*; go test -p 1 -count=1 -tags integration -v -run "^TestInvariant4_ResumeRefusalRetry$" ./test/integration/...'`

Exit code: 0. `--- PASS: TestInvariant4_ResumeRefusalRetry (2.45s)`
