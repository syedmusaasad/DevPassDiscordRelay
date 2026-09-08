# Phase 18 isolation matrix

The integration image was rebuilt **uncached** once before this five-run matrix:

```text
docker compose -f docker-compose.test.yml build --no-cache
```

Each invocation below used `docker compose ... run --rm`, creating a fresh
container. Each test creates its own fresh `TMUX_TMPDIR` and explicitly removes
inherited `TMUX`; therefore every run starts with no pre-existing tmux server in
its namespace. `-count=1` disables Go test-cache reuse.

RUN 1/5: PASS

Exact command:

```text
docker compose -f docker-compose.test.yml run --rm test go test -p 1 -count=1 -tags integration -v -run '^TestInvariant4_ResumeRefusalRetry$' ./test/integration/...
```

Exit code: 0

```text
--- PASS: TestInvariant4_ResumeRefusalRetry (2.76s)
PASS
ok  github.com/devpass/devpass-discord-relay/test/integration  2.767s
```

RUN 2/5: PASS

Exact command:

```text
docker compose -f docker-compose.test.yml run --rm test go test -p 1 -count=1 -tags integration -v -run '^TestInvariant4_ResumeRefusalRetry$' ./test/integration/...
```

Exit code: 0

```text
--- PASS: TestInvariant4_ResumeRefusalRetry (2.44s)
PASS
ok  github.com/devpass/devpass-discord-relay/test/integration  2.441s
```

RUN 3/5: PASS

Exact command:

```text
docker compose -f docker-compose.test.yml run --rm test go test -p 1 -count=1 -tags integration -v -run '^TestInvariant4_ResumeRefusalRetry$' ./test/integration/...
```

Exit code: 0

```text
--- PASS: TestInvariant4_ResumeRefusalRetry (2.44s)
PASS
ok  github.com/devpass/devpass-discord-relay/test/integration  2.443s
```

RUN 4/5: PASS

Exact command:

```text
docker compose -f docker-compose.test.yml run --rm test go test -p 1 -count=1 -tags integration -v -run '^TestInvariant4_ResumeRefusalRetry$' ./test/integration/...
```

Exit code: 0

```text
--- PASS: TestInvariant4_ResumeRefusalRetry (2.46s)
PASS
ok  github.com/devpass/devpass-discord-relay/test/integration  2.461s
```

RUN 5/5: PASS

Exact command:

```text
docker compose -f docker-compose.test.yml run --rm test go test -p 1 -count=1 -tags integration -v -run '^TestInvariant4_ResumeRefusalRetry$' ./test/integration/...
```

Exit code: 0

```text
--- PASS: TestInvariant4_ResumeRefusalRetry (2.45s)
PASS
ok  github.com/devpass/devpass-discord-relay/test/integration  2.453s
```
