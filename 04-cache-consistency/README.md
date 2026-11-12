# 04. Cache consistency and stampedes: Redis in front of PostgreSQL

**Question:** Why did adding a cache make our data wrong?

```bash
make cache
# or, without make:
docker compose up -d --wait postgres redis
docker compose run --rm go go run ./cmd/cachedemo
```

One product row in PostgreSQL (price, version), cached in Redis. Each scenario counts the queries that reach the database and checks what readers were given. Races are replayed step by step in a fixed order, so they happen every run instead of once in a million requests. Measured results: [results.md](results.md).

| # | What it shows | Strategies compared |
|---|---|---|
| 1 | How long readers see the old price after a write | TTL only vs delete the key after the write |
| 2 | The delete-on-write race: a reader loads the old row, the writer updates and deletes the key, then the reader fills the cache with the old row | Delete after write vs delete twice (500 ms apart) vs versioned writes (a Lua script keeps only a newer version) |
| 3 | A write that updates the cache and then rolls back | Cache inside the transaction vs only after commit |
| 4 | 200 readers (4 app instances × 50) requesting a key that just expired, when loading it takes 50 ms | Naive cache-aside vs singleflight per instance vs a Redis lock vs stale-while-revalidate |
| 5 | 2,000 keys cached at the same moment with the same TTL | Same TTL vs ±30% random jitter |

[`internal/cache/cache_test.go`](../internal/cache/cache_test.go) checks every effect (counts and outcomes, with a margin on the timing-dependent jitter result), and CI runs it against PostgreSQL 17 and Redis 7.4.

## Notes

- Everything runs on one machine: Redis and Postgres round trips are well under a millisecond. Across a network, every extra round trip in these strategies (the lock, the Lua script) costs more.
- The "app instances" in scenario 4 are goroutine groups with their own singleflight group and a shared connection pool, standing in for separate processes.
- The Redis lock here is a single `SET NX` with a TTL: fine for preventing a stampede (the worst case is a few duplicate loads). It is not a correctness lock; see the Redis documentation on distributed locks before using one to protect writes.
