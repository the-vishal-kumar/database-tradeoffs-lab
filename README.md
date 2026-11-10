# database-tradeoffs-lab

[![ci](https://github.com/the-vishal-kumar/database-tradeoffs-lab/actions/workflows/ci.yml/badge.svg)](https://github.com/the-vishal-kumar/database-tradeoffs-lab/actions/workflows/ci.yml)

Runnable demos for a post series that compares database concepts side by side: isolation levels, locking strategies, consistency models, storage engines and more. Every claim in a post comes with a demo here that shows the difference happening, and a test that pins the result.

## Run it

You need Docker. Go is optional (the demos run in a Go container).

```bash
make isolation   # 01: isolation levels
make test        # tests that pin every result
make down        # stop and delete the databases
```

Without `make`: `docker compose up -d --wait postgres && docker compose run --rm go go run ./cmd/isolation -details`.

With Go 1.27 installed: `make up`, then `go run ./cmd/isolation -details`. It connects to `localhost:55432` (bound to localhost only).

## Demos

| # | Topic | Compares | Status |
|---|---|---|---|
| 01 | [Isolation levels](01-isolation-levels/) | Read Uncommitted vs Read Committed vs Repeatable Read vs Serializable in PostgreSQL 17, across ten anomalies and fixes | done, [results](01-isolation-levels/results.md) |


## Layout

```
compose.yaml              Postgres 17 (+ a Go toolchain container, profile "tools")
cmd/isolation/            prints the isolation matrix as Markdown
internal/isolation/       the scenarios and the tests that pin them
01-isolation-levels/      what each scenario does, and measured results
```

## Licence

MIT
