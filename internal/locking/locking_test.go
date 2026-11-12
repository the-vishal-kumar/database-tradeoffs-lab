package locking

import (
	"context"
	"os"
	"testing"
	"time"
)

// Pins every outcome the demo README reports, at sizes small enough for CI.
func TestOutcomes(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `make test`")
	}
	ctx := context.Background()
	db, err := Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(ctx, `DROP SCHEMA IF EXISTS locking CASCADE`)
		db.Close()
	})
	const work = 2 * time.Millisecond
	naive, pessimistic, atomic := Strategies[0], Strategies[1], Strategies[3]

	t.Run("high contention", func(t *testing.T) {
		byName := map[string]Result{}
		for _, s := range Strategies {
			r, err := Run(ctx, db, s, 16, 10, 1, work)
			if err != nil {
				t.Fatalf("%s: %v", s.Name, err)
			}
			byName[s.Name] = r
			if s.Name == naive.Name {
				if r.LostUpdates < int64(r.Ops)/2 {
					t.Errorf("naive read-modify-write should lose most updates on one row: %+v", r)
				}
				continue
			}
			if r.LostUpdates != 0 {
				t.Errorf("%s lost updates: %+v", s.Name, r)
			}
		}
		// Why the atomic UPDATE beats FOR UPDATE on a hot row: FOR UPDATE
		// holds the lock through the work, so buyers do it one at a time;
		// the atomic UPDATE lets them all work at once. This is checked as
		// overlap, not throughput, so a loaded CI runner can't flip it.
		if p := byName[pessimistic.Name]; p.MaxConcurrentWork != 1 {
			t.Errorf("FOR UPDATE should serialise the work on one row: %d buyers worked at once", p.MaxConcurrentWork)
		}
		if a := byName[atomic.Name]; a.MaxConcurrentWork < 2 {
			t.Errorf("the atomic UPDATE should let buyers work concurrently: max %d at once", a.MaxConcurrentWork)
		}
		if r := byName[Strategies[2].Name]; r.Retries < int64(r.Ops) {
			t.Errorf("optimistic retries should exceed purchases on one hot row: %+v", r)
		}
	})

	t.Run("oversell", func(t *testing.T) {
		for _, s := range Strategies {
			r, err := Oversell(ctx, db, s, 5, 30, work)
			if err != nil {
				t.Fatalf("%s: %v", s.Name, err)
			}
			if s.Name == naive.Name {
				if r.SalesConfirmed <= int64(r.Stock) {
					t.Errorf("naive should confirm more sales than stock: %+v", r)
				}
				continue
			}
			if r.SalesConfirmed != int64(r.Stock) || r.StockLeft != 0 {
				t.Errorf("%s should sell exactly the stock: %+v", s.Name, r)
			}
		}
	})
}
