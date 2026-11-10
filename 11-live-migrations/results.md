# Results: schema changes under load

Measured on 2026-11-10 with `make livemigrate` on a laptop (Intel i7-8550U, 8 threads), PostgreSQL in Docker. A first run gave the same picture (`ALTER … TYPE` stalled clients 7.9 s; `CREATE INDEX` 2.3 s; the lock-queue case 5.0 s), except that expand/contract still stalled them 3.2 s in its final `SET NOT NULL`, which scans the table under an exclusive lock. Validating a `CHECK … NOT VALID` constraint first removed that; the run below includes the fix.

PostgreSQL 17.11. A 5000000-row table; 8 clients each loop on a primary-key read and a one-row update while the change runs. Latency is per read+update pair.

| Change | How | Change took | Clients: p50 | p99 | slowest | requests over 1 s | errors |
|---|---|---:|---:|---:|---:|---:|---:|
| `owner_id` int → bigint | `ALTER COLUMN … TYPE bigint` (rewrites the table) | 7572 ms | 1.1 ms | 3.0 ms | **7494 ms** | 8 | 0 |
| `owner_id` int → bigint | expand (new column + trigger) / backfill in 10K-row batches / `CHECK … NOT VALID` + `VALIDATE` / contract | 62.3 s | 1.8 ms | 15 ms | **165 ms** | 0 | 0 |
| index on `owner_id` | `CREATE INDEX` | 2073 ms | 1.9 ms | 7.6 ms | **2068 ms** | 8 | 0 |
| index on `owner_id` | `CREATE INDEX CONCURRENTLY` | 4479 ms | 4.6 ms | 25 ms | **378 ms** | 0 | 0 |
| add a NOT NULL column | `ADD COLUMN status text NOT NULL DEFAULT 'active'` | 21 ms | 4.9 ms | 44 ms | **82 ms** | 0 | 0 |
| add a NOT NULL column | `ADD COLUMN created_at timestamptz NOT NULL DEFAULT clock_timestamp()` (volatile default: rewrite) | 7577 ms | 1.3 ms | 6.8 ms | **7511 ms** | 8 | 0 |
| a 5s report is running | `ALTER TABLE … ADD COLUMN flag boolean`, no lock_timeout | 5200 ms | 4.7 ms | 15 ms | **4997 ms** | 8 | 0 |
| a 5s report is running | the same, with `lock_timeout = '200ms'` | 5264 ms | 1.0 ms | 4.8 ms | **203 ms** | 0 | 0 |

- `owner_id` int → bigint, expand (new column + trigger) / backfill in 10K-row batches / `CHECK … NOT VALID` + `VALIDATE` / contract: the contract step only renames and drops; `SET NOT NULL` reuses the validated check instead of scanning under a lock.
- a 5s report is running, `ALTER TABLE … ADD COLUMN flag boolean`, no lock_timeout: every request after the ALTER queued until the report finished.
- a 5s report is running, the same, with `lock_timeout = '200ms'`: the ALTER gave up after 200 ms (retry later); traffic carried on. "Change took" includes waiting for the report to end.
