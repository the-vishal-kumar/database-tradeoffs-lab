# Results: one long transaction

Measured on 2026-11-10 with `make longtx` on a laptop (Intel i7-8550U, 8 threads, 10 GB RAM), everything in Docker on one machine. Commits do not wait for the disk (`synchronous_commit = off`, `innodb_flush_log_at_trx_commit = 2`) so the writers are not limited by the laptop's fsync; this does not change how old row versions are kept.

Three runs agreed within about 15%. With the long transaction open, PostgreSQL ran 233K / 257K / 244K updates (vs 682K / 637K / 590K without), the table grew to 9.9 to 10.9 MB (vs 32 to 48 kB), and reading all 100 rows took 3.7 to 4.1 ms (vs 0.12 ms). In MySQL, the old transaction read all 100 rows in 34 to 43 ms (vs 0.14 ms in a new session), with a history list of 58K to 71K. The run below is the third.

PostgreSQL 17.11, MySQL 8.4.13 (InnoDB). 8 writers update 100 hot rows for 20s per variant; reads are 2000 point lookups by primary key.

### PostgreSQL: VACUUM every second while the rows are updated

| Variant | Updates | Dead row versions VACUUM couldn't remove | Table size | Point read p50 | Point read p99 | Read all 100 rows, p50 | After the transaction ends + VACUUM: dead / size / point read / read all |
|---|---:|---:|---:|---:|---:|---:|---|
| no long transaction | 590246 | 0 | 48 kB | 0.07 ms | 0.14 ms | 0.12 ms | 0 / 48 kB / 0.09 ms / 0.07 ms |
| a transaction left open, reading only another table | 243595 | 243603 | 10576 kB | 0.38 ms | 0.60 ms | 3.91 ms | 0 / 10576 kB / 0.09 ms / 0.34 ms |
| the same, with idle_in_transaction_session_timeout = 2s | 550837 | 0 | 1664 kB | 0.10 ms | 0.16 ms | 0.15 ms | 0 / 1664 kB / 0.10 ms / 0.14 ms |

### MySQL InnoDB: purge runs in the background

| Variant | Updates | History list length | Point read p50: new session / old transaction | Read all 100 rows p50: new session / old transaction |
|---|---:|---:|---|---|
| no long transaction | 88229 | 356 | 0.11 ms / - | 0.16 ms / - |
| a consistent-snapshot transaction left open, reading only another table | 70841 | 70848 | 0.11 ms / 0.60 ms | 0.14 ms / 43.46 ms |
