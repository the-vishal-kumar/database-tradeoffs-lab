#!/usr/bin/env bash
# Disaster-recovery drill on a ~1 GB PostgreSQL database:
#   1. a logical backup (pg_dump) and how long it takes to restore;
#   2. a physical base backup plus continuous WAL archiving;
#   3. writes keep running, then the server AND its disk are destroyed;
#      the database is rebuilt from the base backup + archived WAL, and
#      the drill measures how much was lost (RPO) and how long it took (RTO).
# Writes 15-backup-restore/results.md. Needs only Docker and bash (awk does
# the arithmetic). About 5 minutes.
set -euo pipefail
cd "$(dirname "$0")/.."
OUT=15-backup-restore/results.md
DC="docker compose --profile dr"
P=database-tradeoffs-lab
ROWS=${ROWS:-6000000}

now() { date +%s.%N; }
since() { awk -v a="$(now)" -v b="$1" 'BEGIN { printf "%.1f", a - b }'; }
psqlp() { $DC exec -T pgdr psql -U lab -d lab -Atq "$@"; }
# Over TCP: during first start the image runs a temporary socket-only server.
ready() { for _ in $(seq 240); do docker exec "$1" pg_isready -h 127.0.0.1 -U "${2:-postgres}" >/dev/null 2>&1 && return 0; sleep 0.5; done; return 1; }
cleanup() { docker rm -f dr-restore dr-pitr >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "== fresh primary with WAL archiving"
cleanup
$DC rm -sfv pgdr >/dev/null 2>&1 || true
docker volume rm ${P}_pgdr-data ${P}_pgdr-archive ${P}_pgdr-backup >/dev/null 2>&1 || true
$DC up -d --wait pgdr >/dev/null 2>&1
$DC exec -T -u root pgdr sh -c 'chown postgres /archive /backup'

echo "== loading $ROWS orders"
psqlp -c "CREATE TABLE orders (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, customer_id int NOT NULL, total int NOT NULL,
            note text NOT NULL, created_at timestamptz NOT NULL DEFAULT now());
          INSERT INTO orders (customer_id, total, note) SELECT g % 100000, g % 1000, md5(g::text) || md5((g * 7)::text) || md5((g * 13)::text) FROM generate_series(1, $ROWS) g;
          CREATE INDEX ON orders (customer_id);"
psqlp -c "CHECKPOINT"
DBSIZE=$(psqlp -c "SELECT pg_size_pretty(pg_database_size('lab'))")
VERSION=$(psqlp -c "SHOW server_version")

echo "== 1. logical backup"
t=$(now); $DC exec -T pgdr sh -c 'pg_dump -U lab -d lab -Fc -f /backup/lab.dump'; DUMP_S=$(since "$t")
DUMP_SIZE=$($DC exec -T pgdr sh -c 'du -h /backup/lab.dump | cut -f1')
docker run -d --name dr-restore -e POSTGRES_PASSWORD=x -v ${P}_pgdr-backup:/backup postgres:17.11-alpine >/dev/null
ready dr-restore
t=$(now); docker exec dr-restore pg_restore -U postgres -d postgres -j 4 --no-owner /backup/lab.dump; RESTORE_S=$(since "$t")
RESTORED_ROWS=$(docker exec dr-restore psql -U postgres -Atc "SELECT count(*) FROM orders")
docker rm -f dr-restore >/dev/null

echo "== 2. physical base backup"
t=$(now); $DC exec -T pgdr sh -c 'pg_basebackup -U lab -D /backup/base -X stream -c fast'; BASE_S=$(since "$t")
BASE_SIZE=$($DC exec -T pgdr sh -c 'du -sh /backup/base | cut -f1')

echo "== 3. writes, then disaster"
psqlp -c "CREATE TABLE heartbeat (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, at timestamptz NOT NULL DEFAULT clock_timestamp())"
# A committed row every 100 ms (note: text is hashes, so it compresses like real data, not like filler) for 45 s, each in its own transaction.
psqlp -c "DO \$\$ BEGIN FOR i IN 1..450 LOOP INSERT INTO heartbeat DEFAULT VALUES; COMMIT; PERFORM pg_sleep(0.1); END LOOP; END \$\$;"
LAST=$(psqlp -c "SELECT id || ' ' || extract(epoch FROM at) FROM heartbeat ORDER BY id DESC LIMIT 1")
LAST_ID=${LAST% *}; LAST_AT=${LAST#* }
ARCHIVED=$($DC exec -T pgdr sh -c 'ls /archive | wc -l')
# The server and its disk are gone. Only the backup and the WAL archive
# (in production: another machine, or object storage) survive.
$DC kill pgdr >/dev/null 2>&1; $DC rm -f pgdr >/dev/null 2>&1
docker volume rm ${P}_pgdr-data >/dev/null
DISASTER=$(now)

echo "== 4. point-in-time recovery from base backup + archived WAL"
docker run -d --name dr-pitr -v ${P}_pgdr-backup:/backup -v ${P}_pgdr-archive:/archive --entrypoint sh postgres:17.11-alpine -c '
  cp -a /backup/base /tmp/data && chown -R postgres:postgres /tmp/data && chmod 700 /tmp/data && touch /tmp/data/recovery.signal &&
  exec gosu postgres postgres -D /tmp/data -c "restore_command=cp /archive/%f %p" -c archive_mode=off' >/dev/null
# Recovery replays every archived segment, then promotes to a normal primary.
for _ in $(seq 600); do
  st=$(docker exec dr-pitr psql -U lab -d lab -Atc "SELECT pg_is_in_recovery()" 2>/dev/null || true)
  [ "$st" = "f" ] && break; sleep 0.5
done
PITR_S=$(since "$DISASTER")
REC=$(docker exec dr-pitr psql -U lab -d lab -Atc "SELECT id || ' ' || extract(epoch FROM at) FROM heartbeat ORDER BY id DESC LIMIT 1")
REC_ID=${REC% *}; REC_AT=${REC#* }
REC_ORDERS=$(docker exec dr-pitr psql -U lab -d lab -Atc "SELECT count(*) FROM orders")
LOST_ROWS=$((LAST_ID - REC_ID))
LOST_S=$(awk -v a="$LAST_AT" -v b="$REC_AT" 'BEGIN { printf "%.1f", a - b }')
docker rm -f dr-pitr >/dev/null

{
  echo "# Results: backups and a disaster drill"
  echo
  echo "Measured on $(date -u +%F) with \`make dr\` on a laptop (Intel i7-8550U, NVMe SSD), everything in Docker on one machine. PostgreSQL $VERSION; a $DBSIZE database ($ROWS orders with a secondary index). In production the archive and backups live elsewhere (another machine, object storage); here they're Docker volumes, so copy times are local-disk times."
  echo
  echo "## Taking and restoring backups"
  echo
  echo "| Backup | Took | Size | Restore |"
  echo "|---|---:|---:|---|"
  echo "| Logical: \`pg_dump -Fc\` | ${DUMP_S} s | $DUMP_SIZE | \`pg_restore -j 4\` into a fresh server: **${RESTORE_S} s** ($RESTORED_ROWS orders) |"
  echo "| Physical: \`pg_basebackup -X stream\` | ${BASE_S} s | $BASE_SIZE | Copy the files and start: part of the PITR time below |"
  echo
  echo "## The disaster"
  echo
  echo "After the base backup, a session committed one row every 100 ms for 45 s (row $LAST_ID was the last). Then the server was killed and its data volume deleted. $ARCHIVED WAL segments had been archived (\`archive_timeout = 10s\`)."
  echo
  echo "| Recovery | Data lost (RPO) | Time to a working primary (RTO, from the moment of the disaster) |"
  echo "|---|---|---:|"
  echo "| From the logical dump | everything since the dump: all $LAST_ID heartbeat rows, and the table itself | the restore above, plus redeploying |"
  echo "| Base backup + archived WAL (point-in-time recovery) | **$LOST_ROWS rows, the last ${LOST_S} s** (WAL not yet archived) | **${PITR_S} s** ($REC_ORDERS orders, heartbeat up to row $REC_ID) |"
} > "$OUT"
cat "$OUT"
$DC rm -sfv pgdr >/dev/null 2>&1 || true
