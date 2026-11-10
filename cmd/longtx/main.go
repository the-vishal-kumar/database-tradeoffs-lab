// Command longtx shows how one long-running transaction slows down a whole
// database: PostgreSQL (dead rows VACUUM can't remove) and MySQL InnoDB
// (undo history purge can't remove).
//
//	go run ./cmd/longtx
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/the-vishal-kumar/database-tradeoffs-lab/internal/longtx"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	d := flag.Duration("duration", 20*time.Second, "how long the hot rows are updated, per variant")
	flag.Parse()
	if err := run(*d); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func ms(d time.Duration) string { return fmt.Sprintf("%.2f ms", float64(d.Microseconds())/1000) }

func kb(b int64) string { return fmt.Sprintf("%d kB", b/1024) }

func run(d time.Duration) error {
	ctx := context.Background()
	p := longtx.Params{Writers: 8, Duration: d, Reads: 2000}

	cfg, err := pgxpool.ParseConfig(env("DATABASE_URL", "postgres://lab:lab@localhost:55432/lab?sslmode=disable"))
	if err != nil {
		return err
	}
	cfg.MaxConns = 16
	// Don't wait for the disk on every commit, so the writers aren't limited by
	// the laptop's fsync. It doesn't change how old versions are kept.
	cfg.ConnConfig.RuntimeParams["synchronous_commit"] = "off"
	pg, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer pg.Close()
	var pv string
	_ = pg.QueryRow(ctx, `SHOW server_version`).Scan(&pv)

	my, err := sql.Open("mysql", env("MYSQL_DSN", "root:lab@tcp(localhost:53306)/lab"))
	if err != nil {
		return err
	}
	defer my.Close()
	my.SetMaxOpenConns(16)
	var mv string
	if err := my.QueryRowContext(ctx, `SELECT version()`).Scan(&mv); err != nil {
		return err
	}
	fmt.Printf("PostgreSQL %s, MySQL %s (InnoDB). %d writers update 100 hot rows for %s per variant; reads are %d point lookups by primary key.\n\n",
		pv, mv, p.Writers, d, p.Reads)

	pr, err := longtx.PG(ctx, pg, p)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	fmt.Print("### PostgreSQL: VACUUM every second while the rows are updated\n\n")
	fmt.Println("| Variant | Updates | Dead row versions VACUUM couldn't remove | Table size | Point read p50 | Point read p99 | Read all 100 rows, p50 | After the transaction ends + VACUUM: dead / size / point read / read all |")
	fmt.Println("|---|---:|---:|---:|---:|---:|---:|---|")
	for _, r := range pr {
		fmt.Printf("| %s | %d | %d | %s | %s | %s | %s | %d / %s / %s / %s |\n", r.Variant, r.Updates, r.DeadLeft, kb(r.TableBytes),
			ms(r.ReadP50), ms(r.ReadP99), ms(r.ScanP50), r.AfterDead, kb(r.AfterBytes), ms(r.AfterReadP50), ms(r.AfterScanP50))
	}

	mr, err := longtx.MySQL(ctx, my, p)
	if err != nil {
		return fmt.Errorf("mysql: %w", err)
	}
	fmt.Print("\n### MySQL InnoDB: purge runs in the background\n\n")
	fmt.Println("| Variant | Updates | History list length | Point read p50: new session / old transaction | Read all 100 rows p50: new session / old transaction |")
	fmt.Println("|---|---:|---:|---|---|")
	for _, r := range mr {
		old, oldScan := "—", "—"
		if r.ReadOldP50 > 0 {
			old, oldScan = ms(r.ReadOldP50), ms(r.ScanOldP50)
		}
		fmt.Printf("| %s | %d | %d | %s / %s | %s / %s |\n", r.Variant, r.Updates, r.HistoryLength, ms(r.ReadNewP50), old, ms(r.ScanNewP50), oldScan)
	}
	return nil
}
