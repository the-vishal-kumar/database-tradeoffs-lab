package replicas

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Needs the primary and replica from compose profile "replicas":
// `make replicas-test`.
func TestReplicas(t *testing.T) {
	pu, ru := os.Getenv("PRIMARY_URL"), os.Getenv("REPLICA_URL")
	if pu == "" || ru == "" {
		t.Skip("PRIMARY_URL and REPLICA_URL not set; run `make replicas-test`")
	}
	ctx := context.Background()
	p, err := pgxpool.New(ctx, pu)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	r, err := pgxpool.New(ctx, ru)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	e := Env{Primary: p, Replica: r}

	modes, err := e.ReadYourWrites(ctx, 300)
	if err != nil {
		t.Fatal(err)
	}
	local, apply := modes[0], modes[3]
	if local.StaleReads < local.Writes/2 {
		t.Errorf("an async replica read right after the commit should usually be stale: %+v", local)
	}
	if apply.StaleReads != 0 {
		t.Errorf("remote_apply should never be stale: %+v", apply)
	}

	o, err := e.SyncStandbyGone(ctx, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !o.TimeoutGuardHit || o.CancelErr != "" || !o.VisibleOnPrimary ||
		!strings.Contains(o.Detail, "already committed locally") {
		t.Errorf("a cancelled wait for a missing sync replica: %+v", o)
	}
}
