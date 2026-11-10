// Command slowquery measures why a PostgreSQL query slows down as the table
// grows, at several table sizes, and prints Markdown tables.
//
//	go run ./cmd/slowquery -sizes 10000,1000000,10000000
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/the-vishal-kumar/database-tradeoffs-lab/internal/slowquery"
)

func main() {
	sizesFlag := flag.String("sizes", "10000,1000000,10000000", "comma-separated table sizes")
	flag.Parse()
	if err := run(*sizesFlag); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(sizesFlag string) error {
	var sizes []int
	for _, s := range strings.Split(sizesFlag, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 10000 {
			return fmt.Errorf("bad size %q (minimum 10000)", s)
		}
		sizes = append(sizes, n)
	}
	ctx := context.Background()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://lab:lab@localhost:55432/lab?sslmode=disable"
	}
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	var version, sharedBuffers, workMem string
	_ = c.QueryRow(ctx, `SHOW server_version`).Scan(&version)
	_ = c.QueryRow(ctx, `SHOW shared_buffers`).Scan(&sharedBuffers)
	_ = c.QueryRow(ctx, `SHOW work_mem`).Scan(&workMem)
	fmt.Printf("PostgreSQL %s, shared_buffers %s, work_mem %s, parallel query off. Times: median of 5 runs after a warm-up.\n\n", version, sharedBuffers, workMem)

	bySize := map[int][]slowquery.Step{}
	for _, n := range sizes {
		start := time.Now()
		steps, err := slowquery.Run(ctx, c, n)
		if err != nil {
			return fmt.Errorf("%d rows: %w", n, err)
		}
		bySize[n] = steps
		fmt.Fprintf(os.Stderr, "%d rows done in %s\n", n, time.Since(start).Round(time.Second))
	}
	fmt.Print(slowquery.Report(sizes, bySize))
	_, err = c.Exec(ctx, `DROP SCHEMA IF EXISTS slowquery CASCADE`)
	return err
}
