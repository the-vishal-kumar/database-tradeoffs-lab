# database-tradeoffs-lab

[![ci](https://github.com/the-vishal-kumar/database-tradeoffs-lab/actions/workflows/ci.yml/badge.svg)](https://github.com/the-vishal-kumar/database-tradeoffs-lab/actions/workflows/ci.yml)

Runnable demos for a post series that compares database concepts side by side: isolation levels, locking strategies, consistency models, storage engines and more. Every claim in a post comes with a demo here that shows the difference happening, and a test that pins the result.

## Run it

You need Docker. Go and Node are optional (the demos run in containers).

```bash
make isolation   # 01: isolation levels
make slowquery   # 02: why queries slow down as the table grows (several minutes)
make mongo       # 03: Mongoose vs $jsonSchema vs one collection for everything
make cache       # 04: cache invalidation, races and stampedes
make locking     # 05: lock the row vs retry on conflict
make dlock       # 06: why a Redis lock is not enough
make longtx      # 07: one long transaction vs VACUUM and purge (PostgreSQL, MySQL)
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
| 03 | [MongoDB schemas](03-mongodb-schemas/) | Mongoose validation vs MongoDB's `$jsonSchema` validator vs one collection for everything (TTL, indexes, the 16 MB limit), on MongoDB 8.2 | done, [results](03-mongodb-schemas/results.md) |
| 04 | [Cache consistency](04-cache-consistency/) | Stale reads, the delete-on-write race, rolled-back writes, stampedes and TTL jitter, with Redis 7.4 in front of PostgreSQL 17 | done, [results](04-cache-consistency/results.md) |
| 05 | [Locking](05-locking/) | `SELECT … FOR UPDATE` vs optimistic version checks vs an atomic `UPDATE` vs Serializable, at high and low contention, plus overselling the last tickets, on PostgreSQL 17 | done, [results](05-locking/results.md) |
| 06 | [Redis locks](06-redis-locks/) | A Redis lease lock vs fencing tokens vs PostgreSQL advisory locks vs claiming a row, when the lock holder pauses past its lease; plus a cron job on 3 instances | done, [results](06-redis-locks/results.md) |
| 07 | [Long transactions](07-long-transactions/) | One idle transaction vs PostgreSQL's VACUUM and MySQL InnoDB's purge: dead rows, table bloat, slower reads, and the timeout that limits it | done, [results](07-long-transactions/results.md) |

## Layout

```
compose.yaml              Postgres 17, MongoDB 8.2, Redis 7.4 and MySQL 8.4 (+ Go and Node containers, profile "tools")
cmd/isolation/            prints the isolation matrix as Markdown
internal/isolation/       the scenarios and the tests that pin them
01-isolation-levels/      what each scenario does, and measured results
cmd/slowquery/            measures the slow-query scenarios at several table sizes
internal/slowquery/       the scenarios and a test that checks each effect at 200K rows
02-slow-queries/          what each scenario does, and measured results
03-mongodb-schemas/       the MongoDB demo (Node, Mongoose 8), its test and measured results
cmd/cachedemo/, internal/cache/  the cache demo and its test
04-cache-consistency/     what each scenario does, and measured results
cmd/lockdemo/, internal/locking/  the locking benchmark and its test
05-locking/               what each strategy does, and measured results
cmd/dlockdemo/, internal/dlock/  the distributed-lock demo and its test
06-redis-locks/           what each scenario does, and measured results
cmd/longtx/, internal/longtx/  the long-transaction demo (PostgreSQL and MySQL) and its test
07-long-transactions/     what it does, and measured results
```

## Licence

MIT
