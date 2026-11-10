package dlock

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// Pins every outcome the post relies on.
func TestOutcomes(t *testing.T) {
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

	t.Run("paused holder", func(t *testing.T) {
		r, err := Paused(ctx, e, 100*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if !r[0].BothHeld || !strings.HasPrefix(r[0].Final, "written by A") {
			t.Errorf("a Redis lease alone should let the paused holder overwrite: %+v", r[0])
		}
		if !strings.HasPrefix(r[1].LateWrite, "rejected") || r[1].Final != "written by B" {
			t.Errorf("a fencing token should reject the late write: %+v", r[1])
		}
		if r[2].BothHeld || r[2].Final != "written by B" {
			t.Errorf("an advisory lock should make B wait: %+v", r[2])
		}
		if !strings.HasPrefix(r[3].LateWrite, "failed") || r[3].Final != "written by B" {
			t.Errorf("a dead connection should take A's write with it: %+v", r[3])
		}
	})

	t.Run("unlock", func(t *testing.T) {
		r, err := Unlock(ctx, e, 100*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if !r[0].CAcquired || r[1].CAcquired {
			t.Errorf("DEL should free B's lock, compare-and-delete shouldn't: %+v", r)
		}
	})

	t.Run("cron", func(t *testing.T) {
		// Every execution pauses past the lease, so the Redis lock fails often.
		r, err := Cron(ctx, e, CronParams{Runs: 40, Instances: 3, Skew: 50 * time.Millisecond,
			Work: 5 * time.Millisecond, TTL: 30 * time.Millisecond, Pause: 60 * time.Millisecond, PauseProb: 1})
		if err != nil {
			t.Fatal(err)
		}
		if r[0].RanTwice == 0 {
			t.Errorf("the Redis lease should let some runs happen twice: %+v", r[0])
		}
		for _, x := range r[1:] {
			if x.RanTwice != 0 || x.Skipped != 0 {
				t.Errorf("%s should run every job exactly once: %+v", x.Strategy, x)
			}
		}
	})
}
