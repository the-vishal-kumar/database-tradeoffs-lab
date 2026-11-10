# Results: a streaming replica

Measured on 2026-11-10 with `make replicas` on a laptop (Intel i7-8550U, 8 threads), primary and replica in Docker on the same machine, so the network adds almost nothing; across real networks every synchronous level costs at least one round trip more.

Three runs of section 1 (stale reads out of 2,000): `local` 1,993 / 2,000 / 1,994; `remote_write` 1,966 / 1,999 / 1,847; `on` 14 / 9 / 227; `remote_apply` 0 / 0 / 0. Commit p50 rose from about 1.2–1.6 ms (`local`) to 2.4–2.7 ms (`remote_apply`). The run below is the third.

PostgreSQL 17.11, one primary and one streaming replica, both in Docker on one machine (a network round trip well under a millisecond).

### 1. Write on the primary, then read it back from the replica

4 background sessions insert 1 kB rows continuously; 2,000 increments, each read back from the replica right after its commit.

| synchronous_commit | Commit p50 | Commit p99 | Stale reads from the replica | Replica lag p50 / max (while measuring) |
|---|---:|---:|---:|---|
| `local` | 1.65 ms | 2.62 ms | 1994 of 2000 (99.7%) | 2.12 ms / 4.31 ms |
| `remote_write` | 1.81 ms | 6.25 ms | 1847 of 2000 (92.3%) | 2.22 ms / 20 ms |
| `on` | 2.38 ms | 4.52 ms | 227 of 2000 (11.3%) | 2.19 ms / 16 ms |
| `remote_apply` | 2.56 ms | 6.33 ms | 0 of 2000 (0.0%) | 2.20 ms / 21 ms |

### 2. One big write

`INSERT` of 2000000 rows (685 MB of WAL) in one statement: took 8949 ms on the primary. The replica fell up to 49 MB / 505 ms behind, and caught up 1116 ms after the commit.

### 3. The synchronous replica is gone

`synchronous_standby_names` names a replica that isn't connected; commits use the default `synchronous_commit = on`.

- With `statement_timeout = 3s`, an `UPDATE` waited **9008 ms**: statement_timeout did **not** end it; the client gave up.
- With another session cancelling it after 3 s (as Ctrl-C would), it waited **3003 ms**. Error returned to the client: none: the client was told the commit succeeded. PostgreSQL's warning: "canceling wait for synchronous replication due to user request" — "The transaction has already committed locally, but might not have been replicated to the standby." The row was **changed** on the primary afterwards.
- The same update with `synchronous_commit = local` took 3.15 ms.
