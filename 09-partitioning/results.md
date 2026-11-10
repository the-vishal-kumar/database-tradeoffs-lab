# Results: one table vs monthly partitions

Measured on 2026-11-10 with `make partition` on a laptop (Intel i7-8550U, 8 threads), PostgreSQL in Docker. An earlier run gave the same picture: the queries within 10%, `DELETE` 629 ms and 105 MB of WAL, `DROP` 17 ms.

PostgreSQL 17.11. 6000000 rows over 2026 (500000 a month), 1,000 tenants, 100-byte payload; indexes on (created_at) and (tenant_id, created_at). Sizes include indexes.

### Queries (median of 50 runs)

| Query | Layout | p50 | Partitions in the plan |
|---|---|---:|---:|
| last 2 days (`created_at >= '2026-12-30'`) | one table | 2.85 ms | — |
| last 2 days (`created_at >= '2026-12-30'`) | partitioned by month (12 partitions) | 3.18 ms | 1 |
| one tenant, last 2 days | one table | 0.10 ms | — |
| one tenant, last 2 days | partitioned by month (12 partitions) | 0.08 ms | 1 |
| one tenant, all year (no partition key) | one table | 0.65 ms | — |
| one tenant, all year (no partition key) | partitioned by month (12 partitions) | 1.05 ms | 12 |

### Removing the oldest month

| Layout | How | Took | WAL written | Size before | Size after | Then VACUUM | Size after VACUUM |
|---|---|---:|---:|---:|---:|---:|---:|
| one table | `DELETE … WHERE created_at < '2026-02-01'` | 578 ms | 105 MB | 1218 MB | 1218 MB | 1230 ms | 1218 MB |
| partitioned by month (12 partitions) | `DETACH PARTITION` + `DROP TABLE` | 48 ms | 2 MB | 1219 MB | 1115 MB | 323 ms | 1115 MB |
