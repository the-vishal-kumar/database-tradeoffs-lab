package isolation

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// What Postgres does, scenario by scenario, at RU / RC / RR / Serializable.
// If a Postgres upgrade changes any of these, this test says so.
var want = map[string][4]Outcome{
	"dirty-read":             {Prevented, Prevented, Prevented, Prevented},
	"non-repeatable-read":    {Anomaly, Anomaly, Prevented, Prevented},
	"phantom-read":           {Anomaly, Anomaly, Prevented, Prevented},
	"lost-update":            {Anomaly, Anomaly, Aborted, Aborted},
	"lost-update-atomic":     {Prevented, Prevented, Aborted, Aborted},
	"lost-update-for-update": {Prevented, Prevented, Aborted, Aborted},
	"write-skew":             {Anomaly, Anomaly, Anomaly, Aborted},
	"write-skew-for-update":  {Prevented, Prevented, Aborted, Aborted},
	// A retry only helps where Postgres detects the conflict.
	"lost-update-retry": {Anomaly, Anomaly, Prevented, Prevented},
	"write-skew-retry":  {Anomaly, Anomaly, Anomaly, Prevented},
}

func connect(t *testing.T) Env {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `make test` to start Postgres")
	}
	ctx := context.Background()
	var cs [3]*pgx.Conn
	for i := range cs {
		c, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close(ctx) })
		cs[i] = c
	}
	return Env{C1: cs[0], C2: cs[1], Obs: cs[2]}
}

func TestMatrix(t *testing.T) {
	e := connect(t)
	if len(want) != len(Scenarios) {
		t.Fatalf("want has %d scenarios, Scenarios has %d", len(want), len(Scenarios))
	}
	for _, s := range Scenarios {
		for i, l := range Levels {
			t.Run(s.Key+"/"+l.Name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				r, err := s.Run(ctx, e, l.Iso)
				if err != nil {
					t.Fatal(err)
				}
				if r.Outcome != want[s.Key][i] {
					t.Errorf("got %v (%s), want %v", r.Outcome, r.Detail, want[s.Key][i])
				}
			})
		}
	}
}

// The matrix must not depend on luck: run it several times.
func TestMatrixIsStable(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	e := connect(t)
	for range 5 {
		for _, s := range Scenarios {
			for i, l := range Levels {
				r, err := s.Run(context.Background(), e, l.Iso)
				if err != nil {
					t.Fatal(s.Key, l.Name, err)
				}
				if r.Outcome != want[s.Key][i] {
					t.Fatalf("%s at %s: got %v (%s), want %v", s.Key, l.Name, r.Outcome, r.Detail, want[s.Key][i])
				}
			}
		}
	}
}
