// Package slowquery measures why PostgreSQL queries slow down as a table
// grows even though they use an index. It loads the same orders table at
// several sizes and runs each scenario with EXPLAIN (ANALYZE, BUFFERS),
// recording the plan, rows, buffers touched and execution time.
//
// Buffers (8 KB pages) are the number to compare: they don't depend on the
// machine. Times are medians of several runs on a warm cache and are only
// meaningful relative to each other.
package slowquery

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Result is one query, measured.
type Result struct {
	Plan      string  // node types, outermost first, e.g. "Limit > Index Scan"
	Rows      int64   // rows the query returned
	Buffers   int64   // shared buffers hit + read (8 KB pages)
	HeapFetch int64   // Index Only Scan heap fetches (-1 if no such node)
	EstRows   float64 // planner's row estimate for the filtered scan of orders
	ActRows   int64   // actual rows from that scan
	Ms        float64 // median execution time
}

// Step is one measured variant inside a scenario.
type Step struct {
	Scenario, Variant string
	R                 Result
	Note              string // extra context, e.g. table size
}

const runs = 5

// Measure runs EXPLAIN ANALYZE runs+1 times (the first warms the cache) and
// keeps the median time and the last run's plan details. TIMING OFF: per-node
// clock calls would add overhead proportional to the rows each node handles,
// inflating row-heavy plans (OFFSET) against light ones (keyset).
func Measure(ctx context.Context, c *pgx.Conn, sql string, args ...any) (Result, error) {
	var times []float64
	var last Result
	for i := 0; i <= runs; i++ {
		var raw []byte
		if err := c.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, TIMING OFF, FORMAT JSON) "+sql, args...).Scan(&raw); err != nil {
			return Result{}, fmt.Errorf("%s: %w", firstLine(sql), err)
		}
		r, err := parse(raw)
		if err != nil {
			return Result{}, err
		}
		if i > 0 {
			times = append(times, r.Ms)
		}
		last = r
	}
	sort.Float64s(times)
	last.Ms = times[len(times)/2]
	return last, nil
}

type node struct {
	NodeType     string  `json:"Node Type"`
	ActualRows   float64 `json:"Actual Rows"`
	ActualLoops  float64 `json:"Actual Loops"`
	PlanRows     float64 `json:"Plan Rows"`
	SharedHit    int64   `json:"Shared Hit Blocks"`
	SharedRead   int64   `json:"Shared Read Blocks"`
	HeapFetches  *int64  `json:"Heap Fetches"`
	Filter       string  `json:"Filter"`
	IndexCond    string  `json:"Index Cond"`
	RecheckCond  string  `json:"Recheck Cond"`
	RelationName string  `json:"Relation Name"`
	IndexName    string  `json:"Index Name"`
	Plans        []node  `json:"Plans"`
}

func parse(raw []byte) (Result, error) {
	var out []struct {
		Plan          node    `json:"Plan"`
		ExecutionTime float64 `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out) != 1 {
		return Result{}, fmt.Errorf("explain output: %v", err)
	}
	top := out[0].Plan
	r := Result{Rows: int64(top.ActualRows), Buffers: top.SharedHit + top.SharedRead,
		Ms: out[0].ExecutionTime, HeapFetch: -1}
	var types []string
	var walk func(n node)
	walk = func(n node) {
		types = append(types, n.NodeType)
		if n.HeapFetches != nil {
			r.HeapFetch = *n.HeapFetches
		}
		// The scan of orders that applies the WHERE clause: estimate vs actual.
		onOrders := n.RelationName == "orders" || strings.HasPrefix(n.IndexName, "orders_")
		if onOrders && (n.Filter != "" || n.IndexCond != "" || n.RecheckCond != "") && r.ActRows == 0 {
			r.EstRows = n.PlanRows
			r.ActRows = int64(n.ActualRows * n.ActualLoops)
		}
		for _, ch := range n.Plans {
			walk(ch)
		}
	}
	walk(top)
	r.Plan = strings.Join(compact(types), " > ")
	return r, nil
}

func compact(xs []string) []string {
	var out []string
	for _, x := range xs {
		if x == "Gather" || x == "Gather Merge" {
			x += " (parallel)"
		}
		if len(out) == 0 || out[len(out)-1] != x {
			out = append(out, x)
		}
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}

func exec(ctx context.Context, c *pgx.Conn, sql string, args ...any) error {
	if _, err := c.Exec(ctx, sql, args...); err != nil {
		return fmt.Errorf("%s: %w", firstLine(sql), err)
	}
	return nil
}

// Load (re)creates the orders table with n rows:
//   - created_at increases with id, so it matches the physical order on disk;
//   - customer_id is spread pseudo-randomly, so it doesn't;
//   - tenant 7 is a small, old tenant: every 10th row of the first 1%;
//   - status is 80% shipped, 15% paid, 5% cancelled.
//
// Autovacuum is off for this table so the scenarios control VACUUM and
// ANALYZE themselves.
func Load(ctx context.Context, c *pgx.Conn, n int) error {
	stmts := []string{
		// Its own schema, so other demos' tables named "orders" don't collide.
		`CREATE SCHEMA IF NOT EXISTS slowquery`,
		`SET search_path TO slowquery`,
		// One process per query: parallel workers would split the work (and
		// round per-worker counts), making sizes hard to compare.
		`SET max_parallel_workers_per_gather = 0`,
		`DROP TABLE IF EXISTS orders`,
		`CREATE TABLE orders (
			id          bigint PRIMARY KEY,
			tenant_id   int NOT NULL,
			customer_id int NOT NULL,
			status      text NOT NULL,
			created_at  timestamptz NOT NULL,
			total_cents int NOT NULL,
			note        text NOT NULL
		) WITH (autovacuum_enabled = false)`,
		fmt.Sprintf(`INSERT INTO orders
			SELECT g,
			       CASE WHEN g <= %[1]d / 100 AND g %% 10 = 0 THEN 7 ELSE 100 + (g * 7919) %% 900 END,
			       (g * 2654435761) %% 1000000,
			       CASE WHEN g %% 100 < 80 THEN 'shipped' WHEN g %% 100 < 95 THEN 'paid' ELSE 'cancelled' END,
			       timestamptz '2020-01-01 00:00:00+00' + g * interval '1 second',
			       (g * 31) %% 100000,
			       repeat('x', 60)
			  FROM generate_series(1::bigint, %[1]d) AS g`, n),
		// 1,000,000 customers (customer_id spans 0..999,999); 10% in NL.
		`DROP TABLE IF EXISTS customers`,
		`CREATE TABLE customers (id int PRIMARY KEY, country text NOT NULL) WITH (autovacuum_enabled = false)`,
		`INSERT INTO customers SELECT g, CASE WHEN g % 10 = 0 THEN 'NL' ELSE 'IN' END FROM generate_series(0, 999999) AS g`,
		`VACUUM (ANALYZE) customers`,
		`CREATE INDEX orders_created_idx  ON orders (created_at)`,
		`CREATE INDEX orders_customer_idx ON orders (customer_id)`,
		`CREATE INDEX orders_status_idx   ON orders (status)`,
		`VACUUM (ANALYZE) orders`,
	}
	for _, s := range stmts {
		if err := exec(ctx, c, s); err != nil {
			return err
		}
	}
	return nil
}

func at(n int, frac float64) time.Time {
	return time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(float64(n)*frac) * time.Second)
}

// Run loads n rows and measures every scenario, in an order where later
// scenarios may change the data (stale stats inserts, bloat deletes last).
func Run(ctx context.Context, c *pgx.Conn, n int) ([]Step, error) {
	if err := Load(ctx, c, n); err != nil {
		return nil, err
	}
	var steps []Step
	m := func(scenario, variant, note, sql string, args ...any) error {
		r, err := Measure(ctx, c, sql, args...)
		if err != nil {
			return err
		}
		steps = append(steps, Step{scenario, variant, r, note})
		return nil
	}
	size := func() string {
		var s string
		_ = c.QueryRow(ctx, `SELECT pg_size_pretty(pg_relation_size('orders'))`).Scan(&s)
		return "table " + s
	}

	// 1. Latest orders of a small, old tenant, with only a created_at index.
	q1 := `SELECT id, created_at, total_cents FROM orders WHERE tenant_id = 7 ORDER BY created_at DESC LIMIT 20`
	if err := m("tenant", "index on (created_at) only", "", q1); err != nil {
		return nil, err
	}
	if err := exec(ctx, c, `CREATE INDEX orders_tenant_created_idx ON orders (tenant_id, created_at)`); err != nil {
		return nil, err
	}
	if err := exec(ctx, c, `ANALYZE orders`); err != nil {
		return nil, err
	}
	if err := m("tenant", "index on (tenant_id, created_at)", "", q1); err != nil {
		return nil, err
	}
	if err := exec(ctx, c, `DROP INDEX orders_tenant_created_idx`); err != nil {
		return nil, err
	}

	// 2. Page 10% deep: OFFSET vs keyset (WHERE created_at < last seen).
	offset := n / 10
	var cursor time.Time
	if err := c.QueryRow(ctx, `SELECT created_at FROM orders ORDER BY created_at DESC OFFSET $1 LIMIT 1`, offset-1).Scan(&cursor); err != nil {
		return nil, err
	}
	if err := m("paging", fmt.Sprintf("OFFSET %d LIMIT 20", offset), "",
		`SELECT id, created_at FROM orders ORDER BY created_at DESC OFFSET $1 LIMIT 20`, offset); err != nil {
		return nil, err
	}
	if err := m("paging", "keyset: WHERE created_at < cursor LIMIT 20", "",
		`SELECT id, created_at FROM orders WHERE created_at < $1 ORDER BY created_at DESC LIMIT 20`, cursor); err != nil {
		return nil, err
	}

	// 3. The same 0.5% of rows, by a column that follows the physical order
	// (created_at) vs one that doesn't (customer_id).
	if err := m("correlation", "0.5% of rows by created_at range", "",
		`SELECT sum(total_cents) FROM orders WHERE created_at >= $1 AND created_at < $2`, at(n, 0.40), at(n, 0.405)); err != nil {
		return nil, err
	}
	if err := m("correlation", "0.5% of rows by customer_id range", "",
		`SELECT sum(total_cents) FROM orders WHERE customer_id >= 400000 AND customer_id < 405000`); err != nil {
		return nil, err
	}

	// 4. Stale statistics: 5% of rows (every 20th id) get a status the planner
	// has never seen. Until ANALYZE it thinks almost no rows match. (status is
	// indexed, so these are not HOT updates: the new row versions land on new
	// pages at the end of the table.)
	if err := exec(ctx, c, `UPDATE orders SET status = 'refunded' WHERE id % 20 = 3`); err != nil {
		return nil, err
	}
	q4 := `SELECT count(*), avg(total_cents) FROM orders WHERE status = 'refunded'`
	q4j := `SELECT count(*) FROM orders o JOIN customers c ON c.id = o.customer_id
	         WHERE o.status = 'refunded' AND c.country = 'NL'`
	if err := m("stats", "one table, before ANALYZE", "", q4); err != nil {
		return nil, err
	}
	if err := m("stats", "joined to customers, before ANALYZE", "", q4j); err != nil {
		return nil, err
	}
	if err := exec(ctx, c, `ANALYZE orders`); err != nil {
		return nil, err
	}
	if err := m("stats", "one table, after ANALYZE", "", q4); err != nil {
		return nil, err
	}
	if err := m("stats", "joined to customers, after ANALYZE", "", q4j); err != nil {
		return nil, err
	}

	// 5. Index-only scans need the visibility map: count a 10% range, then
	// update 10% of rows (spread out), then VACUUM.
	q5 := `SELECT count(*) FROM orders WHERE created_at >= $1 AND created_at < $2`
	a, b := at(n, 0.30), at(n, 0.40)
	if err := exec(ctx, c, `VACUUM orders`); err != nil {
		return nil, err
	}
	if err := m("visibility", "after VACUUM", "", q5, a, b); err != nil {
		return nil, err
	}
	if err := exec(ctx, c, `UPDATE orders SET total_cents = total_cents + 1 WHERE id % 10 = 0`); err != nil {
		return nil, err
	}
	if err := m("visibility", "after updating 10% of rows, no VACUUM", "", q5, a, b); err != nil {
		return nil, err
	}
	if err := exec(ctx, c, `VACUUM orders`); err != nil {
		return nil, err
	}
	if err := m("visibility", "after VACUUM again", "", q5, a, b); err != nil {
		return nil, err
	}

	// 6. Bloat: delete the oldest 90% and count what's left. The filter is on
	// an unindexed column, so the table itself must be read.
	q6 := `SELECT count(*) FROM orders WHERE note <> ''`
	if err := m("bloat", "full table", size(), q6); err != nil {
		return nil, err
	}
	if err := exec(ctx, c, `DELETE FROM orders WHERE created_at < $1`, at(n, 0.90)); err != nil {
		return nil, err
	}
	if err := m("bloat", "after deleting 90%, no VACUUM", size(), q6); err != nil {
		return nil, err
	}
	if err := exec(ctx, c, `VACUUM orders`); err != nil {
		return nil, err
	}
	if err := m("bloat", "after VACUUM", size(), q6); err != nil {
		return nil, err
	}
	if err := exec(ctx, c, `VACUUM FULL orders`); err != nil {
		return nil, err
	}
	if err := m("bloat", "after VACUUM FULL (rewrites the table)", size(), q6); err != nil {
		return nil, err
	}
	return steps, nil
}

// Scenarios in report order, with their headings.
var Scenarios = []struct{ Key, Title string }{
	{"tenant", "1. Latest 20 orders of a small, old tenant"},
	{"paging", "2. A page 10% deep: OFFSET vs keyset"},
	{"correlation", "3. 0.5% of rows: by a column in disk order vs a scattered one"},
	{"stats", "4. Stale statistics after 5% of rows change to a new value"},
	{"visibility", "5. Index-only scan and the visibility map"},
	{"bloat", "6. Counting rows after deleting 90%"},
}

// Report renders one Markdown table per scenario, sizes as columns.
func Report(sizes []int, bySize map[int][]Step) string {
	var b strings.Builder
	for _, sc := range Scenarios {
		fmt.Fprintf(&b, "### %s\n\n| Variant | %s |\n|---|%s\n", sc.Title, sizeHeader(sizes), strings.Repeat("---|", len(sizes)))
		var variants []string
		for _, n := range sizes {
			for _, st := range bySize[n] {
				if st.Scenario == sc.Key && !slices.Contains(variants, variantKey(st)) {
					variants = append(variants, variantKey(st))
				}
			}
		}
		for _, v := range variants {
			fmt.Fprintf(&b, "| %s |", v)
			for _, n := range sizes {
				cell := "-"
				for _, st := range bySize[n] {
					if st.Scenario == sc.Key && variantKey(st) == v {
						cell = cellText(sc.Key, st)
					}
				}
				fmt.Fprintf(&b, " %s |", cell)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// variantKey strips the offset value so OFFSET rows line up across sizes.
func variantKey(st Step) string {
	if strings.HasPrefix(st.Variant, "OFFSET ") {
		return "OFFSET (10% deep) LIMIT 20"
	}
	return st.Variant
}

func cellText(scenario string, st Step) string {
	r := st.R
	s := fmt.Sprintf("%s ms · %s buf", ms(r.Ms), thousands(r.Buffers))
	switch scenario {
	case "stats":
		s += fmt.Sprintf(" · est %s / actual %s rows", thousands(int64(r.EstRows)), thousands(r.ActRows))
	case "visibility":
		if r.HeapFetch >= 0 {
			s += " · heap fetches " + thousands(r.HeapFetch)
		}
	case "bloat":
		s += " · " + st.Note
	}
	return s + "<br>" + r.Plan
}

func ms(v float64) string {
	switch {
	case v < 1:
		return fmt.Sprintf("%.2f", v)
	case v < 100:
		return fmt.Sprintf("%.1f", v)
	}
	return thousands(int64(v + 0.5))
}

func sizeHeader(sizes []int) string {
	var parts []string
	for _, n := range sizes {
		parts = append(parts, thousands(int64(n))+" rows")
	}
	return strings.Join(parts, " | ")
}

func thousands(v int64) string {
	s := fmt.Sprint(v)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var out []byte
	for i := range len(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}
