// Command livemigrate runs schema changes on a 5M-row table while 8
// clients keep reading and writing it, and reports how long they stalled.
//
//	go run ./cmd/livemigrate
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/the-vishal-kumar/database-tradeoffs-lab/internal/migrate"
)

func ms(d time.Duration) string {
	if d < 10*time.Millisecond {
		return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000)
	}
	if d >= 10*time.Second {
		return fmt.Sprintf("%.1f s", d.Seconds())
	}
	return fmt.Sprintf("%.0f ms", float64(d.Microseconds())/1000)
}

func main() {
	rows := flag.Int("rows", 5_000_000, "rows in the table")
	flag.Parse()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://lab:lab@localhost:55432/lab?sslmode=disable"
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cfg.MaxConns = 20
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer db.Close()
	e := migrate.Env{DB: db, Rows: *rows}
	var v string
	_ = db.QueryRow(ctx, `SHOW server_version`).Scan(&v)
	fmt.Printf("PostgreSQL %s. A %d-row table; 8 clients each loop on a primary-key read and a one-row update while the change runs. Latency is per read+update pair.\n\n", v, *rows)
	fmt.Println("| Change | How | Change took | Clients: p50 | p99 | slowest | requests over 1 s | errors |")
	fmt.Println("|---|---|---:|---:|---:|---:|---:|---:|")
	var notes []string
	for _, f := range []func(context.Context) ([]migrate.Step, error){
		e.ColumnType, e.Index, e.AddColumn,
		func(ctx context.Context) ([]migrate.Step, error) { return e.LockQueue(ctx, 5*time.Second) },
	} {
		steps, err := f(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		for _, s := range steps {
			fmt.Printf("| %s | %s | %s | %s | %s | **%s** | %d | %d |\n", s.Change, s.How, ms(s.Took), ms(s.Load.P50), ms(s.Load.P99), ms(s.Load.Max), s.Load.Over1s, s.Load.Errors)
			if s.Note != "" {
				notes = append(notes, fmt.Sprintf("- %s, %s: %s.", s.Change, s.How, s.Note))
			}
		}
	}
	fmt.Println()
	for _, n := range notes {
		fmt.Println(n)
	}
	_, _ = db.Exec(ctx, `DROP SCHEMA IF EXISTS mig CASCADE`)
}
