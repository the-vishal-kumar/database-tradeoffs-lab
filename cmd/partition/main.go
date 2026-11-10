// Command partition compares one big PostgreSQL table with the same rows
// partitioned by month.
//
//	go run ./cmd/partition
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/the-vishal-kumar/database-tradeoffs-lab/internal/partition"
)

func mb(b int64) string {
	if b < 1_000_000 {
		return fmt.Sprintf("%.0f kB", float64(b)/1e3)
	}
	return fmt.Sprintf("%.0f MB", float64(b)/1e6)
}

func ms(d time.Duration) string {
	if d < 10*time.Millisecond {
		return fmt.Sprintf("%.2f ms", float64(d.Microseconds())/1000)
	}
	return fmt.Sprintf("%.0f ms", float64(d.Microseconds())/1000)
}

func main() {
	perMonth := flag.Int("per-month", 500_000, "rows per month (12 months)")
	flag.Parse()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://lab:lab@localhost:55432/lab?sslmode=disable"
	}
	ctx := context.Background()
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer db.Close()
	if err := run(ctx, db, *perMonth); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, db *pgxpool.Pool, perMonth int) error {
	var v string
	_ = db.QueryRow(ctx, `SHOW server_version`).Scan(&v)
	if err := partition.Setup(ctx, db, perMonth); err != nil {
		return err
	}
	fmt.Printf("PostgreSQL %s. %d rows over 2026 (%d a month), 1,000 tenants, 100-byte payload; indexes on (created_at) and (tenant_id, created_at). Sizes include indexes.\n\n", v, 12*perMonth, perMonth)

	qs, err := partition.Queries(ctx, db, 50)
	if err != nil {
		return err
	}
	fmt.Println("### Queries (median of 50 runs)\n\n| Query | Layout | p50 | Partitions in the plan |\n|---|---|---:|---:|")
	for _, q := range qs {
		p := "—"
		if q.Partitions > 0 {
			p = fmt.Sprint(q.Partitions)
		}
		fmt.Printf("| %s | %s | %s | %s |\n", q.Query, q.Layout, ms(q.P50), p)
	}

	rs, err := partition.Retire(ctx, db)
	if err != nil {
		return err
	}
	fmt.Println("\n### Removing the oldest month\n\n| Layout | How | Took | WAL written | Size before | Size after | Then VACUUM | Size after VACUUM |\n|---|---|---:|---:|---:|---:|---:|---:|")
	for _, r := range rs {
		fmt.Printf("| %s | %s | %s | %s | %s | %s | %s | %s |\n", r.Layout, r.How, ms(r.Took), mb(r.WALBytes), mb(r.SizeBefore), mb(r.SizeAfter), ms(r.VacuumTook), mb(r.SizeAfterVacuum))
	}
	_, err = db.Exec(ctx, `DROP SCHEMA part CASCADE`)
	return err
}
