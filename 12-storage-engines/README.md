# 12. B-tree vs LSM tree

**Question:** Why are some databases fast at writes and others at reads?

```bash
make engines
# or: docker compose run --rm go go run ./cmd/engines
# flags: -keys 1000000 -value 100 -batch 1000
```

The same workload on three embedded engines, in one Go process, with no database server:

| Engine | Structure | Used by |
|---|---|---|
| [bbolt](https://github.com/etcd-io/bbolt) 1.5 | B+tree, copy-on-write pages, memory-mapped reads | etcd |
| [Pebble](https://github.com/cockroachdb/pebble) 2.1, defaults | LSM tree: WAL + memtable, flushed to sorted files (SSTables), merged by compaction | CockroachDB |
| Pebble 2.1 with Bloom filters (10 bits/key) on every level | The same, with a filter per file so reads skip files that can't hold the key | Typical production LSM setting |

Workload: insert 1,000,000 keys (16 bytes, random order) with 100-byte random values, in batches of 1,000 with one fsync each; overwrite every key once; then 100,000 point reads and 2,000 scans of 100 keys. "Written to disk" is the kernel's `write_bytes` counter for the process (`/proc/self/io`), so it counts everything each engine writes: pages, logs, flushes, compactions. Results: [results.md](results.md).

[`internal/engines/engines_test.go`](../internal/engines/engines_test.go) checks the trade-off at 20,000 keys: the B+tree's write amplification is well above the LSM's, and its point reads are faster.

## Notes

- Both engines are used with their defaults (plus Bloom filters in the third run), with everything in the OS page cache after writing: reads measure CPU and data-structure cost, not disk seeks.
- bbolt is a simple copy-on-write B+tree. PostgreSQL's and InnoDB's B-trees update pages in place and log changes to a WAL, so their write amplification differs in detail, but random writes still dirty one page per key changed, which is the cost measured here.
- Write throughput between runs of the same engine varies with the laptop's fsync latency; write amplification and read costs are steadier.
