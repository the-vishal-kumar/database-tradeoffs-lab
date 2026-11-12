// Package locking measures pessimistic vs optimistic concurrency control in
// PostgreSQL: lock the row with SELECT ... FOR UPDATE, or read freely and
// retry when a version check fails. Also measured: a single atomic UPDATE,
// SERIALIZABLE with retries, and a naive read-modify-write as the baseline
// that loses updates.
//
// The workload is "buy one": read the stock, do some application work (a
// fixed delay, standing in for a price calculation or a call to another
// service), then write stock - 1. Contention is high when every worker
// buys the same item, low when they spread over many items.
package locking

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

// Strategy is one way to do "read, work, write".
type Strategy struct {
	Name string
	// buy decrements the stock of item id by one if it's above zero,
	// calling work where the application would do its work between the
	// read and the write. It returns whether it sold, and how many retries
	// it needed.
	buy func(ctx context.Context, db *pgxpool.Pool, id int, work func()) (sold bool, retries int, err error)
}

const maxRetries = 100

// ErrGaveUp: a retrying strategy hit maxRetries. It's counted, not fatal:
// under heavy contention, giving up is one of the outcomes being measured.
var ErrGaveUp = errors.New("gave up after too many conflicts")

// PoolSize is the connection pool; larger than any worker count used, so
// workers wait on the database, not on the pool.
const PoolSize = 64

// Connect opens a pool sized for the benchmark.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = PoolSize
	return pgxpool.NewWithConfig(ctx, cfg)
}

func isRetryable(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && (pe.Code == "40001" || pe.Code == "40P01")
}

// backoff sleeps a little, with jitter, so retrying workers don't collide again.
func backoff(attempt int) {
	d := time.Duration(1<<min(attempt, 6)) * 100 * time.Microsecond
	time.Sleep(d/2 + time.Duration(rand.Int64N(int64(d))))
}

// Strategies in report order.
var Strategies = []Strategy{
	{"naive read-modify-write (no lock, no check)", func(ctx context.Context, db *pgxpool.Pool, id int, work func()) (bool, int, error) {
		var stock int
		if err := db.QueryRow(ctx, `SELECT stock FROM locking.items WHERE id = $1`, id).Scan(&stock); err != nil {
			return false, 0, err
		}
		work()
		if stock <= 0 {
			return false, 0, nil
		}
		_, err := db.Exec(ctx, `UPDATE locking.items SET stock = $2 WHERE id = $1`, id, stock-1)
		return err == nil, 0, err
	}},
	{"pessimistic: SELECT ... FOR UPDATE, work, UPDATE", func(ctx context.Context, db *pgxpool.Pool, id int, work func()) (bool, int, error) {
		sold := false
		err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
			var stock int
			if err := tx.QueryRow(ctx, `SELECT stock FROM locking.items WHERE id = $1 FOR UPDATE`, id).Scan(&stock); err != nil {
				return err
			}
			work() // the row stays locked during the work
			if stock <= 0 {
				return nil
			}
			_, err := tx.Exec(ctx, `UPDATE locking.items SET stock = stock - 1 WHERE id = $1`, id)
			sold = err == nil
			return err
		})
		return sold, 0, err
	}},
	{"optimistic: read version, work, UPDATE ... WHERE version = read, retry", func(ctx context.Context, db *pgxpool.Pool, id int, work func()) (bool, int, error) {
		for attempt := 0; attempt < maxRetries; attempt++ {
			var stock, version int
			if err := db.QueryRow(ctx, `SELECT stock, version FROM locking.items WHERE id = $1`, id).Scan(&stock, &version); err != nil {
				return false, attempt, err
			}
			work() // no lock held
			if stock <= 0 {
				return false, attempt, nil
			}
			tag, err := db.Exec(ctx, `UPDATE locking.items SET stock = $3, version = version + 1
				WHERE id = $1 AND version = $2`, id, version, stock-1)
			if err != nil {
				return false, attempt, err
			}
			if tag.RowsAffected() == 1 {
				return true, attempt, nil
			}
			backoff(attempt) // someone else wrote first: read again
		}
		return false, maxRetries, ErrGaveUp
	}},
	{"atomic: work, then UPDATE ... SET stock = stock - 1 WHERE stock > 0", func(ctx context.Context, db *pgxpool.Pool, id int, work func()) (bool, int, error) {
		work() // whatever the app needs to do first, outside any lock
		tag, err := db.Exec(ctx, `UPDATE locking.items SET stock = stock - 1 WHERE id = $1 AND stock > 0`, id)
		return err == nil && tag.RowsAffected() == 1, 0, err
	}},
	{"SERIALIZABLE: read, work, write, retry on 40001", func(ctx context.Context, db *pgxpool.Pool, id int, work func()) (bool, int, error) {
		for attempt := 0; attempt < maxRetries; attempt++ {
			sold := false
			err := pgx.BeginTxFunc(ctx, db, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
				var stock int
				if err := tx.QueryRow(ctx, `SELECT stock FROM locking.items WHERE id = $1`, id).Scan(&stock); err != nil {
					return err
				}
				work()
				if stock <= 0 {
					return nil
				}
				_, err := tx.Exec(ctx, `UPDATE locking.items SET stock = $2 WHERE id = $1`, id, stock-1)
				sold = err == nil
				return err
			})
			if err == nil {
				return sold, attempt, nil
			}
			if !isRetryable(err) {
				return false, attempt, err
			}
			backoff(attempt)
		}
		return false, maxRetries, ErrGaveUp
	}},
}

// Result is one strategy at one contention level.
type Result struct {
	Strategy, Contention string
	Ops                  int
	Sold                 int64 // purchases the strategy reported as successful
	Decremented          int64 // how much the stock actually went down
	LostUpdates          int64 // Sold - Decremented: sales the database forgot
	Retries              int64
	GaveUp               int64   // purchases abandoned after maxRetries
	PerSec               float64 // completed purchases per second
	P50, P99             time.Duration
	// MaxConcurrentWork is the most buyers that were doing their
	// application work at the same moment. A strategy that holds the row
	// lock through the work keeps it at 1 on a hot row, however fast the
	// machine is; that serialisation is what costs it throughput.
	MaxConcurrentWork int64
}

// workTracker sleeps for the application work and records how many
// buyers were inside it at once.
type workTracker struct {
	d         time.Duration
	now, peak atomic.Int64
}

func (w *workTracker) do() {
	n := w.now.Add(1)
	for p := w.peak.Load(); n > p && !w.peak.CompareAndSwap(p, n); p = w.peak.Load() {
	}
	time.Sleep(w.d)
	w.now.Add(-1)
}

// setup creates `items` rows, each with `stock` units.
func setup(ctx context.Context, db *pgxpool.Pool, items, stock int) error {
	_, err := db.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS locking;
		DROP TABLE IF EXISTS locking.items;
		CREATE TABLE locking.items (id int PRIMARY KEY, stock int NOT NULL, version int NOT NULL DEFAULT 0);
		INSERT INTO locking.items (id, stock) SELECT g, %d FROM generate_series(1, %d) g;`, stock, items))
	return err
}

func totalStock(ctx context.Context, db *pgxpool.Pool) (int64, error) {
	var n int64
	err := db.QueryRow(ctx, `SELECT sum(stock) FROM locking.items`).Scan(&n)
	return n, err
}

// Run: `workers` goroutines each buy `perWorker` times, from one item (high
// contention) or a random one of `items` (low contention).
func Run(ctx context.Context, db *pgxpool.Pool, s Strategy, workers, perWorker, items int, work time.Duration) (Result, error) {
	const startStock = 1_000_000
	if err := setup(ctx, db, items, startStock); err != nil {
		return Result{}, err
	}
	contention := "high (1 item)"
	if items > 1 {
		contention = fmt.Sprintf("low (%d items)", items)
	}
	r := Result{Strategy: s.Name, Contention: contention, Ops: workers * perWorker}
	var sold, retries, gaveUp atomic.Int64
	wt := &workTracker{d: work}
	lat := make([]time.Duration, 0, r.Ops)
	var mu sync.Mutex
	var firstErr atomic.Value
	var wg sync.WaitGroup
	start := time.Now()
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mine := make([]time.Duration, 0, perWorker)
			for range perWorker {
				id := 1
				if items > 1 {
					id = 1 + rand.IntN(items)
				}
				t := time.Now()
				ok, n, err := s.buy(ctx, db, id, wt.do)
				mine = append(mine, time.Since(t))
				retries.Add(int64(n))
				if errors.Is(err, ErrGaveUp) {
					gaveUp.Add(1)
					continue
				}
				if err != nil {
					firstErr.CompareAndSwap(nil, err)
					return
				}
				if ok {
					sold.Add(1)
				}
			}
			mu.Lock()
			lat = append(lat, mine...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	if err, _ := firstErr.Load().(error); err != nil {
		return r, err
	}
	left, err := totalStock(ctx, db)
	if err != nil {
		return r, err
	}
	r.Sold, r.Retries, r.GaveUp = sold.Load(), retries.Load(), gaveUp.Load()
	r.Decremented = int64(startStock*items) - left
	r.LostUpdates = r.Sold - r.Decremented
	r.PerSec = float64(r.Sold) / elapsed.Seconds()
	r.MaxConcurrentWork = wt.peak.Load()
	slices.Sort(lat)
	r.P50, r.P99 = lat[len(lat)/2], lat[len(lat)*99/100]
	return r, nil
}

// Oversell: `buyers` try to buy one each of an item with `stock` units.
type OversellResult struct {
	Strategy       string
	Stock, Buyers  int
	SalesConfirmed int64 // buyers told "you got one"
	StockLeft      int
	GaveUp         int64
}

func Oversell(ctx context.Context, db *pgxpool.Pool, s Strategy, stock, buyers int, work time.Duration) (OversellResult, error) {
	if err := setup(ctx, db, 1, stock); err != nil {
		return OversellResult{}, err
	}
	var sold, gaveUp atomic.Int64
	wt := &workTracker{d: work}
	var firstErr atomic.Value
	var wg sync.WaitGroup
	gun := make(chan struct{})
	for range buyers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gun
			ok, _, err := s.buy(ctx, db, 1, wt.do)
			if errors.Is(err, ErrGaveUp) {
				gaveUp.Add(1)
			} else if err != nil {
				firstErr.CompareAndSwap(nil, err)
			}
			if ok {
				sold.Add(1)
			}
		}()
	}
	close(gun)
	wg.Wait()
	if err, _ := firstErr.Load().(error); err != nil {
		return OversellResult{}, err
	}
	var left int
	if err := db.QueryRow(ctx, `SELECT stock FROM locking.items WHERE id = 1`).Scan(&left); err != nil {
		return OversellResult{}, err
	}
	return OversellResult{s.Name, stock, buyers, sold.Load(), left, gaveUp.Load()}, nil
}
