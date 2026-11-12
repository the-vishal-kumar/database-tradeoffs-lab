# 15. Backups and a disaster drill

**Question:** What's the cheapest disaster recovery that actually works?

```bash
make dr          # or: ./15-backup-restore/run.sh   (about 5 minutes; only Docker and python3)
```

A ~1 GB PostgreSQL database (6M orders, realistic incompressible text) on a primary that archives every WAL segment (`archive_mode = on`, `archive_timeout = 10s`). The script:

1. takes a logical backup (`pg_dump -Fc`) and restores it into a fresh server (`pg_restore -j 4`);
2. takes a physical base backup (`pg_basebackup`);
3. commits a row every 100 ms for 45 s, then **kills the server and deletes its data volume**;
4. rebuilds it from the base backup plus the archived WAL (point-in-time recovery), and measures what was lost (RPO) and how long it took (RTO).

Results: [results.md](results.md). In production the base backups and WAL archive go to another machine or object storage, typically with a tool such as pgBackRest or WAL-G that adds compression, encryption, retention and parallel restore; this script uses plain `cp` so every step is visible.
