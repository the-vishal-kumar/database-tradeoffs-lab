# Results: schema changes under load

Measured on 2026-11-13 with `make livemigrate` on a laptop (Intel i7-8550U, 8 threads), PostgreSQL in Docker; a second run matched (expand/contract slowest 315 ms, none over 1 s). Two earlier versions of the online path stalled clients: first 3.2 s in a final `SET NOT NULL` that scanned the table under an exclusive lock (fixed by validating a `CHECK … NOT VALID` first), then about 1 s when `ADD CONSTRAINT` had to wait for the autovacuum the backfill had started, with every client queued behind it until `deadlock_timeout` cancelled the vacuum (fixed by an explicit `VACUUM` after the backfill and a 100 ms `lock_timeout` with retries on every DDL step).

PostgreSQL 17.11. A 5000000-row table; 8 clients each loop on a primary-key read and a one-row update while the change runs. Latency is per read+update pair.

| Change | How | Change took | Clients: p50 | p99 | slowest | requests over 1 s | errors |
|---|---|---:|---:|---:|---:|---:|---:|
| `owner_id` int → bigint | `ALTER COLUMN … TYPE bigint` (rewrites the table) | 8648 ms | 4.6 ms | 8.2 ms | **8572 ms** | 8 | 0 |
| `owner_id` int → bigint | expand (new column + trigger) / backfill in 2K-row batches / `VACUUM` / `CHECK … NOT VALID` + `VALIDATE` / contract | 70.6 s | 5.0 ms | 24 ms | **314 ms** | 0 | 0 |
| index on `owner_id` | `CREATE INDEX` | 2664 ms | 1.3 ms | 5.2 ms | **2658 ms** | 8 | 0 |
| index on `owner_id` | `CREATE INDEX CONCURRENTLY` | 5402 ms | 4.4 ms | 15 ms | **153 ms** | 0 | 0 |
| add a NOT NULL column | `ADD COLUMN status text NOT NULL DEFAULT 'active'` | 15 ms | 4.7 ms | 7.0 ms | **20 ms** | 0 | 0 |
| add a NOT NULL column | `ADD COLUMN created_at timestamptz NOT NULL DEFAULT clock_timestamp()` (volatile default: rewrite) | 7990 ms | 4.4 ms | 6.4 ms | **7895 ms** | 8 | 0 |
| a 5s report is running | `ALTER TABLE … ADD COLUMN flag boolean`, no lock_timeout | 5295 ms | 1.5 ms | 5.4 ms | **5086 ms** | 8 | 0 |
| a 5s report is running | the same, with `lock_timeout = '200ms'` | 5493 ms | 5.5 ms | 16 ms | **210 ms** | 0 | 0 |

- `owner_id` int → bigint, expand (new column + trigger) / backfill in 2K-row batches / `VACUUM` / `CHECK … NOT VALID` + `VALIDATE` / contract: the contract step only renames and drops; `SET NOT NULL` reuses the validated check instead of scanning under a lock.
- a 5s report is running, `ALTER TABLE … ADD COLUMN flag boolean`, no lock_timeout: every request after the ALTER queued until the report finished.
- a 5s report is running, the same, with `lock_timeout = '200ms'`: the ALTER gave up after 200 ms (retry later); traffic carried on. "Change took" includes waiting for the report to end.
