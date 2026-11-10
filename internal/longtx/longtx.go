// Package longtx shows how one long-running transaction slows down a whole
// database, in PostgreSQL and in MySQL (InnoDB).
//
// Both keep old row versions so that open transactions can see the data as
// it was when they started (MVCC). PostgreSQL keeps them in the table and
// removes them with VACUUM; InnoDB keeps them in the undo log and removes
// them with purge. Either way, a version can only be removed once no open
// transaction might still need it, so one old transaction holds back the
// cleanup for everything, including tables it never touched.
package longtx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const hotRows = 100

// Params: Writers update the hot rows for Duration while VACUUM runs every
// second (PostgreSQL); then Reads point reads are timed.
type Params struct {
	Writers  int
	Duration time.Duration
	Reads    int
}

func pct(d []time.Duration, q float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	slices.Sort(d)
	return d[min(len(d)-1, int(float64(len(d))*q))]
}

// ---- PostgreSQL ---------------------------------------------------------------

type PGResult struct {
	Variant      string
	Updates      int64
	DeadLeft     int64 // dead row versions still in the table after the last VACUUM
	TableBytes   int64
	ReadP50      time.Duration
	ReadP99      time.Duration
	ScanP50      time.Duration // SELECT sum(balance) over the 100 rows
	AfterBytes   int64         // table size once the long transaction is gone and VACUUM has run
	AfterDead    int64
	AfterReadP50 time.Duration
	AfterScanP50 time.Duration
}

func pgSetup(ctx context.Context, db *pgxpool.Pool) error {
	_, err := db.Exec(ctx, fmt.Sprintf(`CREATE EXTENSION IF NOT EXISTS pgstattuple;
		CREATE SCHEMA IF NOT EXISTS longtx;
		DROP TABLE IF EXISTS longtx.accounts, longtx.reports;
		CREATE TABLE longtx.accounts (id int PRIMARY KEY, balance bigint NOT NULL);
		INSERT INTO longtx.accounts SELECT g, 0 FROM generate_series(1, %d) g;
		CREATE TABLE longtx.reports (id int PRIMARY KEY, body text);
		INSERT INTO longtx.reports VALUES (1, 'quarterly');`, hotRows))
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `VACUUM ANALYZE longtx.accounts`) // VACUUM can't share a multi-statement query
	return err
}

func pgStats(ctx context.Context, db *pgxpool.Pool) (dead, size int64, err error) {
	err = db.QueryRow(ctx, `SELECT dead_tuple_count, pg_relation_size('longtx.accounts') FROM pgstattuple('longtx.accounts')`).Scan(&dead, &size)
	return
}

func pgReads(ctx context.Context, db *pgxpool.Pool, n int, scan bool) ([]time.Duration, error) {
	lat := make([]time.Duration, 0, n)
	q := `SELECT balance FROM longtx.accounts WHERE id = $1`
	if scan {
		q = `SELECT sum(balance) FROM longtx.accounts WHERE id <> $1`
	}
	for range n {
		t := time.Now()
		var b int64
		if err := db.QueryRow(ctx, q, 1+rand.IntN(hotRows)).Scan(&b); err != nil {
			return nil, err
		}
		lat = append(lat, time.Since(t))
	}
	return lat, nil
}

// PG runs three variants: no long transaction; a transaction left open
// reading an unrelated table; the same, cut off by
// idle_in_transaction_session_timeout.
func PG(ctx context.Context, db *pgxpool.Pool, p Params) ([]PGResult, error) {
	var out []PGResult
	for _, v := range []string{"none", "open", "timeout"} {
		if err := pgSetup(ctx, db); err != nil {
			return nil, err
		}
		name := "no long transaction"
		var long *pgxpool.Conn
		if v != "none" {
			c, err := db.Acquire(ctx)
			if err != nil {
				return nil, err
			}
			long = c
			name = "a transaction left open, reading only another table"
			if v == "timeout" {
				name = "the same, with idle_in_transaction_session_timeout = 2s"
				if _, err := c.Exec(ctx, `SET idle_in_transaction_session_timeout = '2s'`); err != nil {
					return nil, err
				}
			}
			// A report, or a request that forgot to commit: it touches one
			// row of an unrelated table and then sits there.
			if _, err := c.Exec(ctx, `BEGIN ISOLATION LEVEL REPEATABLE READ`); err != nil {
				return nil, err
			}
			if _, err := c.Exec(ctx, `SELECT body FROM longtx.reports WHERE id = 1`); err != nil {
				return nil, err
			}
		}
		r := PGResult{Variant: name}
		wctx, stop := context.WithTimeout(ctx, p.Duration)
		var updates atomic.Int64
		var wg sync.WaitGroup
		var firstErr atomic.Value
		for range p.Writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for wctx.Err() == nil {
					_, err := db.Exec(wctx, `UPDATE longtx.accounts SET balance = balance + 1 WHERE id = $1`, 1+rand.IntN(hotRows))
					if err != nil {
						if wctx.Err() == nil {
							firstErr.CompareAndSwap(nil, err)
						}
						return
					}
					updates.Add(1)
				}
			}()
		}
		wg.Add(1)
		go func() { // VACUUM every second, more often than autovacuum would
			defer wg.Done()
			for wctx.Err() == nil {
				_, _ = db.Exec(ctx, `VACUUM longtx.accounts`)
				time.Sleep(time.Second)
			}
		}()
		wg.Wait()
		stop()
		if err, _ := firstErr.Load().(error); err != nil {
			return nil, err
		}
		if _, err := db.Exec(ctx, `VACUUM longtx.accounts`); err != nil {
			return nil, err
		}
		r.Updates = updates.Load()
		var err error
		if r.DeadLeft, r.TableBytes, err = pgStats(ctx, db); err != nil {
			return nil, err
		}
		lat, err := pgReads(ctx, db, p.Reads, false)
		if err != nil {
			return nil, err
		}
		r.ReadP50, r.ReadP99 = pct(lat, 0.5), pct(lat, 0.99)
		if lat, err = pgReads(ctx, db, p.Reads/10, true); err != nil {
			return nil, err
		}
		r.ScanP50 = pct(lat, 0.5)
		if long != nil {
			_, _ = long.Exec(ctx, `ROLLBACK`) // fails if the timeout already ended the session
			long.Release()
		}
		if _, err := db.Exec(ctx, `VACUUM longtx.accounts`); err != nil {
			return nil, err
		}
		if r.AfterDead, r.AfterBytes, err = pgStats(ctx, db); err != nil {
			return nil, err
		}
		if lat, err = pgReads(ctx, db, p.Reads, false); err != nil {
			return nil, err
		}
		r.AfterReadP50 = pct(lat, 0.5)
		if lat, err = pgReads(ctx, db, p.Reads/10, true); err != nil {
			return nil, err
		}
		r.AfterScanP50 = pct(lat, 0.5)
		out = append(out, r)
	}
	_, err := db.Exec(ctx, `DROP SCHEMA longtx CASCADE`)
	return out, err
}

// ---- MySQL (InnoDB) -------------------------------------------------------------

type MySQLResult struct {
	Variant       string
	Updates       int64
	HistoryLength int64         // undo records purge hasn't been allowed to remove
	ReadNewP50    time.Duration // a fresh session reading the latest versions
	ReadOldP50    time.Duration // the long transaction reading the same rows
	ScanNewP50    time.Duration // SELECT sum(balance), fresh session
	ScanOldP50    time.Duration // the same, inside the long transaction
}

var historyRe = regexp.MustCompile(`History list length (\d+)`)

func history(ctx context.Context, db *sql.DB) (int64, error) {
	var typ, name, status string
	if err := db.QueryRowContext(ctx, `SHOW ENGINE INNODB STATUS`).Scan(&typ, &name, &status); err != nil {
		return 0, err
	}
	m := historyRe.FindStringSubmatch(status)
	if m == nil {
		return 0, errors.New("no history list length in InnoDB status")
	}
	return strconv.ParseInt(m[1], 10, 64)
}

type querier interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

func myReads(ctx context.Context, q querier, n int, scan bool) ([]time.Duration, error) {
	lat := make([]time.Duration, 0, n)
	sqlText := `SELECT balance FROM accounts WHERE id = ?`
	if scan {
		sqlText = `SELECT sum(balance) FROM accounts WHERE id <> ?`
	}
	for range n {
		t := time.Now()
		var b int64
		if err := q.QueryRowContext(ctx, sqlText, 1+rand.IntN(hotRows)).Scan(&b); err != nil {
			return nil, err
		}
		lat = append(lat, time.Since(t))
	}
	return lat, nil
}

// MySQL runs: no long transaction; a consistent-snapshot transaction left
// open (reading another table) while the hot rows are updated.
func MySQL(ctx context.Context, db *sql.DB, p Params) ([]MySQLResult, error) {
	// Don't flush the log on every commit, so the writers aren't limited by
	// the laptop's disk. It doesn't change how old versions are kept.
	if _, err := db.ExecContext(ctx, `SET GLOBAL innodb_flush_log_at_trx_commit = 2`); err != nil {
		return nil, err
	}
	defer db.ExecContext(context.Background(), `SET GLOBAL innodb_flush_log_at_trx_commit = 1`)
	var out []MySQLResult
	for _, open := range []bool{false, true} {
		for _, q := range []string{`DROP TABLE IF EXISTS accounts, reports`,
			`CREATE TABLE accounts (id int PRIMARY KEY, balance bigint NOT NULL) ENGINE=InnoDB`,
			`CREATE TABLE reports (id int PRIMARY KEY, body text) ENGINE=InnoDB`,
			`INSERT INTO reports VALUES (1, 'quarterly')`} {
			if _, err := db.ExecContext(ctx, q); err != nil {
				return nil, err
			}
		}
		for i := 1; i <= hotRows; i++ {
			if _, err := db.ExecContext(ctx, `INSERT INTO accounts VALUES (?, 0)`, i); err != nil {
				return nil, err
			}
		}
		time.Sleep(2 * time.Second) // let purge catch up with setup
		r := MySQLResult{Variant: "no long transaction"}
		var long *sql.Conn
		if open {
			c, err := db.Conn(ctx)
			if err != nil {
				return nil, err
			}
			long = c
			r.Variant = "a consistent-snapshot transaction left open, reading only another table"
			if _, err := c.ExecContext(ctx, `START TRANSACTION WITH CONSISTENT SNAPSHOT`); err != nil {
				return nil, err
			}
			var body string
			if err := c.QueryRowContext(ctx, `SELECT body FROM reports WHERE id = 1`).Scan(&body); err != nil {
				return nil, err
			}
		}
		wctx, stop := context.WithTimeout(ctx, p.Duration)
		var updates atomic.Int64
		var wg sync.WaitGroup
		for range p.Writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for wctx.Err() == nil {
					if _, err := db.ExecContext(wctx, `UPDATE accounts SET balance = balance + 1 WHERE id = ?`, 1+rand.IntN(hotRows)); err == nil {
						updates.Add(1)
					}
				}
			}()
		}
		wg.Wait()
		stop()
		time.Sleep(2 * time.Second) // give purge its chance
		r.Updates = updates.Load()
		var err error
		if r.HistoryLength, err = history(ctx, db); err != nil {
			return nil, err
		}
		lat, err := myReads(ctx, db, p.Reads, false)
		if err != nil {
			return nil, err
		}
		r.ReadNewP50 = pct(lat, 0.5)
		if lat, err = myReads(ctx, db, p.Reads/10, true); err != nil {
			return nil, err
		}
		r.ScanNewP50 = pct(lat, 0.5)
		if long != nil {
			// Each read rebuilds the old version of every row from the undo log.
			if lat, err = myReads(ctx, long, p.Reads/10, false); err != nil {
				return nil, err
			}
			r.ReadOldP50 = pct(lat, 0.5)
			if lat, err = myReads(ctx, long, 10, true); err != nil {
				return nil, err
			}
			r.ScanOldP50 = pct(lat, 0.5)
			_, _ = long.ExecContext(ctx, `ROLLBACK`)
			long.Close()
		}
		out = append(out, r)
	}
	return out, nil
}
