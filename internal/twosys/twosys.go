// Package twosys keeps an order (in one database) and its payment (in
// another) consistent four ways, with crashes injected at random points:
//
//   - dual write: commit the order, then the payment;
//   - two-phase commit: PREPARE TRANSACTION in both, record the decision,
//     COMMIT PREPARED in both; recovery finishes in-doubt transactions;
//   - saga: order "pending", charge, then "confirmed"; a declined charge is
//     compensated by cancelling the order; recovery resolves stale pendings;
//   - outbox: the order and an outbox row commit together; a relay charges
//     idempotently and marks the row sent.
//
// A "crash" stops the flow at that point with nothing cleaned up, as if the
// process died.
package twosys

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Env struct{ Orders, Payments *pgxpool.Pool }

var errCrash = errors.New("crash")

type crasher struct {
	r     *rand.Rand
	p     float64
	count int
}

func (c *crasher) at() error {
	if c.r.Float64() < c.p {
		c.count++
		return errCrash
	}
	return nil
}

func (e Env) reset(ctx context.Context) error {
	// Roll back anything a previous run left prepared.
	for _, db := range []*pgxpool.Pool{e.Orders, e.Payments} {
		rows, err := db.Query(ctx, `SELECT gid FROM pg_prepared_xacts WHERE database = current_database()`)
		if err != nil {
			return err
		}
		gids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, g := range gids {
			if _, err := db.Exec(ctx, fmt.Sprintf(`ROLLBACK PREPARED '%s'`, g)); err != nil {
				return err
			}
		}
	}
	if _, err := e.Orders.Exec(ctx, `DROP SCHEMA IF EXISTS ts CASCADE; CREATE SCHEMA ts;
		CREATE TABLE ts.orders (id int PRIMARY KEY, status text NOT NULL, amount int NOT NULL, updated_at timestamptz NOT NULL DEFAULT now());
		CREATE TABLE ts.outbox (order_id int PRIMARY KEY, amount int NOT NULL, sent boolean NOT NULL DEFAULT false);
		CREATE TABLE ts.decisions (gid text PRIMARY KEY, commit boolean NOT NULL);`); err != nil {
		return err
	}
	_, err := e.Payments.Exec(ctx, `DROP TABLE IF EXISTS charges;
		CREATE TABLE charges (order_id int PRIMARY KEY, amount int NOT NULL)`)
	return err
}

// Result of running one strategy.
type Result struct {
	Strategy                  string
	Orders, Crashes, Declined int
	InconsistentBefore        int // after the crashes, before any recovery
	InDoubt                   int // prepared transactions left behind (2PC)
	PendingVisible            int // orders readers could see as "pending" (saga)
	StuckPending              int // saga orders still pending after the crashes, before recovery
	Redelivered               int // charges attempted more than once (outbox)
	InconsistentAfter         int // after recovery / the relay ran
	Took                      time.Duration
}

// inconsistent counts orders whose state disagrees with payments: a
// confirmed order with no charge, a charge for an order that isn't
// confirmed, or a charge for a missing order.
func (e Env) inconsistent(ctx context.Context) (int, error) {
	confirmed := map[int]bool{}
	rows, err := e.Orders.Query(ctx, `SELECT id FROM ts.orders WHERE status = 'confirmed'`)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		confirmed[id] = true
	}
	rows, err = e.Payments.Query(ctx, `SELECT order_id FROM charges`)
	if err != nil {
		return 0, err
	}
	charged, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return 0, err
	}
	bad := 0
	seen := map[int]bool{}
	for _, id := range charged {
		seen[id] = true
		if !confirmed[id] {
			bad++
		}
	}
	for id := range confirmed {
		if !seen[id] {
			bad++
		}
	}
	return bad, nil
}

func (e Env) countPrepared(ctx context.Context) (int, error) {
	n := 0
	for _, db := range []*pgxpool.Pool{e.Orders, e.Payments} {
		var c int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_prepared_xacts WHERE database = current_database()`).Scan(&c); err != nil {
			return 0, err
		}
		n += c
	}
	return n, nil
}

// Run executes one strategy for n orders with crash probability p at every
// crash point, and a decline probability for the charge.
func (e Env) Run(ctx context.Context, strategy string, n int, p, decline float64, seed uint64) (Result, error) {
	if err := e.reset(ctx); err != nil {
		return Result{}, err
	}
	c := &crasher{r: rand.New(rand.NewPCG(seed, 1)), p: p}
	// Whether a card is declined is a fact about the order: retrying the
	// same charge (same idempotency key) gets the same answer.
	dr := rand.New(rand.NewPCG(seed, 2))
	declines := map[int]bool{}
	for id := 1; id <= n; id++ {
		declines[id] = dr.Float64() < decline
	}
	res := Result{Strategy: strategy, Orders: n}
	t := time.Now()
	for id := 1; id <= n; id++ {
		amount := 10 + id%90
		declined := declines[id]
		if declined {
			res.Declined++
		}
		var err error
		switch strategy {
		case "dual write":
			err = e.dualWrite(ctx, c, id, amount, declined)
		case "two-phase commit":
			err = e.twoPhase(ctx, c, id, amount, declined)
		case "saga":
			err = e.saga(ctx, c, id, amount, declined, &res)
		case "outbox":
			err = e.outboxWrite(ctx, c, id, amount)
		}
		if err != nil && !errors.Is(err, errCrash) {
			return res, fmt.Errorf("%s order %d: %w", strategy, id, err)
		}
	}
	var err error
	if res.InconsistentBefore, err = e.inconsistent(ctx); err != nil {
		return res, err
	}
	if res.InDoubt, err = e.countPrepared(ctx); err != nil {
		return res, err
	}
	if err := e.Orders.QueryRow(ctx, `SELECT count(*) FROM ts.orders WHERE status = 'pending'`).Scan(&res.StuckPending); err != nil {
		return res, err
	}
	// Recovery, where the strategy has one.
	switch strategy {
	case "two-phase commit":
		err = e.recover2PC(ctx)
	case "saga":
		err = e.recoverSaga(ctx)
	case "outbox":
		// The relay is the recovery: it runs (and crashes) until the outbox is empty.
		res.Redelivered, err = e.relay(ctx, c, declines)
	}
	if err != nil {
		return res, err
	}
	if res.InconsistentAfter, err = e.inconsistent(ctx); err != nil {
		return res, err
	}
	res.Crashes = c.count // including the relay's, for the outbox
	res.Took = time.Since(t)
	return res, nil
}

// ---- dual write -----------------------------------------------------------------

func (e Env) dualWrite(ctx context.Context, c *crasher, id, amount int, declined bool) error {
	if declined {
		return nil // nothing to do: the order isn't placed
	}
	if _, err := e.Orders.Exec(ctx, `INSERT INTO ts.orders (id, status, amount) VALUES ($1, 'confirmed', $2)`, id, amount); err != nil {
		return err
	}
	if err := c.at(); err != nil { // between the two commits
		return err
	}
	_, err := e.Payments.Exec(ctx, `INSERT INTO charges VALUES ($1, $2)`, id, amount)
	return err
}

// ---- two-phase commit ---------------------------------------------------------------

func (e Env) twoPhase(ctx context.Context, c *crasher, id, amount int, declined bool) error {
	gid := fmt.Sprintf("order-%d", id)
	// Prepared-transaction IDs are unique per server, and both databases
	// live on one server here, so each side gets a suffix.
	prepare := func(db *pgxpool.Pool, side, q string, args ...any) error {
		conn, err := db.Acquire(ctx)
		if err != nil {
			return err
		}
		defer conn.Release()
		if _, err := conn.Exec(ctx, `BEGIN`); err != nil {
			return err
		}
		if _, err := conn.Exec(ctx, q, args...); err != nil {
			_, _ = conn.Exec(ctx, `ROLLBACK`)
			return err
		}
		_, err = conn.Exec(ctx, fmt.Sprintf(`PREPARE TRANSACTION '%s/%s'`, gid, side))
		return err
	}
	if err := prepare(e.Orders, "orders", `INSERT INTO ts.orders (id, status, amount) VALUES ($1, 'confirmed', $2)`, id, amount); err != nil {
		return err
	}
	if err := c.at(); err != nil { // after one prepare
		return err
	}
	commit := !declined // the payment side votes no on a declined card
	if commit {
		if err := prepare(e.Payments, "payments", `INSERT INTO charges VALUES ($1, $2)`, id, amount); err != nil {
			return err
		}
	}
	if err := c.at(); err != nil { // both prepared, decision not recorded
		return err
	}
	// The coordinator records its decision durably before telling anyone.
	if _, err := e.Orders.Exec(ctx, `INSERT INTO ts.decisions VALUES ($1, $2)`, gid, commit); err != nil {
		return err
	}
	if err := c.at(); err != nil { // decided, not yet finished
		return err
	}
	return e.finish(ctx, gid, commit)
}

func (e Env) finish(ctx context.Context, gid string, commit bool) error {
	verb := "ROLLBACK PREPARED"
	if commit {
		verb = "COMMIT PREPARED"
	}
	for side, db := range map[string]*pgxpool.Pool{"orders": e.Orders, "payments": e.Payments} {
		g := gid + "/" + side
		var exists bool
		if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_prepared_xacts WHERE gid = $1 AND database = current_database())`, g).Scan(&exists); err != nil {
			return err
		}
		if exists {
			if _, err := db.Exec(ctx, fmt.Sprintf(`%s '%s'`, verb, g)); err != nil {
				return err
			}
		}
	}
	return nil
}

// recover2PC: for every prepared transaction, the recorded decision wins;
// no decision recorded means the coordinator never decided, so abort.
func (e Env) recover2PC(ctx context.Context) error {
	gids := map[string]bool{}
	for _, db := range []*pgxpool.Pool{e.Orders, e.Payments} {
		rows, err := db.Query(ctx, `SELECT gid FROM pg_prepared_xacts WHERE database = current_database()`)
		if err != nil {
			return err
		}
		l, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, g := range l {
			base, _, _ := strings.Cut(g, "/")
			gids[base] = true
		}
	}
	for g := range gids {
		var commit bool
		err := e.Orders.QueryRow(ctx, `SELECT commit FROM ts.decisions WHERE gid = $1`, g).Scan(&commit)
		if errors.Is(err, pgx.ErrNoRows) {
			commit = false
		} else if err != nil {
			return err
		}
		if err := e.finish(ctx, g, commit); err != nil {
			return err
		}
	}
	return nil
}

// BlockedBy2PC shows what an in-doubt transaction does to everyone else:
// it prepares an order and leaves it, then times how long another session
// waits to update that order.
func (e Env) BlockedBy2PC(ctx context.Context, lockTimeout time.Duration) (waited time.Duration, err error) {
	if err := e.reset(ctx); err != nil {
		return 0, err
	}
	if _, err := e.Orders.Exec(ctx, `INSERT INTO ts.orders VALUES (1, 'pending', 10)`); err != nil {
		return 0, err
	}
	conn, err := e.Orders.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	_, _ = conn.Exec(ctx, `BEGIN`)
	_, _ = conn.Exec(ctx, `UPDATE ts.orders SET status = 'confirmed' WHERE id = 1`)
	_, err = conn.Exec(ctx, `PREPARE TRANSACTION 'in-doubt-1'`)
	conn.Release()
	if err != nil {
		return 0, err
	}
	defer e.Orders.Exec(context.Background(), `ROLLBACK PREPARED 'in-doubt-1'`)
	t := time.Now()
	err = pgx.BeginFunc(ctx, e.Orders, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL lock_timeout = '%dms'`, lockTimeout.Milliseconds())); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE ts.orders SET status = 'cancelled' WHERE id = 1`)
		return err
	})
	return time.Since(t), err
}

// ---- saga ------------------------------------------------------------------------------

func (e Env) saga(ctx context.Context, c *crasher, id, amount int, declined bool, res *Result) error {
	if _, err := e.Orders.Exec(ctx, `INSERT INTO ts.orders (id, status, amount) VALUES ($1, 'pending', $2)`, id, amount); err != nil {
		return err
	}
	res.PendingVisible++           // every saga order is visible as pending for a while
	if err := c.at(); err != nil { // before charging
		return err
	}
	if declined {
		// Compensate: the step that succeeded is undone by a new action.
		_, err := e.Orders.Exec(ctx, `UPDATE ts.orders SET status = 'cancelled' WHERE id = $1`, id)
		return err
	}
	if _, err := e.Payments.Exec(ctx, `INSERT INTO charges VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, amount); err != nil {
		return err
	}
	if err := c.at(); err != nil { // charged, order still pending
		return err
	}
	_, err := e.Orders.Exec(ctx, `UPDATE ts.orders SET status = 'confirmed' WHERE id = $1`, id)
	return err
}

// recoverSaga: a pending order is finished by asking the payment system
// what happened: charged -> confirm; not charged -> cancel.
func (e Env) recoverSaga(ctx context.Context) error {
	rows, err := e.Orders.Query(ctx, `SELECT id FROM ts.orders WHERE status = 'pending'`)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return err
	}
	for _, id := range ids {
		var charged bool
		if err := e.Payments.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM charges WHERE order_id = $1)`, id).Scan(&charged); err != nil {
			return err
		}
		status := "cancelled"
		if charged {
			status = "confirmed"
		}
		if _, err := e.Orders.Exec(ctx, `UPDATE ts.orders SET status = $2 WHERE id = $1 AND status = 'pending'`, id, status); err != nil {
			return err
		}
	}
	return nil
}

// ---- outbox -------------------------------------------------------------------------------

func (e Env) outboxWrite(ctx context.Context, c *crasher, id, amount int) error {
	// The order and the instruction to charge commit together, or not at all.
	return pgx.BeginFunc(ctx, e.Orders, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO ts.orders (id, status, amount) VALUES ($1, 'confirmed', $2)`, id, amount); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO ts.outbox (order_id, amount) VALUES ($1, $2)`, id, amount)
		return err
	})
}

// relay delivers outbox rows to payments: charge (idempotent on order_id),
// then mark sent. A crash between the two means the row is delivered again
// next time; the ON CONFLICT absorbs it. Declines cancel the order.
func (e Env) relay(ctx context.Context, c *crasher, declines map[int]bool) (redelivered int, err error) {
	attempts := map[int]int{}
	for {
		rows, err := e.Orders.Query(ctx, `SELECT order_id, amount FROM ts.outbox WHERE NOT sent ORDER BY order_id LIMIT 100`)
		if err != nil {
			return 0, err
		}
		type item struct{ id, amount int }
		batch, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (item, error) {
			var it item
			return it, r.Scan(&it.id, &it.amount)
		})
		if err != nil {
			return 0, err
		}
		if len(batch) == 0 {
			break
		}
		for _, it := range batch {
			attempts[it.id]++
			if attempts[it.id] == 2 {
				redelivered++
			}
			if declines[it.id] {
				_, err = e.Orders.Exec(ctx, `UPDATE ts.orders SET status = 'cancelled' WHERE id = $1`, it.id)
			} else {
				_, err = e.Payments.Exec(ctx, `INSERT INTO charges VALUES ($1, $2) ON CONFLICT (order_id) DO NOTHING`, it.id, it.amount)
			}
			if err != nil {
				return 0, err
			}
			if c.at() != nil { // the relay dies before marking the row sent
				break
			}
			if _, err := e.Orders.Exec(ctx, `UPDATE ts.outbox SET sent = true WHERE order_id = $1`, it.id); err != nil {
				return 0, err
			}
		}
	}
	return redelivered, nil
}

// Connect opens the orders database and creates and opens a separate
// payments database on the same server.
func Connect(ctx context.Context, url string) (Env, func(), error) {
	orders, err := pgxpool.New(ctx, url)
	if err != nil {
		return Env{}, nil, err
	}
	var exists bool
	if err := orders.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'payments')`).Scan(&exists); err != nil {
		return Env{}, nil, err
	}
	if !exists {
		if _, err := orders.Exec(ctx, `CREATE DATABASE payments`); err != nil {
			return Env{}, nil, err
		}
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return Env{}, nil, err
	}
	cfg.ConnConfig.Database = "payments"
	payments, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return Env{}, nil, err
	}
	e := Env{Orders: orders, Payments: payments}
	return e, func() {
		_ = e.reset(context.Background())
		_, _ = orders.Exec(context.Background(), `DROP SCHEMA IF EXISTS ts CASCADE`)
		payments.Close()
		orders.Close()
	}, nil
}
