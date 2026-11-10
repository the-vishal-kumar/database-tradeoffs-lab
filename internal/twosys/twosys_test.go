package twosys

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestStrategies(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `make test`")
	}
	ctx := context.Background()
	e, cleanup, err := Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	got := map[string]Result{}
	for _, s := range []string{"dual write", "two-phase commit", "saga", "outbox"} {
		r, err := e.Run(ctx, s, 200, 0.05, 0.05, 3)
		if err != nil {
			t.Fatal(err)
		}
		got[s] = r
	}
	if r := got["dual write"]; r.InconsistentAfter == 0 || r.InconsistentAfter != r.Crashes {
		t.Errorf("every dual-write crash should leave an order without its charge: %+v", r)
	}
	if r := got["two-phase commit"]; r.InconsistentBefore != 0 || r.InDoubt == 0 || r.InconsistentAfter != 0 {
		t.Errorf("2PC: never inconsistent, but in doubt until recovery: %+v", r)
	}
	if r := got["saga"]; r.StuckPending == 0 || r.InconsistentAfter != 0 {
		t.Errorf("saga: stuck pending until recovery, then consistent: %+v", r)
	}
	if r := got["outbox"]; r.InconsistentAfter != 0 || r.Redelivered == 0 {
		t.Errorf("outbox: consistent after the relay, with redeliveries absorbed: %+v", r)
	}
	waited, err := e.BlockedBy2PC(ctx, 500*time.Millisecond)
	if err == nil || waited < 400*time.Millisecond {
		t.Errorf("an in-doubt transaction should block the row: waited %s, err %v", waited, err)
	}
}
