# 09. One big table or partitions?

**Question:** Read replicas, partitioning or sharding: what does each one fix?

```bash
make partition
# or: docker compose up -d --wait postgres && docker compose run --rm go go run ./cmd/partition
# flag: -per-month 500000
```

The same 6,000,000 rows (a year of events, 1,000 tenants) in one table and in a table partitioned by month (`PARTITION BY RANGE (created_at)`, 12 partitions), with the same indexes. Measured: queries with and without the partition key, and removing the oldest month. Results: [results.md](results.md).

[`internal/partition/partition_test.go`](../internal/partition/partition_test.go) checks: a recent-data query touches one partition and a query without the key touches all twelve; `DELETE` + `VACUUM` doesn't shrink the table; dropping a partition gives back its space and writes a fraction of the WAL.

## Notes

- Sizes include indexes. "WAL written" is what every replica must also receive and replay, and what backups and archives store.
- `DETACH PARTITION … CONCURRENTLY` (PostgreSQL 14+) avoids blocking queries on the parent while detaching; it can't run inside a transaction block.
- Twelve partitions is a small number. Planning time and per-query overhead grow with the number of partitions a query can't prune, so thousands of tiny partitions are a mistake.
