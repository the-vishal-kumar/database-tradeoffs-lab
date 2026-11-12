# 16. Primary keys: bigint vs UUIDv4 vs UUIDv7

**Question:** does the kind of primary key change insert speed and index size?

`run.sh` starts a PostgreSQL 17 with a deliberately small buffer cache (128 MB), generates 5,000,000 keys of each kind (in the order an application would generate them), and inserts them in batches of 50,000 into three tables. It reports insert time, WAL written, primary-key index size and the index's average leaf density (from `pgstattuple`).

- **bigint:** an increasing integer, 8 bytes.
- **UUIDv4:** random, 16 bytes; each insert lands on a random index page.
- **UUIDv7:** 16 bytes, time-ordered: 48 bits of Unix milliseconds, a 12-bit sub-millisecond fraction (as in PostgreSQL 18's built-in `uuidv7()`), then random bits. PostgreSQL 17 has no built-in, so the script defines one in SQL.

```bash
make ids            # or ./16-id-generation/run.sh; ROWS=... BATCH=... to change the size
```

Results: [results.md](results.md). One run on a laptop; timings vary by a few seconds between runs, sizes and densities repeat.
