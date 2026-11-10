// Command cachedemo measures how a Redis cache in front of PostgreSQL serves
// wrong data, and what happens to the database when a hot key expires.
//
//	go run ./cmd/cachedemo
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/the-vishal-kumar/database-tradeoffs-lab/internal/cache"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func run() error {
	ctx := context.Background()
	e, cleanup, err := cache.Connect(ctx,
		env("DATABASE_URL", "postgres://lab:lab@localhost:55432/lab?sslmode=disable"),
		env("REDIS_URL", "redis://localhost:56379/0"))
	if err != nil {
		return err
	}
	defer cleanup()

	var pgVersion string
	_ = e.DB.QueryRow(ctx, `SHOW server_version`).Scan(&pgVersion)
	info, _ := e.Redis.InfoMap(ctx, "server").Result()
	fmt.Printf("PostgreSQL %s, Redis %s, go-redis %s, a pool of %d database connections.\n\n",
		pgVersion, info["Server"]["redis_version"], redis.Version(), cache.PoolSize)

	stale, err := cache.Staleness(ctx, e, 2*time.Second)
	if err != nil {
		return err
	}
	fmt.Println("### 1. After a write: TTL only vs delete the key (TTL 2 s, a read every 50 ms)\n\n| Strategy | Stale reads | Stale for |\n|---|---:|---:|")
	for _, w := range stale {
		fmt.Printf("| %s | %d | %s |\n", w.Strategy, w.StaleReads, w.StaleFor)
	}

	race, err := cache.Race(ctx, e, time.Minute, 500*time.Millisecond)
	if err != nil {
		return err
	}
	fmt.Println("\n### 2. The race: reader loads the old row, writer updates and invalidates, reader fills the cache (TTL 60 s)\n\n| Strategy | Database | Cache afterwards | Stale for |\n|---|---:|---:|---|")
	for _, r := range race {
		c := "empty"
		if r.Cached != 0 {
			c = fmt.Sprint(r.Cached)
		}
		fmt.Printf("| %s | %d | %s | %s |\n", r.Strategy, r.DBPrice, c, r.StaleFor)
	}

	rb, err := cache.Rollback(ctx, e, time.Minute)
	if err != nil {
		return err
	}
	fmt.Println("\n### 3. A write that rolls back\n\n| Strategy | Readers got | Database has |\n|---|---:|---:|")
	for _, r := range rb {
		fmt.Printf("| %s | %d | %d |\n", r.Strategy, r.Served, r.DBPrice)
	}

	st, err := cache.Stampede(ctx, e, 4, 50, 50*time.Millisecond, 10*time.Second)
	if err != nil {
		return err
	}
	fmt.Println("\n### 4. Stampede: 200 readers (4 instances × 50) hit a key that just expired; loading it takes 50 ms\n\n| Strategy | Database queries | p50 | p99 | Slowest | Served the old copy |\n|---|---:|---:|---:|---:|---:|")
	for _, r := range st {
		fmt.Printf("| %s | %d | %s | %s | %s | %d |\n", r.Strategy, r.DBQueries, ms(r.P50), ms(r.P99), ms(r.Max), r.ServedStale)
	}

	jt, err := cache.Jitter(ctx, e, 2000, time.Second, 0.3)
	if err != nil {
		return err
	}
	fmt.Println("\n### 5. 2,000 keys cached at once with a 1 s TTL, read continuously for 3 s\n\n| Strategy | Peak database queries in any 100 ms | Total queries |\n|---|---:|---:|")
	for _, r := range jt {
		fmt.Printf("| %s | %d | %d |\n", r.Strategy, r.PeakPer100ms, r.TotalQueries)
	}
	return nil
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000) }
