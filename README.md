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
make replicas    # 08: a streaming replica: lag, stale reads, synchronous commit
make partition   # 09: one big table vs monthly partitions
make twosys      # 10: two databases: dual write vs 2PC vs saga vs outbox
make livemigrate # 11: schema changes under load: blocking vs online
make engines     # 12: B+tree vs LSM tree, embedded (no server)
make acid        # 13: transfers in PostgreSQL, MongoDB and DynamoDB Local, with and without transactions
make pgdyn       # 14: the same orders in PostgreSQL and DynamoDB: planned vs unplanned queries
make dr          # 15: backups and a disaster drill: dump vs point-in-time recovery
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
| 08 | [Read replicas](08-replicas/) | A PostgreSQL streaming replica: lag under load and after a big write, stale reads after your own write at each `synchronous_commit` level, and commits when the synchronous replica is gone | done, [results](08-replicas/results.md) |
| 09 | [Partitioning](09-partitioning/) | One table vs monthly partitions (6M rows): partition pruning, queries without the key, and `DELETE` vs `DROP` for the oldest month (time, WAL, space returned) | done, [results](09-partitioning/results.md) |
| 10 | [Two systems](10-two-systems/) | Orders and payments in two databases with crashes injected: dual write vs two-phase commit vs saga vs outbox, and what an in-doubt transaction blocks | done, [results](10-two-systems/results.md) |
| 11 | [Live migrations](11-live-migrations/) | Schema changes on a 5M-row table under load: `ALTER … TYPE` vs expand/contract, `CREATE INDEX` vs `CONCURRENTLY`, constant vs volatile defaults, and the lock queue with and without `lock_timeout` | done, [results](11-live-migrations/results.md) |
| 12 | [Storage engines](12-storage-engines/) | The same workload on a B+tree (bbolt) and an LSM tree (Pebble, with and without Bloom filters): write throughput, write amplification, point reads, range scans, space | done, [results](12-storage-engines/results.md) |
| 13 | [ACID](13-acid/) | Transfers between 100 accounts in PostgreSQL, MongoDB and DynamoDB Local, with and without transactions, with crashes: totals, negative balances, and what plain vs snapshot readers see | done, [results](13-acid/results.md) |
| 14 | [Postgres or DynamoDB](14-postgres-or-dynamodb/) | The same 200K orders in PostgreSQL and a DynamoDB single-table design: the designed access pattern, an ad-hoc report (items read, capacity), and adding an access pattern later | done, [results](14-postgres-or-dynamodb/results.md) |
| 15 | [Backup and restore](15-backup-restore/) | A 1 GB database: `pg_dump` and restore times, a base backup plus WAL archiving, then the server and its disk destroyed mid-write and rebuilt: data lost and time to recover | done, [results](15-backup-restore/results.md) |

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
cmd/replicas/, internal/replicas/  the replica demo and its test (needs profile "replicas")
08-replicas/              replica setup scripts, what it does, and measured results
cmd/partition/, internal/partition/  the partitioning demo and its test
09-partitioning/          what it does, and measured results
cmd/twosys/, internal/twosys/  the two-systems demo and its test
10-two-systems/           what it does, and measured results
cmd/livemigrate/, internal/migrate/  the live-migration demo and its test
11-live-migrations/       what it does, and measured results
cmd/engines/, internal/engines/  the storage-engine comparison and its test
12-storage-engines/       what it does, and measured results
cmd/acid/, internal/acid/  the ACID demo and its test (needs profile "acid")
13-acid/                  what it does, and measured results
cmd/pgdyn/, internal/pgdyn/  the Postgres-vs-DynamoDB demo and its test (needs DynamoDB Local)
14-postgres-or-dynamodb/  what it does, and measured results
15-backup-restore/        the disaster-drill script, what it does, and measured results
```

## Licence

MIT
