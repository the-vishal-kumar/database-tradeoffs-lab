package longtx

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgxpool"
)

var short = Params{Writers: 8, Duration: 5 * time.Second, Reads: 500}

func TestPostgres(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `make test`")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 16
	cfg.ConnConfig.RuntimeParams["synchronous_commit"] = "off"
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r, err := PG(ctx, db, short)
	if err != nil {
		t.Fatal(err)
	}
	none, open, timeout := r[0], r[1], r[2]
	if none.DeadLeft != 0 || open.DeadLeft < open.Updates/2 || timeout.DeadLeft != 0 {
		t.Errorf("dead rows left: none %d, open %d of %d updates, timeout %d", none.DeadLeft, open.DeadLeft, open.Updates, timeout.DeadLeft)
	}
	if open.TableBytes < 10*none.TableBytes {
		t.Errorf("the table should bloat while the transaction is open: %d vs %d bytes", open.TableBytes, none.TableBytes)
	}
	if open.ScanP50 < 3*none.ScanP50 {
		t.Errorf("reading the bloated table should be slower: %s vs %s", open.ScanP50, none.ScanP50)
	}
	if open.AfterDead != 0 || open.AfterBytes != open.TableBytes {
		t.Errorf("VACUUM should remove the dead rows but not shrink the file: %+v", open)
	}
}

func TestMySQL(t *testing.T) {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("MYSQL_DSN not set; run `make test`")
	}
	ctx := context.Background()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(16)
	r, err := MySQL(ctx, db, short)
	if err != nil {
		t.Fatal(err)
	}
	none, open := r[0], r[1]
	if open.HistoryLength < 10*none.HistoryLength || open.HistoryLength < open.Updates/2 {
		t.Errorf("purge should be held back: %d vs %d (updates %d)", open.HistoryLength, none.HistoryLength, open.Updates)
	}
	if open.ScanOldP50 < 5*open.ScanNewP50 {
		t.Errorf("the old transaction should pay to rebuild old versions: %s vs %s", open.ScanOldP50, open.ScanNewP50)
	}
}
