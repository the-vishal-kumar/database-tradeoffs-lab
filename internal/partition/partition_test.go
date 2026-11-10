package partition

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPartitioning(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `make test`")
	}
	ctx := context.Background()
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DROP SCHEMA IF EXISTS part CASCADE`) })
	if err := Setup(ctx, db, 50_000); err != nil {
		t.Fatal(err)
	}
	qs, err := Queries(ctx, db, 5)
	if err != nil {
		t.Fatal(err)
	}
	if qs[1].Partitions != 1 || qs[5].Partitions != 12 {
		t.Errorf("pruning: recent query touched %d partitions (want 1), no-key query %d (want 12)", qs[1].Partitions, qs[5].Partitions)
	}
	rs, err := Retire(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	del, drop := rs[0], rs[1]
	if del.SizeAfterVacuum != del.SizeBefore {
		t.Errorf("DELETE + VACUUM shouldn't shrink the table: %d -> %d", del.SizeBefore, del.SizeAfterVacuum)
	}
	if drop.SizeAfter >= drop.SizeBefore*95/100 {
		t.Errorf("dropping a month should give back about 1/12: %d -> %d", drop.SizeBefore, drop.SizeAfter)
	}
	if drop.WALBytes*5 > del.WALBytes {
		t.Errorf("DROP should write far less WAL than DELETE: %d vs %d", drop.WALBytes, del.WALBytes)
	}
}
