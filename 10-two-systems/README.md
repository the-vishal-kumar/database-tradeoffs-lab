# 10. Keeping two systems consistent without a distributed transaction

**Question:** How do you keep two systems consistent without distributed transactions?

```bash
make twosys
# or: docker compose up -d --wait postgres && docker compose run --rm go go run ./cmd/twosys
```

An order lives in one database (`lab`) and its charge in another (`payments`). 1,000 orders go through each strategy, with a 5% chance of a crash at every point where a crash would hurt, and 5% of cards declined. A "crash" stops the flow with nothing cleaned up. Results: [results.md](results.md).

| Strategy | Flow | Recovery |
|---|---|---|
| Dual write | Commit the order, then the charge | None |
| Two-phase commit | `PREPARE TRANSACTION` in both, record the decision, `COMMIT PREPARED` in both | Finish every prepared transaction by the recorded decision; no decision means abort |
| Saga | Order `pending` → charge → `confirmed`; a declined card is compensated by cancelling the order | For every stale `pending` order, ask payments: charged → confirm, not charged → cancel |
| Outbox | The order and an outbox row commit together; a relay charges (idempotent on the order ID), then marks the row sent | The relay itself: it retries anything not marked sent |

[`internal/twosys/twosys_test.go`](../internal/twosys/twosys_test.go) checks: every dual-write crash leaves an inconsistency; two-phase commit is never visibly inconsistent but leaves in-doubt transactions until recovery; the saga leaves stuck `pending` orders until recovery; the outbox ends consistent with its redeliveries absorbed; and an in-doubt transaction blocks other writers on its rows.

## Notes

- `max_prepared_transactions` is 0 by default in PostgreSQL, which disables `PREPARE TRANSACTION`; compose sets it for this demo.
- Prepared transactions survive a server restart and hold their locks until someone commits or rolls them back. Monitor `pg_prepared_xacts`.
- The charge is idempotent on the order ID (`ON CONFLICT DO NOTHING`), standing in for a payment provider's idempotency key.
