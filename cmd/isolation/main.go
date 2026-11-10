// Command isolation runs every anomaly at every isolation level and prints a
// Markdown table of what Postgres actually did.
//
//	DATABASE_URL=postgres://lab:lab@localhost:55432/lab?sslmode=disable go run ./cmd/isolation
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/the-vishal-kumar/database-tradeoffs-lab/internal/isolation"
)

func main() {
	details := flag.Bool("details", false, "also print what each transaction saw")
	flag.Parse()
	if err := run(*details); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(details bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://lab:lab@localhost:55432/lab?sslmode=disable"
	}
	var conns [3]*pgx.Conn
	for i := range conns {
		c, err := pgx.Connect(ctx, url)
		if err != nil {
			return err
		}
		defer c.Close(context.Background())
		conns[i] = c
	}
	env := isolation.Env{C1: conns[0], C2: conns[1], Obs: conns[2]}

	var version string
	if err := env.Obs.QueryRow(ctx, `SHOW server_version`).Scan(&version); err != nil {
		return err
	}
	fmt.Printf("PostgreSQL %s\n\n", version)

	header := []string{"Scenario"}
	for _, l := range isolation.Levels {
		header = append(header, l.Name)
	}
	fmt.Println("| " + strings.Join(header, " | ") + " |")
	fmt.Println("|" + strings.Repeat("---|", len(header)))
	var notes []string
	for _, s := range isolation.Scenarios {
		row := []string{s.Name}
		for _, l := range isolation.Levels {
			r, err := s.Run(ctx, env, l.Iso)
			if err != nil {
				return fmt.Errorf("%s at %s: %w", s.Key, l.Name, err)
			}
			row = append(row, r.Outcome.String())
			notes = append(notes, fmt.Sprintf("- %s, %s: %s", s.Name, l.Name, r.Detail))
		}
		fmt.Println("| " + strings.Join(row, " | ") + " |")
	}
	if details {
		fmt.Println()
		fmt.Println(strings.Join(notes, "\n"))
	}
	return nil
}
