// Package dlock shows why a Redis lock with a TTL doesn't by itself make a
// job run once, and compares it with fencing tokens, PostgreSQL advisory
// locks and claiming a row.
//
// The failure being reproduced: a lock holder stops (a GC pause, a VM
// migration, a slow disk, a network partition) for longer than its lease.
// The lease expires, someone else takes the lock, and the first holder
// wakes up still believing it holds the lock. Every scenario uses real
// time, so the TTLs really expire; the order of events is scripted, so the
// outcome is the same on every run.
package dlock

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type Env struct {
	DB    *pgxpool.Pool
	Redis *redis.Client
}

func Connect(ctx context.Context, dbURL, redisURL string) (*Env, func(), error) {
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return nil, nil, err
	}
	cfg.MaxConns = 32
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	rdb := redis.NewClient(opt)
	if err := rdb.Ping(ctx).Err(); err != nil {
		pool.Close()
		return nil, nil, err
	}
	return &Env{DB: pool, Redis: rdb}, func() {
		_, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS dlock CASCADE`)
		pool.Close()
		_ = rdb.Close()
	}, nil
}

const lockKey, fenceKey = "dlock:lock", "dlock:fence"

// Release only if we still own the lock: compare and delete, atomically.
var unlockScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0`)

func acquire(ctx context.Context, e *Env, owner string, ttl time.Duration) (bool, error) {
	return e.Redis.SetNX(ctx, lockKey, owner, ttl).Result()
}

func release(ctx context.Context, e *Env, owner string) error {
	return unlockScript.Run(ctx, e.Redis, []string{lockKey}, owner).Err()
}

func reset(ctx context.Context, e *Env) error {
	if err := e.Redis.Del(ctx, lockKey, fenceKey).Err(); err != nil {
		return err
	}
	_, err := e.DB.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS dlock;
		DROP TABLE IF EXISTS dlock.resource, dlock.runs, dlock.claims;
		CREATE TABLE dlock.resource (id int PRIMARY KEY, value text NOT NULL, fence bigint NOT NULL DEFAULT 0);
		INSERT INTO dlock.resource VALUES (1, 'initial', 0);
		CREATE TABLE dlock.runs (run_id int NOT NULL, instance int NOT NULL);
		CREATE TABLE dlock.claims (run_id int PRIMARY KEY);`)
	return err
}

func value(ctx context.Context, e *Env) (string, error) {
	var v string
	err := e.DB.QueryRow(ctx, `SELECT value FROM dlock.resource WHERE id = 1`).Scan(&v)
	return v, err
}

// PauseResult: holder A takes the lock and pauses past the lease; B comes
// along, takes the lock and writes; then A wakes up and writes.
type PauseResult struct {
	Strategy  string
	BothHeld  bool   // did B get the lock while A still thought it held it?
	LateWrite string // what happened to A's write after the pause
	Final     string // the value left in the database
}

// Paused runs the paused-holder story against each strategy. ttl is the
// Redis lease; A pauses for ttl + 50 ms.
func Paused(ctx context.Context, e *Env, ttl time.Duration) ([]PauseResult, error) {
	pause := ttl + 50*time.Millisecond
	var out []PauseResult
	write := func(v string) error {
		_, err := e.DB.Exec(ctx, `UPDATE dlock.resource SET value = $1 WHERE id = 1`, v)
		return err
	}

	// 1. Redis lease lock, released safely, no fencing.
	if err := reset(ctx, e); err != nil {
		return nil, err
	}
	if ok, err := acquire(ctx, e, "A", ttl); err != nil || !ok {
		return nil, fmt.Errorf("A acquire: %v %v", ok, err)
	}
	time.Sleep(pause) // A is paused; its lease runs out
	bOK, err := acquire(ctx, e, "B", ttl)
	if err != nil {
		return nil, err
	}
	if bOK {
		if err := write("written by B"); err != nil {
			return nil, err
		}
		_ = release(ctx, e, "B")
	}
	if err := write("written by A, after its pause"); err != nil { // A wakes up and carries on
		return nil, err
	}
	_ = release(ctx, e, "A")
	v, err := value(ctx, e)
	if err != nil {
		return nil, err
	}
	out = append(out, PauseResult{"Redis lock (SET NX PX), safe release", bOK, "applied: overwrote B's newer write", v})

	// 2. The same lock, plus a fencing token checked by the database.
	if err := reset(ctx, e); err != nil {
		return nil, err
	}
	fenced := func(owner string) (int64, error) {
		ok, err := acquire(ctx, e, owner, ttl)
		if err != nil || !ok {
			return 0, fmt.Errorf("%s acquire: %v %v", owner, ok, err)
		}
		return e.Redis.Incr(ctx, fenceKey).Result() // a number that only grows
	}
	fencedWrite := func(v string, token int64) (bool, error) {
		tag, err := e.DB.Exec(ctx, `UPDATE dlock.resource SET value = $1, fence = $2 WHERE id = 1 AND fence < $2`, v, token)
		return tag.RowsAffected() == 1, err
	}
	tokA, err := fenced("A")
	if err != nil {
		return nil, err
	}
	time.Sleep(pause)
	tokB, err := fenced("B")
	if err != nil {
		return nil, err
	}
	if _, err := fencedWrite("written by B", tokB); err != nil {
		return nil, err
	}
	_ = release(ctx, e, "B")
	okA, err := fencedWrite("written by A, after its pause", tokA)
	if err != nil {
		return nil, err
	}
	late := fmt.Sprintf("rejected: token %d < %d", tokA, tokB)
	if okA {
		late = "applied"
	}
	v, err = value(ctx, e)
	if err != nil {
		return nil, err
	}
	out = append(out, PauseResult{"Redis lock + fencing token checked on write", true, late, v})

	// 3. PostgreSQL advisory lock, taken in the transaction that writes.
	if err := reset(ctx, e); err != nil {
		return nil, err
	}
	txA, err := e.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer txA.Rollback(ctx)
	var got bool
	if err := txA.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(42)`).Scan(&got); err != nil || !got {
		return nil, fmt.Errorf("A advisory: %v %v", got, err)
	}
	time.Sleep(pause) // A pauses; the lock has no TTL, it lasts as long as A's transaction
	var bGot bool
	if err := e.DB.QueryRow(ctx, `SELECT pg_try_advisory_lock(42)`).Scan(&bGot); err != nil {
		return nil, err
	}
	if _, err := txA.Exec(ctx, `UPDATE dlock.resource SET value = $1 WHERE id = 1`, "written by A, after its pause"); err != nil {
		return nil, err
	}
	if err := txA.Commit(ctx); err != nil {
		return nil, err
	}
	// B tries again now that A committed, and goes second.
	if err := pgx.BeginFunc(ctx, e.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(42)`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE dlock.resource SET value = $1 WHERE id = 1`, "written by B")
		return err
	}); err != nil {
		return nil, err
	}
	v, err = value(ctx, e)
	if err != nil {
		return nil, err
	}
	out = append(out, PauseResult{"PostgreSQL advisory lock in the writing transaction", bGot,
		"applied: A still held the lock, so B waited and went second", v})

	// 4. The same, but A's connection dies during the pause (a network
	// partition, or the server killing an idle-in-transaction session).
	if err := reset(ctx, e); err != nil {
		return nil, err
	}
	txA, err = e.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer txA.Rollback(ctx)
	var pidA int
	if err := txA.QueryRow(ctx, `SELECT pg_backend_pid() FROM (SELECT pg_try_advisory_xact_lock(42)) l`).Scan(&pidA); err != nil {
		return nil, err
	}
	time.Sleep(pause / 2)
	if _, err := e.DB.Exec(ctx, `SELECT pg_terminate_backend($1)`, pidA); err != nil {
		return nil, err
	}
	time.Sleep(pause / 2)
	if err := pgx.BeginFunc(ctx, e.DB, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(42)`).Scan(&ok); err != nil || !ok {
			return fmt.Errorf("B advisory after A's connection died: %v %v", ok, err)
		}
		_, err := tx.Exec(ctx, `UPDATE dlock.resource SET value = $1 WHERE id = 1`, "written by B")
		return err
	}); err != nil {
		return nil, err
	}
	late = "applied"
	if _, err := txA.Exec(ctx, `UPDATE dlock.resource SET value = $1 WHERE id = 1`, "written by A, after its pause"); err != nil {
		late = "failed: A's transaction died with its connection"
	} else if err := txA.Commit(ctx); err != nil {
		late = "failed at commit: A's transaction died with its connection"
	}
	v, err = value(ctx, e)
	if err != nil {
		return nil, err
	}
	out = append(out, PauseResult{"Advisory lock, A's connection killed during the pause", true, late, v})
	return out, nil
}

// UnlockResult: A pauses past its lease, B takes the lock, then A releases.
type UnlockResult struct {
	Strategy  string
	CAcquired bool // could C take the lock while B was still working?
}

func Unlock(ctx context.Context, e *Env, ttl time.Duration) ([]UnlockResult, error) {
	var out []UnlockResult
	for _, safe := range []bool{false, true} {
		if err := reset(ctx, e); err != nil {
			return nil, err
		}
		if ok, err := acquire(ctx, e, "A", ttl); err != nil || !ok {
			return nil, fmt.Errorf("A acquire: %v %v", ok, err)
		}
		time.Sleep(ttl + 50*time.Millisecond)
		if ok, err := acquire(ctx, e, "B", ttl*10); err != nil || !ok {
			return nil, fmt.Errorf("B acquire: %v %v", ok, err)
		}
		name := "DEL lock (release without checking the owner)"
		if safe {
			name = "compare-and-delete in Lua (release only your own lock)"
			if err := release(ctx, e, "A"); err != nil {
				return nil, err
			}
		} else if err := e.Redis.Del(ctx, lockKey).Err(); err != nil {
			return nil, err
		}
		c, err := acquire(ctx, e, "C", ttl)
		if err != nil {
			return nil, err
		}
		out = append(out, UnlockResult{name, c})
	}
	return out, nil
}

// CronResult: a job scheduled on every instance, which must run once per
// scheduled time.
type CronResult struct {
	Strategy  string
	Runs      int
	RanTwice  int // scheduled times where the job body ran more than once
	Skipped   int // scheduled times where nobody ran it
	PauseHits int // how many job executions were paused past the lease
}

type CronParams struct {
	Runs, Instances int
	Skew            time.Duration // instances start up to this late (clock skew, scheduler jitter)
	Work, TTL       time.Duration
	Pause           time.Duration // a pause longer than the TTL...
	PauseProb       float64       // ...hitting this fraction of job executions
}

// Cron runs the job Runs times on Instances instances per strategy. Every
// strategy also checks "already done for this run?" before working, as real
// jobs do; the question is whether that check is safe.
func Cron(ctx context.Context, e *Env, p CronParams) ([]CronResult, error) {
	type strategy struct {
		name string
		// try runs the job for runID if this instance gets to; body is the job.
		try func(runID, inst int, body func(q querier) error) error
	}
	done := func(q querier, runID int) (bool, error) {
		var d bool
		err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM dlock.runs WHERE run_id = $1)`, runID).Scan(&d)
		return d, err
	}
	strategies := []strategy{
		{"Redis lock (SET NX PX, safe release) + done check", func(runID, inst int, body func(q querier) error) error {
			owner := fmt.Sprintf("%d/%d", runID, inst)
			key := fmt.Sprintf("dlock:cron:%d", runID)
			ok, err := e.Redis.SetNX(ctx, key, owner, p.TTL).Result()
			if err != nil || !ok {
				return err
			}
			defer unlockScript.Run(ctx, e.Redis, []string{key}, owner)
			if d, err := done(e.DB, runID); err != nil || d {
				return err
			}
			return body(e.DB)
		}},
		{"PostgreSQL advisory lock + done check, in one transaction", func(runID, inst int, body func(q querier) error) error {
			return pgx.BeginFunc(ctx, e.DB, func(tx pgx.Tx) error {
				var ok bool
				if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(1000, $1)`, runID).Scan(&ok); err != nil || !ok {
					return err
				}
				if d, err := done(tx, runID); err != nil || d {
					return err
				}
				return body(tx)
			})
		}},
		{"Claim a row: INSERT ... ON CONFLICT DO NOTHING", func(runID, inst int, body func(q querier) error) error {
			tag, err := e.DB.Exec(ctx, `INSERT INTO dlock.claims VALUES ($1) ON CONFLICT DO NOTHING`, runID)
			if err != nil || tag.RowsAffected() == 0 {
				return err
			}
			return body(e.DB)
		}},
	}
	var out []CronResult
	for _, s := range strategies {
		if err := reset(ctx, e); err != nil {
			return nil, err
		}
		var pauses atomic.Int64
		ran := make([]atomic.Int64, p.Runs)
		var firstErr atomic.Value
		for run := range p.Runs {
			var wg sync.WaitGroup
			for inst := range p.Instances {
				wg.Add(1)
				go func() {
					defer wg.Done()
					time.Sleep(time.Duration(rand.Int64N(int64(p.Skew))))
					err := s.try(run, inst, func(q querier) error {
						ran[run].Add(1) // the side effect: an email, a charge, a report
						time.Sleep(p.Work)
						if rand.Float64() < p.PauseProb {
							pauses.Add(1)
							time.Sleep(p.Pause)
						}
						_, err := q.Exec(ctx, `INSERT INTO dlock.runs VALUES ($1, $2)`, run, inst)
						return err
					})
					if err != nil {
						firstErr.CompareAndSwap(nil, err)
					}
				}()
			}
			wg.Wait()
			if err, _ := firstErr.Load().(error); err != nil {
				return nil, fmt.Errorf("%s: %w", s.name, err)
			}
		}
		r := CronResult{Strategy: s.name, Runs: p.Runs, PauseHits: int(pauses.Load())}
		for i := range ran {
			switch n := ran[i].Load(); {
			case n > 1:
				r.RanTwice++
			case n == 0:
				r.Skipped++
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// querier is what pgxpool.Pool and pgx.Tx have in common here.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}
