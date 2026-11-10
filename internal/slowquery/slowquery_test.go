package slowquery

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The effects the post relies on, checked at a size small enough for CI.
// Buffers, not times, so the test doesn't depend on the machine.
func TestEffects(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `make test`")
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	steps, err := Run(ctx, c, 200_000)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = c.Exec(ctx, `DROP SCHEMA IF EXISTS slowquery CASCADE`) })
	get := func(scenario string, i int) Result {
		var got []Result
		for _, s := range steps {
			if s.Scenario == scenario {
				got = append(got, s.R)
			}
		}
		if i >= len(got) {
			t.Fatalf("%s: no step %d", scenario, i)
		}
		return got[i]
	}

	if slow, fast := get("tenant", 0), get("tenant", 1); slow.Buffers < 20*fast.Buffers {
		t.Errorf("tenant: (created_at) index read %d buffers, composite %d; want at least 20x", slow.Buffers, fast.Buffers)
	}
	if off, key := get("paging", 0), get("paging", 1); off.Buffers < 20*key.Buffers {
		t.Errorf("paging: OFFSET %d buffers vs keyset %d", off.Buffers, key.Buffers)
	}
	if ord, scat := get("correlation", 0), get("correlation", 1); scat.Buffers < 5*ord.Buffers || ord.Rows != scat.Rows {
		t.Errorf("correlation: in-order %d buffers vs scattered %d", ord.Buffers, scat.Buffers)
	}
	if before, after := get("stats", 0), get("stats", 2); before.EstRows*100 > float64(before.ActRows) || after.EstRows*2 < float64(after.ActRows) {
		t.Errorf("stats: estimate %.0f (actual %d) before ANALYZE, %.0f (actual %d) after", before.EstRows, before.ActRows, after.EstRows, after.ActRows)
	}
	v0, v1, v2 := get("visibility", 0), get("visibility", 1), get("visibility", 2)
	if v0.HeapFetch != 0 || v1.HeapFetch <= 0 || v2.HeapFetch != 0 {
		t.Errorf("visibility: heap fetches %d, %d, %d; want 0, >0, 0 (plans %q, %q)", v0.HeapFetch, v1.HeapFetch, v2.HeapFetch, v0.Plan, v1.Plan)
	}
	if full, deleted, vacuumed := get("bloat", 0), get("bloat", 1), get("bloat", 3); deleted.Buffers < full.Buffers/2 || vacuumed.Buffers*5 > full.Buffers {
		t.Errorf("bloat: buffers full %d, after delete %d, after VACUUM FULL %d", full.Buffers, deleted.Buffers, vacuumed.Buffers)
	}
}
