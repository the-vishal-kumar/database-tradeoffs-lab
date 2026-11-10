// Package replicas measures what a PostgreSQL streaming replica trades
// away: how far behind it runs, how often a client that just wrote reads an
// older value from it, what each synchronous_commit level costs to prevent
// that, and what happens to commits when a synchronous replica is gone.
package replicas

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Env struct{ Primary, Replica *pgxpool.Pool }

func pct(d []time.Duration, q float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	slices.Sort(d)
	return d[min(len(d)-1, int(float64(len(d))*q))]
}

func (e Env) setup(ctx context.Context) error {
	_, err := e.Primary.Exec(ctx, `DROP SCHEMA IF EXISTS rep CASCADE; CREATE SCHEMA rep;
		CREATE TABLE rep.counters (id int PRIMARY KEY, v bigint NOT NULL);
		INSERT INTO rep.counters SELECT g, 0 FROM generate_series(1, 100) g;
		CREATE TABLE rep.events (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, payload text NOT NULL);`)
	if err != nil {
		return err
	}
	return e.waitReplica(ctx, `SELECT count(*) = 100 FROM rep.counters`)
}

func (e Env) waitReplica(ctx context.Context, q string) error {
	for range 100 {
		var ok bool
		if e.Replica.QueryRow(ctx, q).Scan(&ok) == nil && ok {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("replica never caught up: %s", q)
}

func (e Env) setSyncStandby(ctx context.Context, names string) error {
	if _, err := e.Primary.Exec(ctx, fmt.Sprintf(`ALTER SYSTEM SET synchronous_standby_names = '%s'`, names)); err != nil {
		return err
	}
	_, err := e.Primary.Exec(ctx, `SELECT pg_reload_conf()`)
	time.Sleep(500 * time.Millisecond)
	return err
}

// background writes 1 kB rows from n sessions until ctx ends, without
// waiting for the replica, so the replica always has WAL to apply.
func (e Env) background(ctx context.Context, n int) *sync.WaitGroup {
	var wg sync.WaitGroup
	payload := strings.Repeat("x", 1024)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				_ = pgx.BeginFunc(ctx, e.Primary, func(tx pgx.Tx) error {
					if _, err := tx.Exec(ctx, `SET LOCAL synchronous_commit = local`); err != nil {
						return err
					}
					_, err := tx.Exec(ctx, `INSERT INTO rep.events (payload) VALUES ($1)`, payload)
					return err
				})
			}
		}()
	}
	return &wg
}

// ---- A: read your own write from the replica -----------------------------------

type ModeResult struct {
	Mode           string
	Writes         int
	CommitP50      time.Duration
	CommitP99      time.Duration
	StaleReads     int // the replica returned a value older than the one just written
	LagP50, LagMax time.Duration
}

// ReadYourWrites: under background load, each iteration commits an
// increment on the primary at the given synchronous_commit level, then
// immediately reads the row from the replica.
func (e Env) ReadYourWrites(ctx context.Context, iterations int) ([]ModeResult, error) {
	if err := e.setup(ctx); err != nil {
		return nil, err
	}
	if err := e.setSyncStandby(ctx, "replica1"); err != nil {
		return nil, err
	}
	defer e.setSyncStandby(context.Background(), "")
	var out []ModeResult
	for _, mode := range []string{"local", "remote_write", "on", "remote_apply"} {
		bctx, stop := context.WithCancel(ctx)
		wg := e.background(bctx, 4)
		var lags []time.Duration
		lagDone := make(chan struct{})
		go func() { // the replica's own view of its lag
			defer close(lagDone)
			for bctx.Err() == nil {
				var l time.Duration
				if e.Replica.QueryRow(ctx, `SELECT coalesce(now() - pg_last_xact_replay_timestamp(), '0')`).Scan(&l) == nil {
					lags = append(lags, l)
				}
				time.Sleep(20 * time.Millisecond)
			}
		}()
		time.Sleep(time.Second) // let the background load settle
		r := ModeResult{Mode: mode, Writes: iterations}
		var commits []time.Duration
		for range iterations {
			id := 1 + rand.IntN(100)
			var wrote, read int64
			t := time.Now()
			err := pgx.BeginFunc(ctx, e.Primary, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `SET LOCAL synchronous_commit = `+mode); err != nil {
					return err
				}
				return tx.QueryRow(ctx, `UPDATE rep.counters SET v = v + 1 WHERE id = $1 RETURNING v`, id).Scan(&wrote)
			})
			commits = append(commits, time.Since(t))
			if err != nil {
				stop()
				return nil, err
			}
			if err := e.Replica.QueryRow(ctx, `SELECT v FROM rep.counters WHERE id = $1`, id).Scan(&read); err != nil {
				stop()
				return nil, err
			}
			if read < wrote {
				r.StaleReads++
			}
		}
		stop()
		wg.Wait()
		<-lagDone
		r.CommitP50, r.CommitP99 = pct(commits, 0.5), pct(commits, 0.99)
		r.LagP50, r.LagMax = pct(lags, 0.5), pct(lags, 1)
		out = append(out, r)
	}
	return out, nil
}

// ---- B: lag during a big write ---------------------------------------------------

type BatchResult struct {
	Rows                int
	WALBytes            int64
	Took                time.Duration
	MaxLagBytes         int64
	MaxLag              time.Duration
	CaughtUpAfterCommit time.Duration
}

// BigBatch inserts rows in one statement and watches how far behind the
// replica falls, and how long it takes to catch up after the commit.
func (e Env) BigBatch(ctx context.Context, rows int) (BatchResult, error) {
	if err := e.setup(ctx); err != nil {
		return BatchResult{}, err
	}
	r := BatchResult{Rows: rows}
	var maxBytes atomic.Int64
	var maxLag atomic.Int64
	done := make(chan struct{})
	watch := make(chan struct{})
	go func() {
		defer close(watch)
		for {
			select {
			case <-done:
				return
			default:
			}
			var b int64
			var l time.Duration
			if e.Primary.QueryRow(ctx, `SELECT coalesce(pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn), 0)::bigint,
				coalesce(replay_lag, '0') FROM pg_stat_replication WHERE application_name = 'replica1'`).Scan(&b, &l) == nil {
				maxBytes.Store(max(maxBytes.Load(), b))
				maxLag.Store(max(maxLag.Load(), int64(l)))
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	var start string
	if err := e.Primary.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&start); err != nil {
		close(done)
		return r, err
	}
	t := time.Now()
	if _, err := e.Primary.Exec(ctx, fmt.Sprintf(`INSERT INTO rep.events (payload) SELECT repeat('y', 200) FROM generate_series(1, %d)`, rows)); err != nil {
		close(done)
		return r, err
	}
	r.Took = time.Since(t)
	if err := e.Primary.QueryRow(ctx, `SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), $1::pg_lsn)::bigint`, start).Scan(&r.WALBytes); err != nil {
		close(done)
		return r, err
	}
	t = time.Now()
	if err := e.waitReplica(ctx, fmt.Sprintf(`SELECT count(*) >= %d FROM rep.events`, rows)); err != nil {
		close(done)
		return r, err
	}
	r.CaughtUpAfterCommit = time.Since(t)
	close(done)
	<-watch
	r.MaxLagBytes, r.MaxLag = maxBytes.Load(), time.Duration(maxLag.Load())
	return r, nil
}

// ---- C: the synchronous replica is gone ------------------------------------------

type OutageResult struct {
	TimeoutWaited      time.Duration // with statement_timeout set: how long the commit waited
	TimeoutGuardHit    bool          // the client had to give up: statement_timeout didn't end the wait
	CancelWaited       time.Duration // with another session cancelling the wait after `wait`
	CancelErr          string        // what the client got back
	Warning, Detail    string        // what PostgreSQL said when the wait was cancelled
	VisibleOnPrimary   bool          // the "unconfirmed" commit is there anyway
	AsyncCommitLatency time.Duration
}

// SyncStandbyGone requires a synchronous standby that doesn't exist (as if
// the only replica had died), then tries to commit two ways.
func (e Env) SyncStandbyGone(ctx context.Context, wait time.Duration) (OutageResult, error) {
	var r OutageResult
	if err := e.setup(ctx); err != nil {
		return r, err
	}
	if err := e.setSyncStandby(ctx, "replica_that_died"); err != nil {
		return r, err
	}
	defer e.setSyncStandby(context.Background(), "")
	var mu sync.Mutex
	cfg := e.Primary.Config().ConnConfig.Copy()
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		mu.Lock()
		r.Warning, r.Detail = n.Message, n.Detail
		mu.Unlock()
	}

	// (a) statement_timeout, with a client-side guard in case it doesn't apply.
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return r, err
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf(`SET statement_timeout = '%dms'`, wait.Milliseconds())); err != nil {
		return r, err
	}
	guard, cancel := context.WithTimeout(ctx, 3*wait)
	t := time.Now()
	_, err = conn.Exec(guard, `UPDATE rep.counters SET v = 41 WHERE id = 1`)
	r.TimeoutWaited = time.Since(t)
	r.TimeoutGuardHit = errors.Is(err, context.DeadlineExceeded) || guard.Err() != nil
	cancel()
	conn.Close(context.Background())
	// The guard only hung up the client; cancel any wait still in progress.
	_, _ = e.Primary.Exec(ctx, `SELECT pg_cancel_backend(pid) FROM pg_stat_activity WHERE wait_event = 'SyncRep'`)
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	r.Warning, r.Detail = "", ""
	mu.Unlock()

	// (b) another session cancels the wait, as Ctrl-C in psql would.
	conn, err = pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return r, err
	}
	defer conn.Close(context.Background())
	var pid int
	if err := conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		return r, err
	}
	go func() {
		time.Sleep(wait)
		_, _ = e.Primary.Exec(ctx, `SELECT pg_cancel_backend($1)`, pid)
	}()
	t = time.Now()
	_, err = conn.Exec(ctx, `UPDATE rep.counters SET v = 42 WHERE id = 1`)
	r.CancelWaited = time.Since(t)
	if err != nil {
		r.CancelErr = err.Error()
	}
	var v int64
	if err := e.Primary.QueryRow(ctx, `SELECT v FROM rep.counters WHERE id = 1`).Scan(&v); err != nil {
		return r, err
	}
	r.VisibleOnPrimary = v == 42

	t = time.Now()
	if err := pgx.BeginFunc(ctx, e.Primary, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL synchronous_commit = local`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE rep.counters SET v = 43 WHERE id = 1`)
		return err
	}); err != nil {
		return r, err
	}
	r.AsyncCommitLatency = time.Since(t)
	mu.Lock()
	defer mu.Unlock()
	return r, nil
}
