package migrate

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// At a smaller size, so the comparisons are relative: the blocking way
// stalls clients for about as long as the change runs; the online way
// doesn't.
func TestOnlineChangesDontStall(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `make test`")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 20
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DROP SCHEMA IF EXISTS mig CASCADE`) })
	e := Env{DB: db, Rows: 1_000_000}

	idx, err := e.Index(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if blocking, online := idx[0], idx[1]; blocking.Load.Max < blocking.Took/2 || online.Load.Max > blocking.Load.Max/2 {
		t.Errorf("CREATE INDEX should stall writers for most of its run, CONCURRENTLY much less: %+v / %+v", blocking, online)
	}

	q, err := e.LockQueue(ctx, 1500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if queued, guarded := q[0], q[1]; queued.Load.Max < time.Second || guarded.Load.Max > 700*time.Millisecond {
		t.Errorf("an ALTER queued behind a report should block traffic; with lock_timeout it shouldn't: %+v / %+v", queued, guarded)
	}

	col, err := e.ColumnType(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rewrite, online := col[0], col[1]; rewrite.Load.Max < rewrite.Took/2 || online.Load.Max > rewrite.Load.Max/2 {
		t.Errorf("ALTER TYPE should stall clients; expand/contract much less: %+v / %+v", rewrite, online)
	}
}
