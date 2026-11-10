// Command dlockdemo shows why a Redis lock with a TTL doesn't make a job
// run once, and what does.
//
//	go run ./cmd/dlockdemo
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/the-vishal-kumar/database-tradeoffs-lab/internal/dlock"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	runs := flag.Int("runs", 300, "scheduled runs in the cron scenario")
	prob := flag.Float64("pause", 0.1, "fraction of job executions hit by a pause longer than the lease")
	flag.Parse()
	if err := run(*runs, *prob); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(runs int, prob float64) error {
	ctx := context.Background()
	e, cleanup, err := dlock.Connect(ctx,
		env("DATABASE_URL", "postgres://lab:lab@localhost:55432/lab?sslmode=disable"),
		env("REDIS_URL", "redis://localhost:56379/0"))
	if err != nil {
		return err
	}
	defer cleanup()
	var pg string
	_ = e.DB.QueryRow(ctx, `SHOW server_version`).Scan(&pg)
	info, _ := e.Redis.InfoMap(ctx, "server").Result()
	fmt.Printf("PostgreSQL %s, Redis %s, go-redis %s.\n\n", pg, info["Server"]["redis_version"], redis.Version())

	const ttl = 200 * time.Millisecond
	pr, err := dlock.Paused(ctx, e, ttl)
	if err != nil {
		return err
	}
	fmt.Printf("### 1. A takes the lock (lease %s), pauses for %s; B takes the lock and writes; A wakes up and writes\n\n", ttl, ttl+50*time.Millisecond)
	fmt.Println("| Strategy | B got the lock while A thought it held it | A's write after the pause | Final value |\n|---|---|---|---|")
	for _, r := range pr {
		fmt.Printf("| %s | %s | %s | %s |\n", r.Strategy, yes(r.BothHeld), r.LateWrite, r.Final)
	}

	ur, err := dlock.Unlock(ctx, e, ttl)
	if err != nil {
		return err
	}
	fmt.Println("\n### 2. A's lease expires, B takes the lock, then A releases \"its\" lock\n\n| Release | C can take the lock while B is still working |\n|---|---|")
	for _, r := range ur {
		fmt.Printf("| %s | %s |\n", r.Strategy, yes(r.CAcquired))
	}

	p := dlock.CronParams{Runs: runs, Instances: 3, Skew: 50 * time.Millisecond, Work: 5 * time.Millisecond,
		TTL: 30 * time.Millisecond, Pause: 60 * time.Millisecond, PauseProb: prob}
	cr, err := dlock.Cron(ctx, e, p)
	if err != nil {
		return err
	}
	fmt.Printf("\n### 3. A job scheduled on %d instances, %d scheduled runs\n\nInstances start up to %s apart; the job takes %s; %.0f%% of executions pause for %s; Redis lease %s.\n\n",
		p.Instances, p.Runs, p.Skew, p.Work, p.PauseProb*100, p.Pause, p.TTL)
	fmt.Println("| Strategy | Runs where the job ran twice | Runs nobody did | Executions paused |\n|---|---:|---:|---:|")
	for _, r := range cr {
		fmt.Printf("| %s | %d | %d | %d |\n", r.Strategy, r.RanTwice, r.Skipped, r.PauseHits)
	}
	return nil
}

func yes(b bool) string {
	if b {
		return "**yes**"
	}
	return "no"
}
