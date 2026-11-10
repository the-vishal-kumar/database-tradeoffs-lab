# 01 — Isolation levels: Read Committed vs Repeatable Read vs Serializable

Companion to the post *"Read Committed vs Repeatable Read vs Serializable: what each one actually lets through"*.

```bash
make isolation
```

This runs ten scenarios at each of Postgres's four isolation levels and prints what happened. Each scenario drives two transactions, T1 and T2, on separate connections and interleaves their statements in a fixed order, so the result is the same on every run (the tests run the whole matrix five times to check that).

| Scenario | What it does |
|---|---|
| Dirty read | T1 updates a balance and doesn't commit; T2 reads it |
| Non-repeatable read | T1 reads a balance twice; another session updates it in between |
| Phantom read | T1 counts a tenant's orders twice; another session inserts one in between |
| Lost update | T1 and T2 both read a counter, add 1 in application code, write it back |
| …fixed with `v = v + 1` | Same, but the increment happens in SQL |
| …fixed with `FOR UPDATE` | Same, but each locks the row before reading; T2 waits for T1 |
| Write skew | Two on-call doctors; each transaction checks "two are on call" and takes a *different* doctor off call |
| …fixed with `FOR UPDATE` | Same, but each locks the on-call rows before counting |
| Lost update, with retry | The lost update, with any aborted transaction run again |
| Write skew, with retry | The write skew, with any aborted transaction run again |

Outcomes:

- **anomaly:** both transactions committed and the data is wrong.
- **prevented:** both committed and the data is right.
- **aborted (40001), retry:** Postgres refused one transaction with a serialization failure. The data is right, but the application has to retry.

The measured matrix is in [results.md](results.md). The code is in [`internal/isolation`](../internal/isolation/isolation.go), and [`isolation_test.go`](../internal/isolation/isolation_test.go) pins every cell, so a Postgres upgrade that changes behaviour fails CI.

## Scope

- **PostgreSQL 17 only.** Other databases use the same level names for different behaviour. MySQL InnoDB, for example, defaults to Repeatable Read and implements it differently. Nothing here measures MySQL; don't read the matrix as applying to it.
- **Two transactions, one interleaving each.** Real contention has more transactions and other orderings. The point is to show what each level permits, not to benchmark it.
