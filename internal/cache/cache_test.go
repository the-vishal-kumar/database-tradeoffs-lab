package cache

import (
	"context"
	"os"
	"testing"
	"time"
)

// Pins every effect the post relies on, at sizes small enough for CI.
func TestEffects(t *testing.T) {
	dbURL, redisURL := os.Getenv("DATABASE_URL"), os.Getenv("REDIS_URL")
	if dbURL == "" || redisURL == "" {
		t.Skip("DATABASE_URL and REDIS_URL not set; run `make test`")
	}
	ctx := context.Background()
	e, cleanup, err := Connect(ctx, dbURL, redisURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)

	t.Run("staleness", func(t *testing.T) {
		w, err := Staleness(ctx, e, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if w[0].StaleReads == 0 || w[0].StaleFor < 500*time.Millisecond {
			t.Errorf("TTL only should serve the old price until expiry: %+v", w[0])
		}
		if w[1].StaleReads != 0 {
			t.Errorf("deleting after the write should serve no stale reads: %+v", w[1])
		}
	})

	t.Run("race", func(t *testing.T) {
		r, err := Race(ctx, e, time.Minute, 300*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if r[0].Cached != 100 {
			t.Errorf("delete-after-write race should leave the old price cached: %+v", r[0])
		}
		if r[1].Cached != 0 {
			t.Errorf("double delete should leave the cache empty after the delay: %+v", r[1])
		}
		if r[2].Cached != 200 || r[2].StaleFor != "never stale" {
			t.Errorf("versioned set should keep the new price: %+v", r[2])
		}
	})

	t.Run("rollback", func(t *testing.T) {
		r, err := Rollback(ctx, e, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if r[0].Served != 300 || r[0].DBPrice != 100 {
			t.Errorf("caching inside the transaction should serve a rolled-back price: %+v", r[0])
		}
		if r[1].Served != 100 {
			t.Errorf("caching after commit should serve the committed price: %+v", r[1])
		}
	})

	t.Run("stampede", func(t *testing.T) {
		const instances, per = 2, 20
		r, err := Stampede(ctx, e, instances, per, 50*time.Millisecond, 10*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if r[0].DBQueries <= instances {
			t.Errorf("naive: want many queries, got %d", r[0].DBQueries)
		}
		if r[1].DBQueries > instances {
			t.Errorf("singleflight: want at most one query per instance, got %d", r[1].DBQueries)
		}
		if r[2].DBQueries != 1 {
			t.Errorf("lock: want exactly one query, got %d", r[2].DBQueries)
		}
		if r[3].DBQueries != 1 || r[3].ServedStale != instances*per {
			t.Errorf("stale-while-revalidate: want one background query and every reader served the old copy: %+v", r[3])
		}
	})

	t.Run("jitter", func(t *testing.T) {
		// Few keys and a 1 s TTL: readers find every expired key well inside
		// one jitter window (±300 ms) even on a slow, loaded runner. With
		// 500 keys and 500 ms, readers under -race on a busy machine took
		// longer to reach every key than the jitter spread, and the test
		// failed (93 vs 91).
		r, err := Jitter(ctx, e, 100, time.Second, 0.3)
		if err != nil {
			t.Fatal(err)
		}
		// How synchronised the misses are still depends on how fast readers
		// cycle through the keys, so only require a clear cut.
		if float64(r[1].PeakPer100ms) > 0.7*float64(r[0].PeakPer100ms) {
			t.Errorf("jitter should cut the peak by at least 30%%: same TTL %d, jittered %d", r[0].PeakPer100ms, r[1].PeakPer100ms)
		}
	})
}
