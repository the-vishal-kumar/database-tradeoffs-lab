# Results: slow queries at scale

Measured on 2026-11-10 with `make slowquery` on a laptop (Intel(R) Core(TM) i7-8550U CPU @ 1.80GHz, 8 threads, 10 GB RAM), the official `postgres:17-alpine` image in its default configuration (shared_buffers 128 MB, so the 10M-row table does not fit in Postgres's own cache), parallel query off, measured with `EXPLAIN (ANALYZE, BUFFERS, TIMING OFF)` so per-node clock calls don't inflate row-heavy plans.

Each cell: median execution time · shared buffers touched (8 KB pages, hit + read) · plan. **Buffers are the number to compare**: they don't depend on the machine; times do.

### 1. Latest 20 orders of a small, old tenant

| Variant | 10,000 rows | 1,000,000 rows | 10,000,000 rows |
|---|---|---|---|
| index on (created_at) only | 0.45 ms · 164 buf<br>Limit > Sort > Seq Scan | 134 ms · 21,649 buf<br>Limit > Index Scan | 1,513 ms · 216,402 buf<br>Limit > Index Scan |
| index on (tenant_id, created_at) | 0.07 ms · 4 buf<br>Limit > Sort > Bitmap Heap Scan > Bitmap Index Scan | 0.04 ms · 7 buf<br>Limit > Index Scan | 0.06 ms · 7 buf<br>Limit > Index Scan |

### 2. A page 10% deep: OFFSET vs keyset

| Variant | 10,000 rows | 1,000,000 rows | 10,000,000 rows |
|---|---|---|---|
| OFFSET (10% deep) LIMIT 20 | 0.15 ms · 25 buf<br>Limit > Index Scan | 13.4 ms · 2,192 buf<br>Limit > Index Scan | 189 ms · 21,862 buf<br>Limit > Index Scan |
| keyset: WHERE created_at < cursor LIMIT 20 | 0.03 ms · 3 buf<br>Limit > Index Scan | 0.04 ms · 7 buf<br>Limit > Index Scan | 0.04 ms · 4 buf<br>Limit > Index Scan |

### 3. 0.5% of rows: by a column in disk order vs a scattered one

| Variant | 10,000 rows | 1,000,000 rows | 10,000,000 rows |
|---|---|---|---|
| 0.5% of rows by created_at range | 0.03 ms · 5 buf<br>Aggregate > Index Scan | 0.72 ms · 100 buf<br>Aggregate > Index Scan | 7.9 ms · 961 buf<br>Aggregate > Index Scan |
| 0.5% of rows by customer_id range | 0.12 ms · 52 buf<br>Aggregate > Bitmap Heap Scan > Bitmap Index Scan | 4.1 ms · 5,017 buf<br>Aggregate > Bitmap Heap Scan > Bitmap Index Scan | 209 ms · 50,061 buf<br>Aggregate > Bitmap Heap Scan > Bitmap Index Scan |

### 4. Stale statistics after 5% of rows change to a new value

| Variant | 10,000 rows | 1,000,000 rows | 10,000,000 rows |
|---|---|---|---|
| one table, before ANALYZE | 0.11 ms · 12 buf · est 1 / actual 500 rows<br>Aggregate > Index Scan | 7.3 ms · 905 buf · est 1 / actual 50,000 rows<br>Aggregate > Index Scan | 79.3 ms · 9,018 buf · est 1 / actual 500,000 rows<br>Aggregate > Index Scan |
| joined to customers, before ANALYZE | 1.1 ms · 2,012 buf · est 1 / actual 500 rows<br>Aggregate > Nested Loop > Index Scan | 103 ms · 200,905 buf · est 1 / actual 50,000 rows<br>Aggregate > Nested Loop > Index Scan | 1,098 ms · 2,009,018 buf · est 1 / actual 500,000 rows<br>Aggregate > Nested Loop > Index Scan |
| one table, after ANALYZE | 0.11 ms · 12 buf · est 500 / actual 500 rows<br>Aggregate > Bitmap Heap Scan > Bitmap Index Scan | 6.8 ms · 905 buf · est 50,400 / actual 50,000 rows<br>Aggregate > Bitmap Heap Scan > Bitmap Index Scan | 83.2 ms · 9,018 buf · est 494,988 / actual 500,000 rows<br>Aggregate > Bitmap Heap Scan > Bitmap Index Scan |
| joined to customers, after ANALYZE | 0.83 ms · 2,012 buf · est 500 / actual 500 rows<br>Aggregate > Nested Loop > Bitmap Heap Scan > Bitmap Index Scan > Index Scan | 96.1 ms · 5,330 buf · est 50,400 / actual 50,000 rows<br>Aggregate > Hash Join > Bitmap Heap Scan > Bitmap Index Scan > Hash > Seq Scan | 193 ms · 13,443 buf · est 494,988 / actual 500,000 rows<br>Aggregate > Hash Join > Bitmap Heap Scan > Bitmap Index Scan > Hash > Seq Scan |

### 5. Index-only scan and the visibility map

| Variant | 10,000 rows | 1,000,000 rows | 10,000,000 rows |
|---|---|---|---|
| after VACUUM | 0.20 ms · 5 buf · heap fetches 0<br>Aggregate > Index Only Scan | 8.7 ms · 277 buf · heap fetches 0<br>Aggregate > Index Only Scan | 114 ms · 102,737 buf · heap fetches 0<br>Aggregate > Index Only Scan |
| after updating 10% of rows, no VACUUM | 0.23 ms · 218 buf · heap fetches 950<br>Aggregate > Index Only Scan | 23.1 ms · 21,588 buf · heap fetches 95,000<br>Aggregate > Index Only Scan | 473 ms · 515,851 buf · heap fetches 950,000<br>Aggregate > Index Only Scan |
| after VACUUM again | 0.13 ms · 5 buf · heap fetches 0<br>Aggregate > Index Only Scan | 9.7 ms · 277 buf · heap fetches 0<br>Aggregate > Index Only Scan | 129 ms · 302,736 buf · heap fetches 0<br>Aggregate > Index Only Scan |

### 6. Counting rows after deleting 90%

| Variant | 10,000 rows | 1,000,000 rows | 10,000,000 rows |
|---|---|---|---|
| full table | 0.97 ms · 181 buf · table 1448 kB<br>Aggregate > Seq Scan | 123 ms · 18,076 buf · table 141 MB<br>Aggregate > Seq Scan | 1,560 ms · 180,752 buf · table 1412 MB<br>Aggregate > Seq Scan |
| after deleting 90%, no VACUUM | 0.27 ms · 181 buf · table 1448 kB<br>Aggregate > Seq Scan | 34.2 ms · 18,076 buf · table 141 MB<br>Aggregate > Seq Scan | 560 ms · 180,752 buf · table 1412 MB<br>Aggregate > Seq Scan |
| after VACUUM | 0.14 ms · 181 buf · table 1448 kB<br>Aggregate > Seq Scan | 21.1 ms · 18,076 buf · table 141 MB<br>Aggregate > Seq Scan | 336 ms · 180,752 buf · table 1412 MB<br>Aggregate > Seq Scan |
| after VACUUM FULL (rewrites the table) | 0.12 ms · 17 buf · table 136 kB<br>Aggregate > Seq Scan | 13.3 ms · 1,644 buf · table 13 MB<br>Aggregate > Seq Scan | 171 ms · 16,436 buf · table 128 MB<br>Aggregate > Seq Scan |

