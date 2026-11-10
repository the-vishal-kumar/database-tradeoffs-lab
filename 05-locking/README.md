# 05 — Lock the row or retry on conflict?

Companion to the post *"Should you lock the row or retry on conflict?"*

```bash
make locking
# or, without make:
docker compose up -d --wait postgres
docker compose run --rm go go run ./cmd/lockdemo
# flags: -workers 32 -per 25 -items 1000 -work 2ms
```

The workload is "buy one": read the stock, do some application work (2 ms of sleep, standing in for a price calculation or a call to another service), then write stock − 1. 32 buyers make 25 purchases each, either all on one item (high contention) or on a random one of 1,000 items (low contention). A third scenario has 50 buyers rush the last 10 tickets. Measured results: [results.md](results.md).

| Strategy | What it does | Holds a lock during the work? |
|---|---|---|
| Naive | `SELECT stock`, work, `UPDATE … SET stock = <read − 1>` | no, and nothing checks for conflicts: the baseline that loses updates |
| Pessimistic | `BEGIN; SELECT … FOR UPDATE`, work, `UPDATE`, `COMMIT` | yes: other buyers of the same row wait |
| Optimistic | `SELECT stock, version`, work, `UPDATE … WHERE version = <read>`; 0 rows updated → back off and retry | no; conflicts are detected at write time |
| Atomic | work, then `UPDATE … SET stock = stock − 1 WHERE stock > 0` | only for the single statement |
| Serializable | read, work, write in a `SERIALIZABLE` transaction; retry on SQLSTATE 40001 | no row lock; PostgreSQL aborts one of two conflicting transactions |

Retrying strategies back off with jitter (0.1 ms doubling to 6.4 ms) and give up after 100 attempts; give-ups are counted, not hidden.

[`internal/locking/locking_test.go`](../internal/locking/locking_test.go) checks the outcomes: the naive version loses updates and oversells, every other strategy loses nothing and sells exactly the stock, the atomic `UPDATE` beats `FOR UPDATE` on a hot row by at least 2×, and optimistic retries outnumber purchases on a hot row.

## Notes

- Correctness numbers (lost updates, tickets sold) are the same on every run. Throughput under high contention varies a lot between runs, optimistic most of all; results.md shows three runs.
- Everything runs on one machine, so a database round trip is well under a millisecond. Across a network, the extra round trips of retries cost more, and the time a `FOR UPDATE` lock is held grows by a round trip too.
- The Serializable retries at low contention are mostly false positives: PostgreSQL tracks what a transaction read at index-page granularity (`SIReadLock` on a page of `items_pkey`), so transactions on different items that share an index page can still conflict.
