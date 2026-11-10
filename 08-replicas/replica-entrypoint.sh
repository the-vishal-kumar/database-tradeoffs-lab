#!/bin/sh
# Clones the primary with pg_basebackup and starts as a hot standby.
set -e
export PGPASSWORD=replicator
until pg_isready -h pgprimary -U replicator -d lab >/dev/null 2>&1; do sleep 1; done
if [ ! -s "$PGDATA/PG_VERSION" ]; then
  mkdir -p "$PGDATA" && chown postgres:postgres "$PGDATA" && chmod 700 "$PGDATA"
  gosu postgres pg_basebackup -h pgprimary -U replicator -D "$PGDATA" -R -X stream -c fast \
    -d "host=pgprimary user=replicator application_name=replica1"
fi
exec gosu postgres postgres -c hot_standby=on -c hot_standby_feedback=off
