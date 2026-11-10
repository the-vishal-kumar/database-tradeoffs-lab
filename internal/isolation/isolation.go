// Package isolation runs concurrency anomalies against PostgreSQL at each
// transaction isolation level and reports what actually happened.
//
// Every scenario drives two transactions, T1 and T2, on two separate
// connections, interleaving their statements in a fixed order. A third
// connection sets up tables and watches for lock waits. Nothing depends on
// timing except the two "FOR UPDATE" scenarios, which wait until T2 is
// visibly blocked on a lock before T1 continues.
package isolation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Level is a transaction isolation level as Postgres names it.
type Level struct {
	Name string
	Iso  pgx.TxIsoLevel
}

// Levels are the four SQL-standard levels, weakest first.
var Levels = []Level{
	{"Read Uncommitted", pgx.ReadUncommitted},
	{"Read Committed", pgx.ReadCommitted},
	{"Repeatable Read", pgx.RepeatableRead},
	{"Serializable", pgx.Serializable},
}

// Outcome is what happened when a scenario ran at one level.
type Outcome int

const (
	// Anomaly: both transactions committed and the result is wrong.
	Anomaly Outcome = iota
	// Prevented: both transactions committed and the result is right.
	Prevented
	// Aborted: Postgres refused one transaction with a serialization
	// failure (SQLSTATE 40001). The data is right; the app must retry.
	Aborted
)

func (o Outcome) String() string {
	switch o {
	case Anomaly:
		return "anomaly"
	case Prevented:
		return "prevented"
	case Aborted:
		return "aborted (40001), retry"
	}
	return "?"
}

// Result is one scenario at one level.
type Result struct {
	Outcome Outcome
	Detail  string // what the transactions saw, in one line
}

// Env holds the three connections a scenario uses.
type Env struct {
	C1, C2 *pgx.Conn // T1 and T2
	Obs    *pgx.Conn // setup, final reads and lock-wait checks (autocommit)
}

// Scenario is one anomaly (or one fix for an anomaly).
type Scenario struct {
	Key  string
	Name string
	Run  func(ctx context.Context, e Env, iso pgx.TxIsoLevel) (Result, error)
}

// Scenarios, in the order the post discusses them.
var Scenarios = []Scenario{
	{"dirty-read", "Dirty read", dirtyRead},
	{"non-repeatable-read", "Non-repeatable read", nonRepeatableRead},
	{"phantom-read", "Phantom read", phantomRead},
	{"lost-update", "Lost update (read, then write)", lostUpdate},
	{"lost-update-atomic", "Lost update, fixed with UPDATE v = v + 1", lostUpdateAtomic},
	{"lost-update-for-update", "Lost update, fixed with SELECT … FOR UPDATE", lostUpdateForUpdate},
	{"write-skew", "Write skew (on-call doctors)", writeSkew},
	{"write-skew-for-update", "Write skew, fixed with SELECT … FOR UPDATE", writeSkewForUpdate},
	{"lost-update-retry", "Lost update, with a retry on 40001", lostUpdateRetry},
	{"write-skew-retry", "Write skew, with a retry on 40001", writeSkewRetry},
}

// IsSerializationFailure reports whether err means "retry the transaction".
func IsSerializationFailure(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && (pe.Code == "40001" || pe.Code == "40P01")
}

// txn wraps a transaction and records whether Postgres aborted it, so a
// scenario can keep stepping through the other transaction.
type txn struct {
	tx      pgx.Tx
	aborted bool
}

func begin(ctx context.Context, c *pgx.Conn, iso pgx.TxIsoLevel) (*txn, error) {
	tx, err := c.BeginTx(ctx, pgx.TxOptions{IsoLevel: iso})
	if err != nil {
		return nil, err
	}
	return &txn{tx: tx}, nil
}

// step runs fn unless the transaction was already aborted. A serialization
// failure marks it aborted; any other error is returned.
func (t *txn) step(ctx context.Context, fn func(pgx.Tx) error) error {
	if t.aborted {
		return nil
	}
	if err := fn(t.tx); err != nil {
		if IsSerializationFailure(err) {
			t.aborted = true
			_ = t.tx.Rollback(ctx)
			return nil
		}
		return err
	}
	return nil
}

func (t *txn) exec(ctx context.Context, sql string, args ...any) error {
	return t.step(ctx, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, sql, args...); return err })
}

func (t *txn) queryInt(ctx context.Context, dst *int, sql string, args ...any) error {
	return t.step(ctx, func(tx pgx.Tx) error { return tx.QueryRow(ctx, sql, args...).Scan(dst) })
}

func (t *txn) commit(ctx context.Context) error {
	return t.step(ctx, func(tx pgx.Tx) error { return tx.Commit(ctx) })
}

func (t *txn) rollback(ctx context.Context) { _ = t.tx.Rollback(ctx) }

func setup(ctx context.Context, e Env, sql string) error {
	_, err := e.Obs.Exec(ctx, sql) // no arguments: simple protocol, several statements allowed
	return err
}

func readInt(ctx context.Context, e Env, sql string) (int, error) {
	var n int
	err := e.Obs.QueryRow(ctx, sql).Scan(&n)
	return n, err
}

// waitForLock returns once the backend behind c is waiting on a lock.
func waitForLock(ctx context.Context, e Env, c *pgx.Conn) error {
	pid := c.PgConn().PID()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		err := e.Obs.QueryRow(ctx,
			`SELECT coalesce(wait_event_type = 'Lock', false) FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&waiting)
		if err != nil {
			return err
		}
		if waiting {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("backend %d never waited on a lock", pid)
}

// ---- dirty read ----

// T1 changes a row and doesn't commit; can T2 see the change?
func dirtyRead(ctx context.Context, e Env, iso pgx.TxIsoLevel) (Result, error) {
	if err := setup(ctx, e, `DROP TABLE IF EXISTS accounts;
		CREATE TABLE accounts (id int PRIMARY KEY, balance int NOT NULL);
		INSERT INTO accounts VALUES (1, 100);`); err != nil {
		return Result{}, err
	}
	t1, err := begin(ctx, e.C1, iso)
	if err != nil {
		return Result{}, err
	}
	defer t1.rollback(ctx)
	t2, err := begin(ctx, e.C2, iso)
	if err != nil {
		return Result{}, err
	}
	defer t2.rollback(ctx)

	var seen int
	if err := t1.exec(ctx, `UPDATE accounts SET balance = 0 WHERE id = 1`); err != nil {
		return Result{}, err
	}
	if err := t2.queryInt(ctx, &seen, `SELECT balance FROM accounts WHERE id = 1`); err != nil {
		return Result{}, err
	}
	t1.rollback(ctx)
	if err := t2.commit(ctx); err != nil {
		return Result{}, err
	}
	if seen == 0 {
		return Result{Anomaly, "T2 read T1's uncommitted balance 0"}, nil
	}
	return Result{Prevented, fmt.Sprintf("T2 read %d, the committed balance", seen)}, nil
}

// ---- non-repeatable read ----

// T1 reads a row twice; another session updates it in between.
func nonRepeatableRead(ctx context.Context, e Env, iso pgx.TxIsoLevel) (Result, error) {
	if err := setup(ctx, e, `DROP TABLE IF EXISTS accounts;
		CREATE TABLE accounts (id int PRIMARY KEY, balance int NOT NULL);
		INSERT INTO accounts VALUES (1, 100);`); err != nil {
		return Result{}, err
	}
	t1, err := begin(ctx, e.C1, iso)
	if err != nil {
		return Result{}, err
	}
	defer t1.rollback(ctx)

	var first, second int
	if err := t1.queryInt(ctx, &first, `SELECT balance FROM accounts WHERE id = 1`); err != nil {
		return Result{}, err
	}
	if _, err := e.C2.Exec(ctx, `UPDATE accounts SET balance = 50 WHERE id = 1`); err != nil {
		return Result{}, err
	}
	if err := t1.queryInt(ctx, &second, `SELECT balance FROM accounts WHERE id = 1`); err != nil {
		return Result{}, err
	}
	if err := t1.commit(ctx); err != nil {
		return Result{}, err
	}
	detail := fmt.Sprintf("T1 read %d, then %d", first, second)
	if first != second {
		return Result{Anomaly, detail}, nil
	}
	return Result{Prevented, detail}, nil
}

// ---- phantom read ----

// T1 counts rows matching a condition twice; another session inserts a
// matching row in between.
func phantomRead(ctx context.Context, e Env, iso pgx.TxIsoLevel) (Result, error) {
	if err := setup(ctx, e, `DROP TABLE IF EXISTS orders;
		CREATE TABLE orders (id serial PRIMARY KEY, tenant text NOT NULL, amount int NOT NULL);
		INSERT INTO orders (tenant, amount) VALUES ('acme', 10), ('acme', 20);`); err != nil {
		return Result{}, err
	}
	t1, err := begin(ctx, e.C1, iso)
	if err != nil {
		return Result{}, err
	}
	defer t1.rollback(ctx)

	var first, second int
	q := `SELECT count(*) FROM orders WHERE tenant = 'acme'`
	if err := t1.queryInt(ctx, &first, q); err != nil {
		return Result{}, err
	}
	if _, err := e.C2.Exec(ctx, `INSERT INTO orders (tenant, amount) VALUES ('acme', 30)`); err != nil {
		return Result{}, err
	}
	if err := t1.queryInt(ctx, &second, q); err != nil {
		return Result{}, err
	}
	if err := t1.commit(ctx); err != nil {
		return Result{}, err
	}
	detail := fmt.Sprintf("T1 counted %d rows, then %d", first, second)
	if first != second {
		return Result{Anomaly, detail}, nil
	}
	return Result{Prevented, detail}, nil
}

// ---- lost update ----

const counterSetup = `DROP TABLE IF EXISTS counters;
	CREATE TABLE counters (id int PRIMARY KEY, v int NOT NULL);
	INSERT INTO counters VALUES (1, 0);`

// finishCounter reads the final value: two increments should make it 2.
func finishCounter(ctx context.Context, e Env, t1, t2 *txn, wrong string) (Result, error) {
	final, err := readInt(ctx, e, `SELECT v FROM counters WHERE id = 1`)
	if err != nil {
		return Result{}, err
	}
	switch {
	case t1.aborted || t2.aborted:
		return Result{Aborted, fmt.Sprintf("one transaction was aborted; v = %d", final)}, nil
	case final == 2:
		return Result{Prevented, "both increments kept; v = 2"}, nil
	default:
		return Result{Anomaly, fmt.Sprintf("%s; v = %d", wrong, final)}, nil
	}
}

// Both transactions read the counter, add one in application code, and
// write the result back.
func lostUpdate(ctx context.Context, e Env, iso pgx.TxIsoLevel) (Result, error) {
	if err := setup(ctx, e, counterSetup); err != nil {
		return Result{}, err
	}
	t1, err := begin(ctx, e.C1, iso)
	if err != nil {
		return Result{}, err
	}
	defer t1.rollback(ctx)
	t2, err := begin(ctx, e.C2, iso)
	if err != nil {
		return Result{}, err
	}
	defer t2.rollback(ctx)

	var v1, v2 int
	for _, s := range []func() error{
		func() error { return t1.queryInt(ctx, &v1, `SELECT v FROM counters WHERE id = 1`) },
		func() error { return t2.queryInt(ctx, &v2, `SELECT v FROM counters WHERE id = 1`) },
		func() error { return t1.exec(ctx, `UPDATE counters SET v = $1 WHERE id = 1`, v1+1) },
		func() error { return t1.commit(ctx) },
		func() error { return t2.exec(ctx, `UPDATE counters SET v = $1 WHERE id = 1`, v2+1) },
		func() error { return t2.commit(ctx) },
	} {
		if err := s(); err != nil {
			return Result{}, err
		}
	}
	return finishCounter(ctx, e, t1, t2, "T2 overwrote T1's increment")
}

// The increment happens in SQL, so T2's UPDATE works on the latest row.
// T2 reads first only so that its snapshot is taken before T1 commits.
func lostUpdateAtomic(ctx context.Context, e Env, iso pgx.TxIsoLevel) (Result, error) {
	if err := setup(ctx, e, counterSetup); err != nil {
		return Result{}, err
	}
	t1, err := begin(ctx, e.C1, iso)
	if err != nil {
		return Result{}, err
	}
	defer t1.rollback(ctx)
	t2, err := begin(ctx, e.C2, iso)
	if err != nil {
		return Result{}, err
	}
	defer t2.rollback(ctx)

	var ignore int
	for _, s := range []func() error{
		func() error { return t1.queryInt(ctx, &ignore, `SELECT v FROM counters WHERE id = 1`) },
		func() error { return t2.queryInt(ctx, &ignore, `SELECT v FROM counters WHERE id = 1`) },
		func() error { return t1.exec(ctx, `UPDATE counters SET v = v + 1 WHERE id = 1`) },
		func() error { return t1.commit(ctx) },
		func() error { return t2.exec(ctx, `UPDATE counters SET v = v + 1 WHERE id = 1`) },
		func() error { return t2.commit(ctx) },
	} {
		if err := s(); err != nil {
			return Result{}, err
		}
	}
	return finishCounter(ctx, e, t1, t2, "an increment was lost")
}

// Both transactions lock the row before reading it. T2 waits for T1.
func lostUpdateForUpdate(ctx context.Context, e Env, iso pgx.TxIsoLevel) (Result, error) {
	if err := setup(ctx, e, counterSetup); err != nil {
		return Result{}, err
	}
	t1, err := begin(ctx, e.C1, iso)
	if err != nil {
		return Result{}, err
	}
	defer t1.rollback(ctx)
	t2, err := begin(ctx, e.C2, iso)
	if err != nil {
		return Result{}, err
	}
	defer t2.rollback(ctx)

	// Take T2's snapshot before T1 changes anything, as in the other scenarios.
	var ignore, v1, v2 int
	if err := t2.queryInt(ctx, &ignore, `SELECT 1`); err != nil {
		return Result{}, err
	}
	if err := t1.queryInt(ctx, &v1, `SELECT v FROM counters WHERE id = 1 FOR UPDATE`); err != nil {
		return Result{}, err
	}
	done := make(chan error, 1)
	go func() { done <- t2.queryInt(ctx, &v2, `SELECT v FROM counters WHERE id = 1 FOR UPDATE`) }()
	if err := waitForLock(ctx, e, e.C2); err != nil {
		return Result{}, err
	}
	if err := t1.exec(ctx, `UPDATE counters SET v = $1 WHERE id = 1`, v1+1); err != nil {
		return Result{}, err
	}
	if err := t1.commit(ctx); err != nil {
		return Result{}, err
	}
	if err := <-done; err != nil {
		return Result{}, err
	}
	if err := t2.exec(ctx, `UPDATE counters SET v = $1 WHERE id = 1`, v2+1); err != nil {
		return Result{}, err
	}
	if err := t2.commit(ctx); err != nil {
		return Result{}, err
	}
	return finishCounter(ctx, e, t1, t2, "an increment was lost")
}

// ---- write skew ----

const doctorsSetup = `DROP TABLE IF EXISTS doctors;
	CREATE TABLE doctors (name text PRIMARY KEY, on_call boolean NOT NULL);
	INSERT INTO doctors VALUES ('alice', true), ('bob', true);`

// Rule: at least one doctor must stay on call. Each transaction checks the
// rule, sees two doctors on call, and takes a different doctor off call.
// Neither touches the row the other one changes.
func writeSkew(ctx context.Context, e Env, iso pgx.TxIsoLevel) (Result, error) {
	if err := setup(ctx, e, doctorsSetup); err != nil {
		return Result{}, err
	}
	t1, err := begin(ctx, e.C1, iso)
	if err != nil {
		return Result{}, err
	}
	defer t1.rollback(ctx)
	t2, err := begin(ctx, e.C2, iso)
	if err != nil {
		return Result{}, err
	}
	defer t2.rollback(ctx)

	q := `SELECT count(*) FROM doctors WHERE on_call`
	var n1, n2 int
	for _, s := range []func() error{
		func() error { return t1.queryInt(ctx, &n1, q) },
		func() error { return t2.queryInt(ctx, &n2, q) },
		func() error {
			if n1 < 2 {
				return nil
			}
			return t1.exec(ctx, `UPDATE doctors SET on_call = false WHERE name = 'alice'`)
		},
		func() error {
			if n2 < 2 {
				return nil
			}
			return t2.exec(ctx, `UPDATE doctors SET on_call = false WHERE name = 'bob'`)
		},
		func() error { return t1.commit(ctx) },
		func() error { return t2.commit(ctx) },
	} {
		if err := s(); err != nil {
			return Result{}, err
		}
	}
	return finishDoctors(ctx, e, t1, t2)
}

// Each transaction locks the on-call rows before counting them. At Read
// Committed, T2 waits, then re-checks the rows and sees only Bob on call.
func writeSkewForUpdate(ctx context.Context, e Env, iso pgx.TxIsoLevel) (Result, error) {
	if err := setup(ctx, e, doctorsSetup); err != nil {
		return Result{}, err
	}
	t1, err := begin(ctx, e.C1, iso)
	if err != nil {
		return Result{}, err
	}
	defer t1.rollback(ctx)
	t2, err := begin(ctx, e.C2, iso)
	if err != nil {
		return Result{}, err
	}
	defer t2.rollback(ctx)

	// Count the locked rows in a subquery: FOR UPDATE can't sit next to count(*).
	q := `SELECT count(*) FROM (SELECT name FROM doctors WHERE on_call FOR UPDATE) AS locked`
	var ignore, n1, n2 int
	if err := t2.queryInt(ctx, &ignore, `SELECT 1`); err != nil {
		return Result{}, err
	}
	if err := t1.queryInt(ctx, &n1, q); err != nil {
		return Result{}, err
	}
	done := make(chan error, 1)
	go func() { done <- t2.queryInt(ctx, &n2, q) }()
	if err := waitForLock(ctx, e, e.C2); err != nil {
		return Result{}, err
	}
	if n1 >= 2 {
		if err := t1.exec(ctx, `UPDATE doctors SET on_call = false WHERE name = 'alice'`); err != nil {
			return Result{}, err
		}
	}
	if err := t1.commit(ctx); err != nil {
		return Result{}, err
	}
	if err := <-done; err != nil {
		return Result{}, err
	}
	if !t2.aborted && n2 >= 2 {
		if err := t2.exec(ctx, `UPDATE doctors SET on_call = false WHERE name = 'bob'`); err != nil {
			return Result{}, err
		}
	}
	if err := t2.commit(ctx); err != nil {
		return Result{}, err
	}
	return finishDoctors(ctx, e, t1, t2)
}

func finishDoctors(ctx context.Context, e Env, t1, t2 *txn) (Result, error) {
	onCall, err := readInt(ctx, e, `SELECT count(*) FROM doctors WHERE on_call`)
	if err != nil {
		return Result{}, err
	}
	switch {
	case t1.aborted || t2.aborted:
		return Result{Aborted, fmt.Sprintf("one transaction was aborted; %d doctor(s) on call", onCall)}, nil
	case onCall == 0:
		return Result{Anomaly, "both committed; nobody is on call"}, nil
	default:
		return Result{Prevented, fmt.Sprintf("both committed; %d doctor(s) on call", onCall)}, nil
	}
}

// ---- retrying serialization failures ----

// work is one transaction's logic in two phases, so a scenario can overlap
// T1 and T2: both read, then both write. State flows from read to write
// through the closures; a retry runs both phases again from scratch.
type work struct {
	read, write func(*txn) error
}

// retry runs w in a fresh transaction until it commits or fails with
// something other than a serialization failure. Real code would add
// jittered backoff; three attempts are plenty for two transactions.
func retry(ctx context.Context, c *pgx.Conn, iso pgx.TxIsoLevel, w work) (int, error) {
	for attempt := 1; attempt <= 3; attempt++ {
		t, err := begin(ctx, c, iso)
		if err != nil {
			return attempt, err
		}
		for _, phase := range []func(*txn) error{w.read, w.write} {
			if err := phase(t); err != nil {
				t.rollback(ctx)
				return attempt, err
			}
		}
		if err := t.commit(ctx); err != nil {
			return attempt, err
		}
		if !t.aborted {
			return attempt, nil
		}
	}
	return 3, errors.New("still failing after 3 attempts")
}

// The read-then-write increment from lostUpdate, with the aborted
// transaction run again.
func lostUpdateRetry(ctx context.Context, e Env, iso pgx.TxIsoLevel) (Result, error) {
	if err := setup(ctx, e, counterSetup); err != nil {
		return Result{}, err
	}
	increment := func() work {
		var v int
		return work{
			read:  func(t *txn) error { return t.queryInt(ctx, &v, `SELECT v FROM counters WHERE id = 1`) },
			write: func(t *txn) error { return t.exec(ctx, `UPDATE counters SET v = $1 WHERE id = 1`, v+1) },
		}
	}
	return overlapAndRetry(ctx, e, iso, increment(), increment(), func() (Result, error) {
		v, err := readInt(ctx, e, `SELECT v FROM counters WHERE id = 1`)
		if err != nil || v == 2 {
			return Result{Prevented, "v = 2"}, err
		}
		return Result{Anomaly, fmt.Sprintf("v = %d", v)}, nil
	})
}

// The on-call check, with the aborted transaction run again. On retry it
// counts again, sees one doctor on call, and leaves them on call.
func writeSkewRetry(ctx context.Context, e Env, iso pgx.TxIsoLevel) (Result, error) {
	if err := setup(ctx, e, doctorsSetup); err != nil {
		return Result{}, err
	}
	goOffCall := func(name string) work {
		var n int
		return work{
			read: func(t *txn) error { return t.queryInt(ctx, &n, `SELECT count(*) FROM doctors WHERE on_call`) },
			write: func(t *txn) error {
				if n < 2 {
					return nil
				}
				return t.exec(ctx, `UPDATE doctors SET on_call = false WHERE name = $1`, name)
			},
		}
	}
	return overlapAndRetry(ctx, e, iso, goOffCall("alice"), goOffCall("bob"), func() (Result, error) {
		n, err := readInt(ctx, e, `SELECT count(*) FROM doctors WHERE on_call`)
		if err != nil || n >= 1 {
			return Result{Prevented, fmt.Sprintf("%d doctor(s) on call", n)}, err
		}
		return Result{Anomaly, "nobody is on call"}, nil
	})
}

// overlapAndRetry runs T1 read, T2 read, T1 write + commit, T2 write +
// commit, then retries whichever transaction Postgres aborted.
func overlapAndRetry(ctx context.Context, e Env, iso pgx.TxIsoLevel, w1, w2 work, check func() (Result, error)) (Result, error) {
	t1, err := begin(ctx, e.C1, iso)
	if err != nil {
		return Result{}, err
	}
	defer t1.rollback(ctx)
	t2, err := begin(ctx, e.C2, iso)
	if err != nil {
		return Result{}, err
	}
	defer t2.rollback(ctx)
	for _, s := range []func() error{
		func() error { return w1.read(t1) },
		func() error { return w2.read(t2) },
		func() error { return w1.write(t1) },
		func() error { return t1.commit(ctx) },
		func() error { return w2.write(t2) },
		func() error { return t2.commit(ctx) },
	} {
		if err := s(); err != nil {
			return Result{}, err
		}
	}
	retried := ""
	for i, x := range []struct {
		t *txn
		c *pgx.Conn
		w work
	}{{t1, e.C1, w1}, {t2, e.C2, w2}} {
		if !x.t.aborted {
			continue
		}
		n, err := retry(ctx, x.c, iso, x.w)
		if err != nil {
			return Result{}, err
		}
		retried += fmt.Sprintf("T%d aborted, committed on attempt %d; ", i+1, n+1)
	}
	r, err := check()
	r.Detail = retried + r.Detail
	return r, err
}
