# 07. Why does one long-running transaction slow down the whole database?

**Question:** Why does one long-running transaction slow down the whole database?

```bash
make longtx
# or, without make:
docker compose up -d --wait postgres mysql
docker compose run --rm go go run ./cmd/longtx
# flag: -duration 20s (per variant)
```

8 writers update 100 hot rows as fast as they can for 20 s. Meanwhile, one session opens a transaction, reads one row of an **unrelated** table, and sits there, like a report, a debugging session in a SQL client, or a request that forgot to commit. Afterwards: how many old row versions are still kept, how big the table is, and how long reads take. Measured results: [results.md](results.md).

| Database | Where old versions live | Variants |
|---|---|---|
| PostgreSQL 17 | In the table itself, until VACUUM removes them (VACUUM runs every second here) | No long transaction; a transaction left open; the same with `idle_in_transaction_session_timeout = 2s` |
| MySQL 8.4 (InnoDB) | In the undo log, until purge removes them; the table holds the newest version | No long transaction; a `START TRANSACTION WITH CONSISTENT SNAPSHOT` left open |

[`internal/longtx/longtx_test.go`](../internal/longtx/longtx_test.go) checks each effect: dead rows VACUUM can't remove, the table bloating and staying bloated after VACUUM, the timeout releasing the cleanup, InnoDB's history list growing, and the old transaction paying to rebuild old versions.

## Notes

- Dead tuples are counted exactly with the `pgstattuple` extension (`dead_tuple_count`), not with the estimates in `pg_stat_user_tables`.
- PostgreSQL's horizon is per database: an open transaction holds back cleanup in every table of its database, not only the tables it read. Replication slots and `hot_standby_feedback` from replicas can hold it back the same way.
- `idle_in_transaction_session_timeout` ends a session that sits idle *inside* a transaction; it doesn't stop a long-*running* query. For those, `statement_timeout`, and since PostgreSQL 17 `transaction_timeout`.
- The InnoDB history list length comes from `SHOW ENGINE INNODB STATUS`.
