# database-tradeoffs-lab

[![ci](https://github.com/the-vishal-kumar/database-tradeoffs-lab/actions/workflows/ci.yml/badge.svg)](https://github.com/the-vishal-kumar/database-tradeoffs-lab/actions/workflows/ci.yml)

Runnable demos for a post series that compares database concepts side by side: isolation levels, locking strategies, consistency models, storage engines and more. Every claim in a post comes with a demo here that shows the difference happening, and a test that pins the result.

## Run it

You need Docker. Go is optional (the demos run in a Go container).

```bash
make isolation   # 01: isolation levels
make slowquery   # 02: why queries slow down as the table grows (several minutes)
make test        # tests that pin every result
make down        # stop and delete the databases
```

Without `make`: `docker compose up -d --wait postgres && docker compose run --rm go go run ./cmd/isolation -details`.

With Go 1.27 installed: `make up`, then `go run ./cmd/isolation -details`. It connects to `localhost:55432` (bound to localhost only).

## Demos

| # | Topic | Compares | Status |
|---|---|---|---|
| 01 | [Isolation levels](01-isolation-levels/) | Read Uncommitted vs Read Committed vs Repeatable Read vs Serializable in PostgreSQL 17, across ten anomalies and fixes | done, [results](01-isolation-levels/results.md) |
| 02 | [Slow queries at scale](02-slow-queries/) | Why a query that uses an index still slows down as the table grows: six causes, measured at 10K, 1M and 10M rows in PostgreSQL 17 | done, [results](02-slow-queries/results.md) |


## Layout

```
compose.yaml              Postgres 17 (+ a Go toolchain container, profile "tools")
cmd/isolation/            prints the isolation matrix as Markdown
internal/isolation/       the scenarios and the tests that pin them
01-isolation-levels/      what each scenario does, and measured results
cmd/slowquery/            measures the slow-query scenarios at several table sizes
internal/slowquery/       the scenarios and a test that checks each effect at 200K rows
02-slow-queries/          what each scenario does, and measured results
```

## Licence

MIT
