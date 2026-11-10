// Command twosys keeps orders and payments (two databases) consistent four
// ways, with crashes injected, and shows what an in-doubt two-phase commit
// does to other writers.
//
//	go run ./cmd/twosys
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/the-vishal-kumar/database-tradeoffs-lab/internal/twosys"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://lab:lab@localhost:55432/lab?sslmode=disable"
	}
	e, cleanup, err := twosys.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer cleanup()
	const n, p, decline = 1000, 0.05, 0.05
	fmt.Printf("PostgreSQL, orders in database `lab` and payments in database `payments`. %d orders per strategy; a %.0f%% chance of a crash at every crash point; %.0f%% of cards declined.\n\n", n, p*100, decline*100)
	fmt.Println("| Strategy | Crashes | Inconsistent before recovery (outbox: before the relay runs) | Left in doubt / pending | Charges redelivered | Inconsistent after recovery | Took |")
	fmt.Println("|---|---:|---:|---|---:|---:|---:|")
	for _, s := range []string{"dual write", "two-phase commit", "saga", "outbox"} {
		r, err := e.Run(ctx, s, n, p, decline, 1)
		if err != nil {
			return err
		}
		left := "—"
		switch s {
		case "two-phase commit":
			left = fmt.Sprintf("%d prepared transactions", r.InDoubt)
		case "saga":
			left = fmt.Sprintf("%d orders stuck pending (all %d were visible as pending for a while)", r.StuckPending, r.PendingVisible)
		}
		fmt.Printf("| %s | %d | %d | %s | %d | %d | %s |\n", s, r.Crashes, r.InconsistentBefore, left, r.Redelivered, r.InconsistentAfter,
			fmt.Sprintf("%.1f s", r.Took.Seconds()))
	}
	waited, err := e.BlockedBy2PC(ctx, 2*time.Second)
	msg := "no error"
	if err != nil {
		msg = err.Error()
		if i := strings.Index(msg, "(SQLSTATE"); i > 0 {
			msg = strings.TrimSpace(msg[:i])
		}
	}
	fmt.Printf("\n### An in-doubt transaction holds its locks\n\nAn order updated inside a transaction that was prepared and then left in doubt (the coordinator died). Another session updating the same order, with `lock_timeout = 2s`, waited %.1f s and got: \"%s\". It would wait for as long as the transaction stays in doubt, and so would VACUUM's cleanup behind it.\n", waited.Seconds(), msg)
	return nil
}
