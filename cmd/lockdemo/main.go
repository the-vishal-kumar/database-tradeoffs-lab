// Command lockdemo compares locking the row (SELECT ... FOR UPDATE) with
// optimistic retries (a version column), an atomic UPDATE, SERIALIZABLE, and
// a naive read-modify-write, at high and low contention.
//
//	go run ./cmd/lockdemo
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/the-vishal-kumar/database-tradeoffs-lab/internal/locking"
)

func main() {
	workers := flag.Int("workers", 32, "concurrent buyers")
	per := flag.Int("per", 25, "purchases per buyer")
	items := flag.Int("items", 1000, "items in the low-contention run")
	work := flag.Duration("work", 2*time.Millisecond, "application work between the read and the write")
	flag.Parse()
	if err := run(*workers, *per, *items, *work); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(workers, per, items int, work time.Duration) error {
	ctx := context.Background()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://lab:lab@localhost:55432/lab?sslmode=disable"
	}
	db, err := locking.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer db.Close()
	var v string
	_ = db.QueryRow(ctx, `SHOW server_version`).Scan(&v)
	fmt.Printf("PostgreSQL %s. %d buyers × %d purchases, %s of application work between read and write, pool of %d.\n\n",
		v, workers, per, work, locking.PoolSize)

	for _, n := range []int{1, items} {
		label := "### High contention: every buyer wants the same item"
		if n > 1 {
			label = fmt.Sprintf("### Low contention: buyers spread over %d items", n)
		}
		fmt.Printf("%s\n\n| Strategy | Sold | Stock went down by | Lost updates | Retries | Gave up | Purchases/s | p50 | p99 |\n|---|---:|---:|---:|---:|---:|---:|---:|---:|\n", label)
		for _, s := range locking.Strategies {
			r, err := locking.Run(ctx, db, s, workers, per, n, work)
			if err != nil {
				return fmt.Errorf("%s: %w", s.Name, err)
			}
			fmt.Printf("| %s | %d | %d | %d | %d | %d | %.0f | %s | %s |\n", r.Strategy, r.Sold, r.Decremented,
				r.LostUpdates, r.Retries, r.GaveUp, r.PerSec, ms(r.P50), ms(r.P99))
		}
		fmt.Println()
	}

	const stock, buyers = 10, 50
	fmt.Printf("### The last %d tickets: %d buyers at once\n\n| Strategy | Buyers told \"you got one\" | Stock left | Gave up |\n|---|---:|---:|---:|\n", stock, buyers)
	for _, s := range locking.Strategies {
		r, err := locking.Oversell(ctx, db, s, stock, buyers, work)
		if err != nil {
			return fmt.Errorf("%s: %w", s.Name, err)
		}
		fmt.Printf("| %s | %d | %d | %d |\n", r.Strategy, r.SalesConfirmed, r.StockLeft, r.GaveUp)
	}
	_, err = db.Exec(ctx, `DROP SCHEMA locking CASCADE`)
	return err
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000) }
