# Results: cache consistency and stampedes

Measured on 2026-11-10 with `make cache` on a laptop (Intel i7-8550U, 8 threads, 10 GB RAM), everything in Docker on one machine. Times depend on the machine; the counts and outcomes are what to compare. Each scenario is deterministic except the timings in 4 and 5.

PostgreSQL 17.11, Redis 7.4.12, go-redis 9.23.0, a pool of 50 database connections.

### 1. After a write: TTL only vs delete the key (TTL 2 s, a read every 50 ms)

| Strategy | Stale reads | Stale for |
|---|---:|---:|
| TTL only (no invalidation) | 40 | 2s |
| delete the key after the write | 0 | 0s |

### 2. The race: reader loads the old row, writer updates and invalidates, reader fills the cache (TTL 60 s)

| Strategy | Database | Cache afterwards | Stale for |
|---|---:|---:|---|
| delete after the write | 200 | 100 | until the TTL (1m0s) expires |
| delete, then delete again after 500ms | 200 | empty | 500ms |
| versioned: writer sets the new version, cache keeps the newest | 200 | 200 | never stale |

### 3. A write that rolls back

| Strategy | Readers got | Database has |
|---|---:|---:|
| set the cache inside the transaction | 300 | 100 |
| touch the cache only after commit | 100 | 100 |

### 4. Stampede: 200 readers (4 instances × 50) hit a key that just expired; loading it takes 50 ms

| Strategy | Database queries | p50 | p99 | Slowest | Served the old copy |
|---|---:|---:|---:|---:|---:|
| naive cache-aside | 130 | 123.9 ms | 221.7 ms | 224.4 ms | 0 |
| singleflight in each of 4 app instances | 4 | 51.4 ms | 51.8 ms | 51.8 ms | 0 |
| Redis lock (SET NX): one loader, the rest wait for the cache | 1 | 64.5 ms | 68.2 ms | 68.2 ms | 0 |
| stale-while-revalidate: serve the old copy, one refresher | 1 | 4.5 ms | 5.7 ms | 5.7 ms | 200 |

### 5. 2,000 keys cached at once with a 1 s TTL, read continuously for 3 s

| Strategy | Peak database queries in any 100 ms | Total queries |
|---|---:|---:|
| same TTL for every key | 1661 | 4000 |
| TTL ±30% random jitter | 343 | 4759 |
