#!/usr/bin/env bash
# Primary keys: bigint identity vs random UUID (v4) vs time-ordered UUID (v7).
# Keys are generated first (as an application would generate them) and then
# inserted in that order, ROWS rows in batches, on a
# PostgreSQL with a small buffer cache (so the index outgrows memory, as in
# production), and measures time, WAL written, index size and leaf density.
# Writes 16-id-generation/results.md. Needs only Docker. About 5 minutes.
set -euo pipefail
cd "$(dirname "$0")/.."
OUT=16-id-generation/results.md
ROWS=${ROWS:-5000000}
BATCH=${BATCH:-50000}
C=ids-pg

docker rm -f $C >/dev/null 2>&1 || true
trap 'docker rm -f $C >/dev/null 2>&1 || true' EXIT
docker run -d --name $C -e POSTGRES_USER=lab -e POSTGRES_PASSWORD=lab -e POSTGRES_DB=lab \
  postgres:17-alpine postgres -c shared_buffers=128MB -c max_wal_size=4GB -c checkpoint_timeout=30min >/dev/null
for _ in $(seq 240); do docker exec $C pg_isready -h 127.0.0.1 -U lab >/dev/null 2>&1 && break; sleep 0.5; done
psql() { docker exec -i $C psql -U lab -d lab -Atq -v ON_ERROR_STOP=1 "$@"; }

psql <<'SQL'
CREATE EXTENSION pgstattuple;
-- PostgreSQL 17 has no built-in uuidv7() (18 does). Like 18's, this puts
-- 48 bits of Unix milliseconds first, then the version (7), then 12 bits of
-- sub-millisecond fraction (so IDs from one session stay in order even within
-- a millisecond), then the variant and random bits.
CREATE FUNCTION uuidv7() RETURNS uuid LANGUAGE sql VOLATILE AS $$
  SELECT (lpad(to_hex(ms), 12, '0') || '7' || lpad(to_hex(frac), 3, '0')
          || to_hex(8 + floor(random() * 4)::int) || substr(md5(random()::text), 1, 15))::uuid
    FROM (SELECT floor(e * 1000)::bigint AS ms, floor((e * 1000 - floor(e * 1000)) * 4096)::int AS frac
            FROM (SELECT extract(epoch FROM clock_timestamp()) AS e) t) u
$$;
SQL
VERSION=$(psql -c "SHOW server_version")
echo "== generating $ROWS keys of each kind"
psql -c "CREATE UNLOGGED TABLE src AS SELECT g AS n, gen_random_uuid() AS v4, uuidv7() AS v7, repeat(md5(g::text), 3) AS payload
         FROM generate_series(1, $ROWS) g; CREATE INDEX ON src (n);"

{
  echo "# Primary keys: bigint vs UUIDv4 vs UUIDv7"
  echo
  echo "PostgreSQL $VERSION in Docker, \`shared_buffers\` = 128 MB. $(printf "%'d" "$ROWS") rows inserted into each table in batches of $(printf "%'d" "$BATCH"), each row a key plus about 100 bytes. Measured $(date -u +%Y-%m-%d)."
  echo
  echo "| Key | Insert time | Rows/s | WAL written | Primary key index | Leaf density | Table |"
  echo "|---|---:|---:|---:|---:|---:|---:|"
} > "$OUT"

for kind in bigint uuidv4 uuidv7; do
  case $kind in
    bigint) col="id bigint PRIMARY KEY"; src=n ;;
    uuidv4) col="id uuid PRIMARY KEY"; src=v4 ;;
    uuidv7) col="id uuid PRIMARY KEY"; src=v7 ;;
  esac
  echo "== $kind"
  psql -c "DROP TABLE IF EXISTS t; CREATE TABLE t ($col, payload text NOT NULL); CHECKPOINT;"
  lsn0=$(psql -c "SELECT pg_current_wal_lsn()")
  t0=$(date +%s.%N)
  n=0
  while [ "$n" -lt "$ROWS" ]; do
    psql -c "INSERT INTO t SELECT $src, payload FROM src WHERE n > $n AND n <= $((n + BATCH)) ORDER BY n"
    n=$((n + BATCH))
  done
  secs=$(python3 -c "print(f'{$(date +%s.%N) - $t0:.1f}')")
  read -r wal idx tbl dens < <(psql -F' ' -c "SELECT pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), '$lsn0')),
      pg_size_pretty(pg_relation_size('t_pkey')), pg_size_pretty(pg_relation_size('t')),
      round((pgstatindex('t_pkey')).avg_leaf_density::numeric, 1)" | sed 's/ \([kMG]B\)/\1/g')
  rate=$(python3 -c "print(f'{$ROWS / $secs:,.0f}')")
  echo "| $kind | ${secs} s | $rate | ${wal/B/ B} | ${idx/B/ B} | ${dens}% | ${tbl/B/ B} |" | sed 's/\([0-9]\)\([kMG]\) B/\1 \2B/g' >> "$OUT"
done
echo >> "$OUT"
cat "$OUT"
