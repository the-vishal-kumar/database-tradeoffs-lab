// Package cache measures how a Redis cache in front of PostgreSQL serves
// wrong data, and how it can flatten the database when a hot key expires.
//
// The "database" is one product row: price and version. Every scenario
// counts the queries that reach Postgres and checks what readers were given.
// Races are reproduced deterministically by running the steps of the reader
// and the writer in a fixed order, not by hoping for unlucky timing.
package cache

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// Env is a database, a cache, and a counter of queries that reached the database.
type Env struct {
	DB      *pgxpool.Pool
	Redis   *redis.Client
	queries atomic.Int64
}

// Item is what the database holds and the cache copies.
type Item struct {
	Price   int
	Version int
}

func (it Item) encode() string { return fmt.Sprintf("%d:%d", it.Price, it.Version) }

func decode(s string) (Item, error) {
	p, v, ok := strings.Cut(s, ":")
	if !ok {
		return Item{}, fmt.Errorf("bad cache value %q", s)
	}
	price, err1 := strconv.Atoi(p)
	ver, err2 := strconv.Atoi(v)
	return Item{price, ver}, errors.Join(err1, err2)
}

const key = "product:1"

// reset puts price 100, version 1 in the database and empties the cache.
func (e *Env) reset(ctx context.Context) error {
	e.queries.Store(0)
	if _, err := e.DB.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS cachedemo;
		DROP TABLE IF EXISTS cachedemo.products;
		CREATE TABLE cachedemo.products (id int PRIMARY KEY, price int NOT NULL, version int NOT NULL);
		INSERT INTO cachedemo.products VALUES (1, 100, 1);`); err != nil {
		return err
	}
	return e.Redis.FlushDB(ctx).Err()
}

// load reads the product from the database, taking at least delay (a slow
// query), and counts it.
func (e *Env) load(ctx context.Context, delay time.Duration) (Item, error) {
	e.queries.Add(1)
	var it Item
	err := e.DB.QueryRow(ctx, `SELECT price, version FROM cachedemo.products, pg_sleep($1) WHERE id = 1`,
		delay.Seconds()).Scan(&it.Price, &it.Version)
	return it, err
}

// update changes the price and bumps the version, as one committed write.
func (e *Env) update(ctx context.Context, price int) (Item, error) {
	var it Item
	err := e.DB.QueryRow(ctx, `UPDATE cachedemo.products SET price = $1, version = version + 1
		WHERE id = 1 RETURNING price, version`, price).Scan(&it.Price, &it.Version)
	return it, err
}

func (e *Env) cached(ctx context.Context) (Item, bool, error) {
	s, err := e.Redis.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, err
	}
	it, err := decode(s)
	return it, err == nil, err
}

// readThrough is cache-aside: the cache, or the database and then fill the cache.
func (e *Env) readThrough(ctx context.Context, ttl time.Duration) (Item, error) {
	if it, ok, err := e.cached(ctx); err != nil || ok {
		return it, err
	}
	it, err := e.load(ctx, 0)
	if err != nil {
		return it, err
	}
	return it, e.Redis.Set(ctx, key, it.encode(), ttl).Err()
}

// setIfNewer writes the cache only if it holds nothing or an older version.
// A Lua script runs atomically inside Redis.
var setIfNewer = redis.NewScript(`
local cur = redis.call('GET', KEYS[1])
if cur then
  local v = tonumber(string.match(cur, ':(%d+)$'))
  if v and v >= tonumber(ARGV[2]) then return 0 end
end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[3])
return 1`)

func (e *Env) setVersioned(ctx context.Context, it Item, ttl time.Duration) error {
	return setIfNewer.Run(ctx, e.Redis, []string{key}, it.encode(), it.Version, ttl.Milliseconds()).Err()
}

// ---- 1. TTL only vs invalidate on write ----

// StaleWindow is how long readers kept getting the old price after a write.
type StaleWindow struct {
	Strategy   string
	StaleReads int           // reads (every 50 ms) that returned the old price
	StaleFor   time.Duration // from the write until the first fresh read
}

// Staleness updates the price and polls through the cache every 50 ms.
func Staleness(ctx context.Context, e *Env, ttl time.Duration) ([]StaleWindow, error) {
	var out []StaleWindow
	for _, strategy := range []string{"TTL only (no invalidation)", "delete the key after the write"} {
		if err := e.reset(ctx); err != nil {
			return nil, err
		}
		if _, err := e.readThrough(ctx, ttl); err != nil {
			return nil, err
		}
		if _, err := e.update(ctx, 200); err != nil {
			return nil, err
		}
		if strings.HasPrefix(strategy, "delete") {
			if err := e.Redis.Del(ctx, key).Err(); err != nil {
				return nil, err
			}
		}
		start := time.Now()
		w := StaleWindow{Strategy: strategy}
		for time.Since(start) < ttl+time.Second {
			it, err := e.readThrough(ctx, ttl)
			if err != nil {
				return nil, err
			}
			if it.Price == 200 {
				w.StaleFor = time.Since(start).Round(100 * time.Millisecond)
				break
			}
			w.StaleReads++
			time.Sleep(50 * time.Millisecond)
		}
		out = append(out, w)
	}
	return out, nil
}

// ---- 2. the delete-on-write race ----

// RaceResult is what the cache holds, and for how long, after the race.
type RaceResult struct {
	Strategy string
	Cached   int // price in the cache afterwards (0 = empty)
	StaleFor string
	DBPrice  int
}

// Race replays the classic interleaving: a reader misses and loads the old
// row; a writer updates the row and invalidates; then the reader writes what
// it loaded into the cache.
func Race(ctx context.Context, e *Env, ttl, doubleDeleteDelay time.Duration) ([]RaceResult, error) {
	var out []RaceResult
	type writerFn func(ctx context.Context, it Item) error
	strategies := []struct {
		name   string
		writer writerFn
		reader func(ctx context.Context, it Item) error
	}{
		{"delete after the write", func(ctx context.Context, _ Item) error { return e.Redis.Del(ctx, key).Err() },
			func(ctx context.Context, it Item) error { return e.Redis.Set(ctx, key, it.encode(), ttl).Err() }},
		{fmt.Sprintf("delete, then delete again after %s", doubleDeleteDelay), func(ctx context.Context, _ Item) error {
			if err := e.Redis.Del(ctx, key).Err(); err != nil {
				return err
			}
			go func() { time.Sleep(doubleDeleteDelay); e.Redis.Del(context.Background(), key) }()
			return nil
		}, func(ctx context.Context, it Item) error { return e.Redis.Set(ctx, key, it.encode(), ttl).Err() }},
		{"versioned: writer sets the new version, cache keeps the newest", func(ctx context.Context, it Item) error { return e.setVersioned(ctx, it, ttl) },
			func(ctx context.Context, it Item) error { return e.setVersioned(ctx, it, ttl) }},
	}
	for _, s := range strategies {
		if err := e.reset(ctx); err != nil {
			return nil, err
		}
		// Reader: miss, load the old row.
		if _, ok, err := e.cached(ctx); err != nil || ok {
			return nil, fmt.Errorf("expected a miss: %v", err)
		}
		old, err := e.load(ctx, 0)
		if err != nil {
			return nil, err
		}
		// Writer: update, then invalidate (or write through).
		fresh, err := e.update(ctx, 200)
		if err != nil {
			return nil, err
		}
		if err := s.writer(ctx, fresh); err != nil {
			return nil, err
		}
		// Reader, a moment late: fill the cache with what it loaded.
		if err := s.reader(ctx, old); err != nil {
			return nil, err
		}
		r := RaceResult{Strategy: s.name, DBPrice: fresh.Price}
		start := time.Now()
		for {
			it, ok, err := e.cached(ctx)
			if err != nil {
				return nil, err
			}
			if !ok || it.Price == fresh.Price {
				if ok {
					r.Cached = it.Price
				}
				if time.Since(start) < 20*time.Millisecond {
					r.StaleFor = "never stale"
				} else {
					r.StaleFor = time.Since(start).Round(100 * time.Millisecond).String()
				}
				break
			}
			if time.Since(start) > 3*time.Second {
				r.Cached = it.Price
				r.StaleFor = fmt.Sprintf("until the TTL (%s) expires", ttl)
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		out = append(out, r)
	}
	return out, nil
}

// ---- 3. cache written before the database commits ----

// Rollback reports what readers saw after a write that set the cache inside
// its transaction and then rolled back.
type RollbackResult struct {
	Strategy string
	Served   int // price a reader got from the cache path
	DBPrice  int
}

func Rollback(ctx context.Context, e *Env, ttl time.Duration) ([]RollbackResult, error) {
	var out []RollbackResult
	for _, afterCommit := range []bool{false, true} {
		if err := e.reset(ctx); err != nil {
			return nil, err
		}
		tx, err := e.DB.Begin(ctx)
		if err != nil {
			return nil, err
		}
		var it Item
		if err := tx.QueryRow(ctx, `UPDATE cachedemo.products SET price = 300, version = version + 1
			WHERE id = 1 RETURNING price, version`).Scan(&it.Price, &it.Version); err != nil {
			return nil, err
		}
		if !afterCommit {
			if err := e.Redis.Set(ctx, key, it.encode(), ttl).Err(); err != nil {
				return nil, err
			}
		}
		// Something later in the request fails: the transaction rolls back,
		// and a cache update meant to follow the commit never runs.
		if err := tx.Rollback(ctx); err != nil {
			return nil, err
		}
		served, err := e.readThrough(ctx, ttl)
		if err != nil {
			return nil, err
		}
		db, err := e.load(ctx, 0)
		if err != nil {
			return nil, err
		}
		name := "set the cache inside the transaction"
		if afterCommit {
			name = "touch the cache only after commit"
		}
		out = append(out, RollbackResult{name, served.Price, db.Price})
	}
	return out, nil
}

// ---- 4. stampede ----

// StampedeResult is one strategy under N concurrent readers of an expired key.
type StampedeResult struct {
	Strategy      string
	Readers       int
	DBQueries     int64
	P50, P99, Max time.Duration
	ServedStale   int
}

// Stampede: instances × perInstance readers ask for the same key at once,
// right after it expired. Loading it costs queryTime in the database.
func Stampede(ctx context.Context, e *Env, instances, perInstance int, queryTime, ttl time.Duration) ([]StampedeResult, error) {
	type strategy struct {
		name string
		// get returns the item and whether it was a stale copy. inst is the
		// app instance the reader runs in (singleflight is per process).
		get func(ctx context.Context, inst int) (Item, bool, error)
		// prime prepares the cache state "just after expiry".
		prime func(ctx context.Context) error
	}
	empty := func(ctx context.Context) error { return e.Redis.Del(ctx, key).Err() }
	loadAndSet := func(ctx context.Context) (Item, error) {
		it, err := e.load(ctx, queryTime)
		if err != nil {
			return it, err
		}
		return it, e.Redis.Set(ctx, key, it.encode(), ttl).Err()
	}
	groups := make([]singleflight.Group, instances)

	strategies := []strategy{
		{"naive cache-aside", func(ctx context.Context, _ int) (Item, bool, error) {
			if it, ok, err := e.cached(ctx); err != nil || ok {
				return it, false, err
			}
			it, err := loadAndSet(ctx)
			return it, false, err
		}, empty},
		{fmt.Sprintf("singleflight in each of %d app instances", instances), func(ctx context.Context, inst int) (Item, bool, error) {
			if it, ok, err := e.cached(ctx); err != nil || ok {
				return it, false, err
			}
			v, err, _ := groups[inst].Do(key, func() (any, error) { return loadAndSet(ctx) })
			if err != nil {
				return Item{}, false, err
			}
			return v.(Item), false, nil
		}, empty},
		{"Redis lock (SET NX): one loader, the rest wait for the cache", func(ctx context.Context, _ int) (Item, bool, error) {
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if it, ok, err := e.cached(ctx); err != nil || ok {
					return it, false, err
				}
				got, err := e.Redis.SetNX(ctx, key+":lock", "1", 5*time.Second).Result()
				if err != nil {
					return Item{}, false, err
				}
				if got {
					// Check again: another loader may have filled the cache
					// and released the lock between our miss and our SET NX.
					if it, ok, err := e.cached(ctx); err != nil || ok {
						e.Redis.Del(ctx, key+":lock")
						return it, false, err
					}
					it, err := loadAndSet(ctx)
					e.Redis.Del(ctx, key+":lock")
					return it, false, err
				}
				time.Sleep(10 * time.Millisecond)
			}
			return Item{}, false, errors.New("timed out waiting for the cache")
		}, empty},
		{"stale-while-revalidate: serve the old copy, one refresher", func(ctx context.Context, _ int) (Item, bool, error) {
			// The key holds "value|soft-expiry"; its Redis TTL is much longer.
			s, err := e.Redis.Get(ctx, key+":swr").Result()
			if errors.Is(err, redis.Nil) {
				it, err := loadAndSet(ctx)
				return it, false, err
			}
			if err != nil {
				return Item{}, false, err
			}
			val, soft, _ := strings.Cut(s, "|")
			it, err := decode(val)
			if err != nil {
				return it, false, err
			}
			exp, _ := strconv.ParseInt(soft, 10, 64)
			if time.Now().UnixMilli() < exp {
				return it, false, nil
			}
			// Soft-expired: one reader refreshes in the background; everyone,
			// including that reader, gets the old copy now.
			if got, _ := e.Redis.SetNX(ctx, key+":swr:lock", "1", 5*time.Second).Result(); got {
				go func() {
					bg := context.Background()
					fresh, err := e.load(bg, queryTime)
					if err == nil {
						e.Redis.Set(bg, key+":swr", fresh.encode()+"|"+strconv.FormatInt(time.Now().Add(ttl).UnixMilli(), 10), 10*ttl)
					}
					e.Redis.Del(bg, key+":swr:lock")
				}()
			}
			return it, true, nil
		}, func(ctx context.Context) error {
			// A copy whose soft expiry has just passed.
			return e.Redis.Set(ctx, key+":swr", Item{100, 1}.encode()+"|"+strconv.FormatInt(time.Now().Add(-time.Millisecond).UnixMilli(), 10), 10*ttl).Err()
		}},
	}

	var out []StampedeResult
	for _, s := range strategies {
		if err := e.reset(ctx); err != nil {
			return nil, err
		}
		if err := s.prime(ctx); err != nil {
			return nil, err
		}
		e.queries.Store(0)
		n := instances * perInstance
		lat := make([]time.Duration, n)
		stale := atomic.Int64{}
		var wg sync.WaitGroup
		var firstErr atomic.Value
		startGun := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-startGun
				t := time.Now()
				_, wasStale, err := s.get(ctx, i%instances)
				lat[i] = time.Since(t)
				if err != nil {
					firstErr.CompareAndSwap(nil, err)
				}
				if wasStale {
					stale.Add(1)
				}
			}(i)
		}
		close(startGun)
		wg.Wait()
		if err, _ := firstErr.Load().(error); err != nil {
			return nil, err
		}
		time.Sleep(queryTime + 100*time.Millisecond) // let a background refresh finish and be counted
		slices.Sort(lat)
		out = append(out, StampedeResult{
			Strategy: s.name, Readers: n, DBQueries: e.queries.Load(),
			P50: lat[n/2], P99: lat[n*99/100], Max: lat[n-1], ServedStale: int(stale.Load()),
		})
	}
	return out, nil
}

// ---- 5. TTL jitter ----

// JitterResult is how database load was spread when many keys expire.
type JitterResult struct {
	Strategy     string
	Keys         int
	PeakPer100ms int64
	TotalQueries int64
}

// Jitter caches `keys` items at the same moment with the same TTL (or a TTL
// with ±spread random jitter) and has readers request them for 3×TTL.
func Jitter(ctx context.Context, e *Env, keys int, ttl time.Duration, spread float64) ([]JitterResult, error) {
	var out []JitterResult
	for _, jitter := range []bool{false, true} {
		if err := e.reset(ctx); err != nil {
			return nil, err
		}
		ttlFor := func() time.Duration {
			if !jitter {
				return ttl
			}
			return time.Duration(float64(ttl) * (1 - spread + 2*spread*rand.Float64()))
		}
		var buckets sync.Map // 100 ms slot -> *atomic.Int64
		start := time.Now()
		count := func() {
			slot := time.Since(start) / (100 * time.Millisecond)
			v, _ := buckets.LoadOrStore(slot, new(atomic.Int64))
			v.(*atomic.Int64).Add(1)
		}
		get := func(ctx context.Context, k int) error {
			name := fmt.Sprintf("jitter:%d", k)
			if err := e.Redis.Get(ctx, name).Err(); err == nil {
				return nil
			} else if !errors.Is(err, redis.Nil) {
				return err
			}
			count()
			if _, err := e.load(ctx, 0); err != nil {
				return err
			}
			return e.Redis.Set(ctx, name, "1", ttlFor()).Err()
		}
		// Warm every key at once, as after a deploy or a cache flush: one
		// pipeline, so the keys really are set within milliseconds of each
		// other (one round trip per key would spread them over the warm-up
		// itself, which on a busy machine blurs the effect being measured).
		pipe := e.Redis.Pipeline()
		for k := range keys {
			pipe.Set(ctx, fmt.Sprintf("jitter:%d", k), "1", ttlFor())
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return nil, err
		}
		var wg sync.WaitGroup
		var firstErr atomic.Value
		for w := range 16 {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for k := w; time.Since(start) < 3*ttl; k = (k + 16) % keys {
					if err := get(ctx, k); err != nil {
						firstErr.CompareAndSwap(nil, err)
						return
					}
				}
			}(w)
		}
		wg.Wait()
		if err, _ := firstErr.Load().(error); err != nil {
			return nil, err
		}
		r := JitterResult{Strategy: "same TTL for every key", Keys: keys}
		if jitter {
			r.Strategy = fmt.Sprintf("TTL ±%d%% random jitter", int(spread*100))
		}
		buckets.Range(func(_, v any) bool {
			n := v.(*atomic.Int64).Load()
			r.TotalQueries += n
			r.PeakPer100ms = max(r.PeakPer100ms, n)
			return true
		})
		out = append(out, r)
	}
	return out, nil
}

// PoolSize is the database connection pool size, like a typical app instance.
const PoolSize = 50

// Connect opens the database pool and the Redis client.
func Connect(ctx context.Context, dbURL, redisURL string) (*Env, func(), error) {
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return nil, nil, err
	}
	cfg.MaxConns = PoolSize
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	opt.PoolSize = 256
	rdb := redis.NewClient(opt)
	if err := rdb.Ping(ctx).Err(); err != nil {
		pool.Close()
		return nil, nil, err
	}
	e := &Env{DB: pool, Redis: rdb}
	return e, func() {
		_, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS cachedemo CASCADE`)
		pool.Close()
		_ = rdb.Close()
	}, nil
}
