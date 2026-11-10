# 06 — Why isn't a Redis lock enough?

Companion to the post *"Why isn't a Redis lock enough?"*

```bash
make dlock
# or, without make:
docker compose up -d --wait postgres redis
docker compose run --rm go go run ./cmd/dlockdemo
# flags: -runs 300 -pause 0.1
```

The failure being reproduced: a lock holder stops for longer than its lease (a GC pause, a VM being migrated, a slow disk, a network partition). The lease expires, someone else takes the lock, and the first holder wakes up still believing it holds it. TTLs expire in real time; the order of events is scripted, so scenarios 1 and 2 give the same result every run. Measured results: [results.md](results.md).

| # | Scenario | Compared |
|---|---|---|
| 1 | A takes the lock, pauses past its lease; B takes the lock and writes; A wakes up and writes | Redis `SET NX PX` lock vs the same plus a fencing token checked by the database vs a PostgreSQL advisory lock in the writing transaction vs the same with A's connection killed during the pause |
| 2 | A's lease expires, B takes the lock, then A releases "its" lock | `DEL` vs compare-and-delete in Lua |
| 3 | A job scheduled on 3 instances (started up to 50 ms apart), 300 scheduled runs; 10% of executions pause past the 30 ms lease | Redis lock + "already done?" check vs advisory lock + check in one transaction vs claiming the run with `INSERT … ON CONFLICT DO NOTHING` |

[`internal/dlock/dlock_test.go`](../internal/dlock/dlock_test.go) checks every outcome; in the test, every execution pauses, so the Redis lease fails often enough to assert on.

## Notes

- **Fencing tokens** (scenario 1, row 2) need the *resource* to check the token. A database row can (`WHERE fence < $token`); an email, a payment API or a file upload usually can't, unless it accepts an idempotency key.
- **Advisory locks** are held by a PostgreSQL session or transaction, not by a timer. A paused holder keeps the lock, which is the point (scenario 1, row 3), and also the cost: a holder that hangs without its connection dying blocks everyone. Set `idle_in_transaction_session_timeout` so a stuck holder is eventually cut off; when it is, its transaction rolls back with it (row 4).
- **Claiming a row** makes "who runs this" and "it has been claimed" one atomic write. If the claimer crashes after claiming, the run is never done: real implementations store a status and a lease (`claimed_at`) and let another instance reclaim stale claims.
- This is one Redis instance. Redlock (several independent Redis instances) changes how a lock is acquired, but not the paused-holder problem: any lease-based lock needs fencing for correctness.
