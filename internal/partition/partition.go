// Package partition compares one big PostgreSQL table with the same data
// partitioned by month: removing the oldest month, querying recent rows,
// and querying without the partition key.
package partition

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Layout struct {
	Name, Table string
}

var Layouts = []Layout{{"one table", "part.plain"}, {"partitioned by month (12 partitions)", "part.events"}}

// Setup creates both tables with the same rows: perMonth rows in each month
// of 2026, 1,000 tenants, a 100-byte payload.
func Setup(ctx context.Context, db *pgxpool.Pool, perMonth int) error {
	q := `DROP SCHEMA IF EXISTS part CASCADE; CREATE SCHEMA part;
		CREATE TABLE part.plain (created_at timestamptz NOT NULL, tenant_id int NOT NULL, payload text NOT NULL);
		CREATE TABLE part.events (created_at timestamptz NOT NULL, tenant_id int NOT NULL, payload text NOT NULL)
		  PARTITION BY RANGE (created_at);`
	for m := 1; m <= 12; m++ {
		next := fmt.Sprintf("2026-%02d-01", m+1)
		if m == 12 {
			next = "2027-01-01"
		}
		q += fmt.Sprintf(`CREATE TABLE part.events_2026_%02d PARTITION OF part.events FOR VALUES FROM ('2026-%02d-01') TO ('%s');`, m, m, next)
	}
	if _, err := db.Exec(ctx, q); err != nil {
		return err
	}
	for _, l := range Layouts {
		if _, err := db.Exec(ctx, fmt.Sprintf(`INSERT INTO %s
			SELECT timestamptz '2026-01-01' + (g::float8 / %d) * interval '365 days', 1 + g %% 1000, repeat('p', 100)
			FROM generate_series(0, %d) g`, l.Table, 12*perMonth, 12*perMonth-1)); err != nil {
			return err
		}
		if _, err := db.Exec(ctx, fmt.Sprintf(`CREATE INDEX ON %s (created_at); CREATE INDEX ON %s (tenant_id, created_at)`, l.Table, l.Table)); err != nil {
			return err
		}
		if _, err := db.Exec(ctx, `VACUUM ANALYZE `+l.Table); err != nil {
			return err
		}
	}
	return nil
}

func size(ctx context.Context, db *pgxpool.Pool, table string) (int64, error) {
	// For a partitioned table, sum its partitions (tables and indexes).
	// pg_partition_tree returns nothing for an ordinary table.
	var b int64
	err := db.QueryRow(ctx, `SELECT CASE WHEN c.relkind = 'p'
		THEN (SELECT coalesce(sum(pg_total_relation_size(relid)), 0) FROM pg_partition_tree(c.oid) WHERE isleaf)
		ELSE pg_total_relation_size(c.oid) END::bigint
		FROM pg_class c WHERE c.oid = $1::regclass`, table).Scan(&b)
	return b, err
}

func walSince(ctx context.Context, db *pgxpool.Pool, start string) (int64, error) {
	var b int64
	err := db.QueryRow(ctx, `SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), $1::pg_lsn)::bigint`, start).Scan(&b)
	return b, err
}

func lsn(ctx context.Context, db *pgxpool.Pool) (string, error) {
	var s string
	err := db.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&s)
	return s, err
}

// RetireResult: removing January.
type RetireResult struct {
	Layout, How           string
	Took                  time.Duration
	WALBytes              int64
	SizeBefore, SizeAfter int64
	VacuumTook            time.Duration // the cleanup DELETE leaves behind
	SizeAfterVacuum       int64
}

func Retire(ctx context.Context, db *pgxpool.Pool) ([]RetireResult, error) {
	var out []RetireResult
	for _, l := range Layouts {
		r := RetireResult{Layout: l.Name}
		var err error
		if r.SizeBefore, err = size(ctx, db, l.Table); err != nil {
			return nil, err
		}
		start, err := lsn(ctx, db)
		if err != nil {
			return nil, err
		}
		t := time.Now()
		if l.Table == "part.plain" {
			r.How = "`DELETE … WHERE created_at < '2026-02-01'`"
			_, err = db.Exec(ctx, `DELETE FROM part.plain WHERE created_at < '2026-02-01'`)
		} else {
			r.How = "`DETACH PARTITION` + `DROP TABLE`"
			_, err = db.Exec(ctx, `ALTER TABLE part.events DETACH PARTITION part.events_2026_01`)
			if err == nil {
				_, err = db.Exec(ctx, `DROP TABLE part.events_2026_01`)
			}
		}
		if err != nil {
			return nil, err
		}
		r.Took = time.Since(t)
		if r.WALBytes, err = walSince(ctx, db, start); err != nil {
			return nil, err
		}
		if r.SizeAfter, err = size(ctx, db, l.Table); err != nil {
			return nil, err
		}
		t = time.Now()
		if _, err := db.Exec(ctx, `VACUUM `+l.Table); err != nil {
			return nil, err
		}
		r.VacuumTook = time.Since(t)
		if r.SizeAfterVacuum, err = size(ctx, db, l.Table); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// QueryResult: one query on both layouts.
type QueryResult struct {
	Query, Layout string
	P50           time.Duration
	Partitions    int // partitions the plan touches (0 for the plain table)
}

func Queries(ctx context.Context, db *pgxpool.Pool, runs int) ([]QueryResult, error) {
	qs := []struct{ name, where string }{
		{"last 2 days (`created_at >= '2026-12-30'`)", "created_at >= '2026-12-30'"},
		{"one tenant, last 2 days", "tenant_id = 7 AND created_at >= '2026-12-30'"},
		{"one tenant, all year (no partition key)", "tenant_id = 7"},
	}
	var out []QueryResult
	for _, q := range qs {
		for _, l := range Layouts {
			sql := fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s`, l.Table, q.where)
			r := QueryResult{Query: q.name, Layout: l.Name}
			if l.Table == "part.events" {
				var plan string
				rows, err := db.Query(ctx, `EXPLAIN `+sql)
				if err != nil {
					return nil, err
				}
				seen := map[string]bool{}
				for rows.Next() {
					if err := rows.Scan(&plan); err != nil {
						return nil, err
					}
					for _, m := range partRe.FindAllString(plan, -1) {
						seen[m] = true
					}
				}
				rows.Close()
				r.Partitions = len(seen)
			}
			var lat []time.Duration
			for range runs {
				t := time.Now()
				var n int64
				if err := db.QueryRow(ctx, sql).Scan(&n); err != nil {
					return nil, err
				}
				lat = append(lat, time.Since(t))
			}
			slices.Sort(lat)
			r.P50 = lat[len(lat)/2]
			out = append(out, r)
		}
	}
	return out, nil
}

// partRe matches a partition name in a plan line (an index name starts with it too).
var partRe = regexp.MustCompile(`events_2026_\d\d`)
