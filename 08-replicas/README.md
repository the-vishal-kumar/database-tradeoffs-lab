# 08 — Read replicas: lag, stale reads and synchronous commit

Companion to the posts *"CAP or PACELC: what trade-off do you make when nothing is broken?"* and *"Read replicas, partitioning or sharding: what does each one fix?"*

```bash
make replicas
# or, without make:
docker compose --profile replicas up -d --wait pgprimary pgreplica
docker compose run --rm -e PRIMARY_URL=postgres://lab:lab@pgprimary:5432/lab?sslmode=disable \
  -e REPLICA_URL=postgres://lab:lab@pgreplica:5432/lab?sslmode=disable go go run ./cmd/replicas
```

A PostgreSQL 17 primary and a streaming replica cloned from it with `pg_basebackup` (see [`replica-entrypoint.sh`](replica-entrypoint.sh)). Measured results: [results.md](results.md).

| # | Scenario | Compared |
|---|---|---|
| 1 | Under background write load, commit an increment on the primary and immediately read the row from the replica, 2,000 times | `synchronous_commit` = `local`, `remote_write`, `on` (remote flush), `remote_apply`, with the replica named in `synchronous_standby_names` |
| 2 | One `INSERT` of 2,000,000 rows | How far behind the replica falls (bytes and time), and how long it takes to catch up |
| 3 | The synchronous replica is gone | A commit with `statement_timeout`, and a commit cancelled from another session: how long it waits, what the client is told, and whether the row changed |

[`internal/replicas/replicas_test.go`](../internal/replicas/replicas_test.go) checks: an async replica read right after a commit is usually stale, `remote_apply` never is, `statement_timeout` doesn't end the wait for a missing synchronous replica, and a cancelled wait leaves the transaction committed locally. It needs the replica running (`make replicas-test`), so CI skips it.

## Notes

- `synchronous_commit = on` waits until the replica has *flushed* the WAL, not *applied* it: a read on the replica can still be stale. Only `remote_apply` waits for the change to be visible there.
- The commit-wait is not covered by `statement_timeout` (the timeout is disabled before the commit step), so with a synchronous replica that's gone, commits wait until someone cancels them or the replica returns. A cancelled wait does **not** roll back: the transaction is already committed on the primary.
- One replica on one machine. With several, `synchronous_standby_names` can require `ANY 1 (a, b)` or `FIRST 2 (a, b, c)`, so one replica going away doesn't stop commits.
