# Results: backups and a disaster drill

Measured on 2026-11-10 with `make dr` on a laptop (Intel i7-8550U, NVMe SSD), everything in Docker on one machine. PostgreSQL 17.11; a 1077 MB database (6000000 orders with a secondary index). In production the archive and backups live elsewhere (another machine, object storage); here they're Docker volumes, so copy times are local-disk times.

## Taking and restoring backups

| Backup | Took | Size | Restore |
|---|---:|---:|---|
| Logical: `pg_dump -Fc` | 36.3 s | 364.2M | `pg_restore -j 4` into a fresh server: **26.1 s** (6000000 orders) |
| Physical: `pg_basebackup -X stream` | 4.3 s | 1.1G | Copy the files and start: part of the PITR time below |

## The disaster

After the base backup, a session committed one row every 100 ms for 45 s (row 450 was the last). Then the server was killed and its data volume deleted. 104 WAL segments had been archived (`archive_timeout = 10s`).

| Recovery | Data lost (RPO) | Time to a working primary (RTO, from the moment of the disaster) |
|---|---|---:|
| From the logical dump | everything since the dump: all 450 heartbeat rows, and the table itself | the restore above, plus redeploying |
| Base backup + archived WAL (point-in-time recovery) | **75 rows, the last 7.8 s** (WAL not yet archived) | **5.6 s** (6000000 orders, heartbeat up to row 375) |
