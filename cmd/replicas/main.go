// Command replicas measures a PostgreSQL streaming replica: lag, stale
// reads after your own write, the cost of each synchronous_commit level,
// and commits when the synchronous replica is gone.
//
//	go run ./cmd/replicas
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/the-vishal-kumar/database-tradeoffs-lab/internal/replicas"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func ms(d time.Duration) string {
	if d < 10*time.Millisecond {
		return fmt.Sprintf("%.2f ms", float64(d.Microseconds())/1000)
	}
	return fmt.Sprintf("%.0f ms", float64(d.Microseconds())/1000)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	p, err := pgxpool.New(ctx, env("PRIMARY_URL", "postgres://lab:lab@localhost:55442/lab?sslmode=disable"))
	if err != nil {
		return err
	}
	defer p.Close()
	r, err := pgxpool.New(ctx, env("REPLICA_URL", "postgres://lab:lab@localhost:55443/lab?sslmode=disable"))
	if err != nil {
		return err
	}
	defer r.Close()
	e := replicas.Env{Primary: p, Replica: r}
	var v string
	_ = p.QueryRow(ctx, `SHOW server_version`).Scan(&v)
	fmt.Printf("PostgreSQL %s, one primary and one streaming replica, both in Docker on one machine (a network round trip well under a millisecond).\n\n", v)

	modes, err := e.ReadYourWrites(ctx, 2000)
	if err != nil {
		return err
	}
	fmt.Print("### 1. Write on the primary, then read it back from the replica\n\n4 background sessions insert 1 kB rows continuously; 2,000 increments, each read back from the replica right after its commit.\n\n")
	fmt.Println("| synchronous_commit | Commit p50 | Commit p99 | Stale reads from the replica | Replica lag p50 / max (while measuring) |")
	fmt.Println("|---|---:|---:|---:|---|")
	for _, m := range modes {
		fmt.Printf("| `%s` | %s | %s | %d of %d (%.1f%%) | %s / %s |\n", m.Mode, ms(m.CommitP50), ms(m.CommitP99), m.StaleReads, m.Writes,
			float64(m.StaleReads)*100/float64(m.Writes), ms(m.LagP50), ms(m.LagMax))
	}

	b, err := e.BigBatch(ctx, 2_000_000)
	if err != nil {
		return err
	}
	fmt.Printf("\n### 2. One big write\n\n`INSERT` of %d rows (%.0f MB of WAL) in one statement: took %s on the primary. The replica fell up to %.0f MB / %s behind, and caught up %s after the commit.\n",
		b.Rows, float64(b.WALBytes)/1e6, ms(b.Took), float64(b.MaxLagBytes)/1e6, ms(b.MaxLag), ms(b.CaughtUpAfterCommit))

	o, err := e.SyncStandbyGone(ctx, 3*time.Second)
	if err != nil {
		return err
	}
	cancelErr := "none: the client was told the commit succeeded"
	if o.CancelErr != "" {
		cancelErr = o.CancelErr
	}
	timeoutNote := "statement_timeout ended the wait"
	if o.TimeoutGuardHit {
		timeoutNote = "statement_timeout did **not** end it; the client gave up"
	}
	fmt.Printf("\n### 3. The synchronous replica is gone\n\n`synchronous_standby_names` names a replica that isn't connected; commits use the default `synchronous_commit = on`.\n\n"+
		"- With `statement_timeout = 3s`, an `UPDATE` waited **%s**: %s.\n"+
		"- With another session cancelling it after 3 s (as Ctrl-C would), it waited **%s**. Error returned to the client: %s. PostgreSQL's warning: \"%s\" — \"%s\" The row was **%s** on the primary afterwards.\n"+
		"- The same update with `synchronous_commit = local` took %s.\n",
		ms(o.TimeoutWaited), timeoutNote, ms(o.CancelWaited), cancelErr, o.Warning, o.Detail,
		map[bool]string{true: "changed", false: "not changed"}[o.VisibleOnPrimary], ms(o.AsyncCommitLatency))
	return nil
}
