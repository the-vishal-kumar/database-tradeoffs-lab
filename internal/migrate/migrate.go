// Package migrate runs schema changes on a PostgreSQL table while clients
// keep reading and writing it, and measures how long those clients stall:
// the one-step change most people write first, and the online way.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Env struct {
	DB   *pgxpool.Pool
	Rows int
}

// Setup creates mig.accounts with an int primary key and some columns.
func (e Env) Setup(ctx context.Context) error {
	_, err := e.DB.Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS mig CASCADE; CREATE SCHEMA mig;
		CREATE TABLE mig.accounts (id int PRIMARY KEY, owner_id int NOT NULL, balance int NOT NULL, note text NOT NULL);
		INSERT INTO mig.accounts SELECT g, g %% 100000, 0, repeat('n', 60) FROM generate_series(1, %d) g;`, e.Rows))
	if err != nil {
		return err
	}
	_, err = e.DB.Exec(ctx, `VACUUM ANALYZE mig.accounts`)
	return err
}

// Load is what clients experienced while a change ran.
type Load struct {
	Requests      int
	P50, P99, Max time.Duration
	Over1s        int // requests that took longer than a second
	Errors        int
}

// underLoad runs fn while 8 clients each loop: read a random row, update a
// random row's balance. It returns fn's duration and the clients' latencies.
func (e Env) underLoad(ctx context.Context, fn func(context.Context) error) (took time.Duration, l Load, err error) {
	lctx, stop := context.WithCancel(ctx)
	var mu sync.Mutex
	var lat []time.Duration
	var errs atomic.Int64
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var mine []time.Duration
			for lctx.Err() == nil {
				id := 1 + rand.IntN(e.Rows)
				t := time.Now()
				var b int
				err := e.DB.QueryRow(lctx, `SELECT balance FROM mig.accounts WHERE id = $1`, id).Scan(&b)
				if err == nil {
					_, err = e.DB.Exec(lctx, `UPDATE mig.accounts SET balance = balance + 1 WHERE id = $1`, 1+rand.IntN(e.Rows))
				}
				if lctx.Err() != nil {
					break
				}
				if err != nil {
					errs.Add(1)
					continue
				}
				mine = append(mine, time.Since(t))
			}
			mu.Lock()
			lat = append(lat, mine...)
			mu.Unlock()
		}()
	}
	time.Sleep(500 * time.Millisecond) // steady state first
	t := time.Now()
	err = fn(ctx)
	took = time.Since(t)
	time.Sleep(500 * time.Millisecond)
	stop()
	wg.Wait()
	slices.Sort(lat)
	l = Load{Requests: len(lat), Errors: int(errs.Load())}
	if len(lat) > 0 {
		l.P50, l.P99, l.Max = lat[len(lat)/2], lat[len(lat)*99/100], lat[len(lat)-1]
	}
	for _, d := range lat {
		if d > time.Second {
			l.Over1s++
		}
	}
	return took, l, err
}

// Step is one way of making a change.
type Step struct {
	Change, How string
	Took        time.Duration
	Load        Load
	Note        string
}

func exec(db *pgxpool.Pool, sql string) func(context.Context) error {
	return func(ctx context.Context) error { _, err := db.Exec(ctx, sql); return err }
}

// ColumnType: id int -> bigint (the "we're running out of ids" migration).
func (e Env) ColumnType(ctx context.Context) ([]Step, error) {
	var out []Step
	if err := e.Setup(ctx); err != nil {
		return nil, err
	}
	took, l, err := e.underLoad(ctx, exec(e.DB, `ALTER TABLE mig.accounts ALTER COLUMN owner_id TYPE bigint`))
	if err != nil {
		return nil, err
	}
	out = append(out, Step{"`owner_id` int → bigint", "`ALTER COLUMN … TYPE bigint` (rewrites the table)", took, l, ""})

	if err := e.Setup(ctx); err != nil {
		return nil, err
	}
	took, l, err = e.underLoad(ctx, func(ctx context.Context) error {
		// Expand: a new column, kept in sync by a trigger for new writes.
		for _, q := range []string{
			`ALTER TABLE mig.accounts ADD COLUMN owner_id_new bigint`,
			`CREATE FUNCTION mig.sync_owner() RETURNS trigger LANGUAGE plpgsql AS $$
			   BEGIN NEW.owner_id_new := NEW.owner_id; RETURN NEW; END $$`,
			`CREATE TRIGGER sync_owner BEFORE INSERT OR UPDATE ON mig.accounts FOR EACH ROW EXECUTE FUNCTION mig.sync_owner()`,
		} {
			if _, err := e.DB.Exec(ctx, q); err != nil {
				return err
			}
		}
		// Backfill in small batches, each its own short transaction.
		for lo := 1; lo <= e.Rows; lo += 10000 {
			if _, err := e.DB.Exec(ctx, `UPDATE mig.accounts SET owner_id_new = owner_id
				WHERE id BETWEEN $1 AND $2 AND owner_id_new IS NULL`, lo, lo+9999); err != nil {
				return err
			}
		}
		// Prove NOT NULL without blocking writes: a NOT VALID check is
		// instant, and VALIDATE scans while allowing reads and writes.
		for _, q := range []string{
			`ALTER TABLE mig.accounts ADD CONSTRAINT owner_new_not_null CHECK (owner_id_new IS NOT NULL) NOT VALID`,
			`ALTER TABLE mig.accounts VALIDATE CONSTRAINT owner_new_not_null`,
		} {
			if _, err := e.DB.Exec(ctx, q); err != nil {
				return err
			}
		}
		// Contract: swap names in one short transaction, drop the old column.
		// SET NOT NULL uses the validated check (PostgreSQL 12+): no scan.
		return pgx.BeginFunc(ctx, e.DB, func(tx pgx.Tx) error {
			for _, q := range []string{
				`SET LOCAL lock_timeout = '2s'`,
				`DROP TRIGGER sync_owner ON mig.accounts`,
				`ALTER TABLE mig.accounts DROP COLUMN owner_id`,
				`ALTER TABLE mig.accounts RENAME COLUMN owner_id_new TO owner_id`,
				`ALTER TABLE mig.accounts ALTER COLUMN owner_id SET NOT NULL`,
				`ALTER TABLE mig.accounts DROP CONSTRAINT owner_new_not_null`,
			} {
				if _, err := tx.Exec(ctx, q); err != nil {
					return err
				}
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	out = append(out, Step{"`owner_id` int → bigint", "expand (new column + trigger) / backfill in 10K-row batches / `CHECK … NOT VALID` + `VALIDATE` / contract", took, l,
		"the contract step only renames and drops; `SET NOT NULL` reuses the validated check instead of scanning under a lock"})
	return out, nil
}

// Index: CREATE INDEX vs CREATE INDEX CONCURRENTLY.
func (e Env) Index(ctx context.Context) ([]Step, error) {
	var out []Step
	for _, how := range []string{"CREATE INDEX", "CREATE INDEX CONCURRENTLY"} {
		if err := e.Setup(ctx); err != nil {
			return nil, err
		}
		took, l, err := e.underLoad(ctx, exec(e.DB, how+` accounts_owner_idx ON mig.accounts (owner_id)`))
		if err != nil {
			return nil, err
		}
		out = append(out, Step{"index on `owner_id`", "`" + how + "`", took, l, ""})
	}
	return out, nil
}

// AddColumn: a NOT NULL column with a constant default (metadata-only since
// PostgreSQL 11) vs one with a volatile default (rewrites the table).
func (e Env) AddColumn(ctx context.Context) ([]Step, error) {
	var out []Step
	for _, c := range []struct{ how, sql string }{
		{"`ADD COLUMN status text NOT NULL DEFAULT 'active'`", `ALTER TABLE mig.accounts ADD COLUMN status text NOT NULL DEFAULT 'active'`},
		{"`ADD COLUMN created_at timestamptz NOT NULL DEFAULT clock_timestamp()` (volatile default: rewrite)", `ALTER TABLE mig.accounts ADD COLUMN created_at timestamptz NOT NULL DEFAULT clock_timestamp()`},
	} {
		if err := e.Setup(ctx); err != nil {
			return nil, err
		}
		took, l, err := e.underLoad(ctx, exec(e.DB, c.sql))
		if err != nil {
			return nil, err
		}
		out = append(out, Step{"add a NOT NULL column", c.how, took, l, ""})
	}
	return out, nil
}

// LockQueue: a long read holds a weak lock; an ALTER queues for a strong
// one; every query after it queues behind the ALTER. With lock_timeout the
// ALTER gives up instead, and traffic carries on.
func (e Env) LockQueue(ctx context.Context, report time.Duration) ([]Step, error) {
	var out []Step
	for _, timeout := range []time.Duration{0, 200 * time.Millisecond} {
		if err := e.Setup(ctx); err != nil {
			return nil, err
		}
		took, l, err := e.underLoad(ctx, func(ctx context.Context) error {
			// A report that reads the table for `report`.
			repDone := make(chan struct{})
			go func() {
				defer close(repDone)
				_ = pgx.BeginFunc(ctx, e.DB, func(tx pgx.Tx) error {
					if _, err := tx.Exec(ctx, `SELECT count(*) FROM mig.accounts`); err != nil {
						return err
					}
					_, err := tx.Exec(ctx, fmt.Sprintf(`SELECT pg_sleep(%f)`, report.Seconds()))
					return err
				})
			}()
			time.Sleep(200 * time.Millisecond)
			// A tiny, metadata-only change, which still needs an exclusive lock.
			err := pgx.BeginFunc(ctx, e.DB, func(tx pgx.Tx) error {
				if timeout > 0 {
					if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL lock_timeout = '%dms'`, timeout.Milliseconds())); err != nil {
						return err
					}
				}
				_, err := tx.Exec(ctx, `ALTER TABLE mig.accounts ADD COLUMN flag boolean`)
				return err
			})
			<-repDone
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.Code == "55P03" {
				return nil // gave up waiting: retry later, which is the point
			}
			return err
		})
		if err != nil {
			return nil, err
		}
		how := "`ALTER TABLE … ADD COLUMN flag boolean`, no lock_timeout"
		note := "every request after the ALTER queued until the report finished"
		if timeout > 0 {
			how = fmt.Sprintf("the same, with `lock_timeout = '%dms'`", timeout.Milliseconds())
			note = "the ALTER gave up after 200 ms (retry later); traffic carried on. \"Change took\" includes waiting for the report to end"
		}
		out = append(out, Step{fmt.Sprintf("a %s report is running", report), how, took, l, note})
	}
	return out, nil
}
