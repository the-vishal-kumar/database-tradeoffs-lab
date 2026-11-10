# 02 — Slow queries at scale: why a query that uses an index still slows down

Companion to the post *"Why does my Postgres query get slower as the table grows, even with an index?"*

```bash
make slowquery          # 10K, 1M and 10M rows; several minutes, ~1.5 GB of disk
# or, without make:
docker compose up -d --wait postgres
docker compose run --rm go go run ./cmd/slowquery -sizes 10000,1000000,10000000
```

It builds the same `orders` table at each size and runs six scenarios with `EXPLAIN (ANALYZE, BUFFERS)`. Measured results: [results.md](results.md).

## The data

`orders(id, tenant_id, customer_id, status, created_at, total_cents, note)`, generated deterministically:

- `created_at` rises with `id`, so it follows the physical order of rows on disk.
- `customer_id` is spread pseudo-randomly over 0–999,999, so it doesn't.
- Tenant 7 is a small, *old* tenant: every 10th row of the first 1% of the table.
- Status is 80% `shipped`, 15% `paid`, 5% `cancelled`.
- Indexes on `created_at`, `customer_id` and `status`. A `customers` table (1M rows, 10% in `NL`) for the join.

Autovacuum is off for these tables so each scenario controls `VACUUM` and `ANALYZE` itself. Parallel query is off so every size is measured the same way.

## The scenarios

| # | What it shows | The fix it measures |
|---|---|---|
| 1 | `WHERE tenant_id = 7 ORDER BY created_at DESC LIMIT 20` with only a `created_at` index: Postgres walks the index from the newest row until it has found 20 rows of tenant 7, and tenant 7's rows are all old | A composite index on `(tenant_id, created_at)` |
| 2 | `OFFSET` pagination 10% deep reads and throws away every skipped row | Keyset pagination: `WHERE created_at < <last seen> ORDER BY created_at DESC LIMIT 20` |
| 3 | The same 0.5% of rows, fetched by a column in disk order (`created_at`) vs a scattered one (`customer_id`): one page per row when scattered | (Explains the cost; fixes are clustering, covering indexes or partitioning) |
| 4 | 5% of rows change to a status the planner has never seen. With stale statistics it estimates 1 row; on its own the query is fine, but joined to `customers` it picks a nested loop | `ANALYZE` (autovacuum normally does it; after bulk changes, run it yourself) |
| 5 | An index-only `count(*)` over 10% of the rows, after updating 10% of the table without `VACUUM`: it has to visit the table to check each changed row | `VACUUM` (sets the visibility map again) |
| 6 | `count(*)` after deleting 90% of the rows: the table file is just as big, so a scan reads just as many pages | `VACUUM` makes the space reusable but doesn't shrink the file; `VACUUM FULL` (or `pg_repack`) does |

[`internal/slowquery/slowquery_test.go`](../internal/slowquery/slowquery_test.go) checks every effect at 200,000 rows in CI, on buffers rather than times, so it doesn't depend on the machine.

## Reading the numbers

- **Buffers** are 8 KB pages touched (from Postgres's cache or the OS). They are the honest measure of work; time depends on the machine and on what's cached.
- At 10M rows the table (~1.4 GB) is larger than the default `shared_buffers` (128 MB), so many reads come from the OS page cache. On a production server with more memory, times would be lower; the page counts would not.
- In scenario 5 at 10M rows, the index-only scan touches far more buffers after the updates even once heap fetches are back to 0. Most likely those are visibility-map lookups: updated rows moved to the end of a 1.4 GB table, so consecutive index entries point into distant parts of it, each covered by a different visibility-map page. At 1M rows the whole table fits under one visibility-map page, and the effect disappears.
