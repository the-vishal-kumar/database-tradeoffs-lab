# 11 — Changing a schema without stopping traffic

Companion to the post *"How do you migrate a live database without downtime?"*

```bash
make livemigrate
# or: docker compose up -d --wait postgres && docker compose run --rm go go run ./cmd/livemigrate
# flag: -rows 5000000
```

A 5,000,000-row table. While each change runs, 8 clients loop on a primary-key read plus a one-row update, and every request's latency is recorded: the slowest request is how long users were stalled. Results: [results.md](results.md).

| Change | One step (blocking) | Online |
|---|---|---|
| `int` → `bigint` | `ALTER COLUMN … TYPE bigint` (rewrites the table under an exclusive lock) | Expand: new column + trigger; backfill in 2K-row batches; `VACUUM`; `CHECK … NOT VALID` + `VALIDATE`; contract: drop, rename, `SET NOT NULL` (uses the check). Every DDL step runs with `lock_timeout = '100ms'` and retries |
| An index | `CREATE INDEX` (blocks writes) | `CREATE INDEX CONCURRENTLY` |
| A `NOT NULL` column with a default | A volatile default (`clock_timestamp()`): rewrite | A constant default: metadata only (PostgreSQL 11+) |
| Any `ALTER TABLE`, while a long query runs | No `lock_timeout`: the `ALTER` waits, and every query behind it waits too | `lock_timeout = '200ms'`: the `ALTER` gives up and is retried later |

[`internal/migrate/migrate_test.go`](../internal/migrate/migrate_test.go) checks each pair on a 1M-row table: the blocking way stalls clients for most of its run, the online way far less.

## Notes

- `CREATE INDEX CONCURRENTLY` can't run in a transaction block, and if it fails it leaves an `INVALID` index to drop and retry.
- The trigger keeps the new column in sync with writes during the backfill; the application switches to the new column only after the contract step, so for a real change, deploy code that reads either column first.
- A lock wait is per table: while an `ALTER` waits for its lock, even plain reads of that table queue behind it.
- Autovacuum counts as "someone in the way". A big backfill wakes it, and an `ALTER` that needs a conflicting lock waits for it until `deadlock_timeout` (1 s by default) cancels it, with every client queued behind the `ALTER`. Vacuum explicitly after the backfill: `VACUUM` waits for a running autovacuum without blocking reads or writes.
