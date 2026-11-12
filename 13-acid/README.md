# 13. What ACID buys: transfers in three databases

**Question:** What does ACID actually promise, and what do you lose with BASE?

```bash
make acid
# or:
docker compose --profile acid up -d --wait postgres mongors dynamodb
docker compose run --rm go go run ./cmd/acid
```

100 accounts holding 1,000 each. 8 workers x 1,000 transfers (8,000 total), each a random amount of 1 to 100 between two accounts; a transfer debits only if the balance covers it. 2% of transfers "crash" half-way (after the debit). Then, without crashes, a reader adds up every balance over and over while transfers run. Results: [results.md](results.md).

| Store | Without a transaction | With a transaction | Plain read | Snapshot read |
|---|---|---|---|---|
| PostgreSQL 17 | Two `UPDATE`s | One transaction (deadlocks retried) | `SELECT sum(balance)`: one statement is one snapshot | - |
| MongoDB 8.2 (one-node replica set: transactions need one) | Two `updateOne` | `session.withTransaction` | `find()` in batches of 10 | `find()` in a transaction with `readConcern: snapshot` |
| DynamoDB Local 3.3.2 | Two `UpdateItem` | `TransactWriteItems` | `Scan` with `ConsistentRead`, pages of 10 | `TransactGetItems` (up to 100 items) |

[`internal/acid/acid_test.go`](../internal/acid/acid_test.go) checks: without transactions crashes lose money; with them the total holds and nothing goes negative; with transactional writers a paged plain read sees transfers half-done and a snapshot read never does. It needs the `acid` profile running (`make acid-test`), so CI skips it.

## Notes

- Durability (the D) isn't tested here: that's the fsync story in the write-ahead log demo of [systems-data-structures](https://github.com/the-vishal-kumar/systems-data-structures).
- PostgreSQL deadlocks when two transfers between the same accounts run in opposite directions; the transaction is retried. Locking the lower account id first avoids it.
- `TransactGetItems` can be refused when it conflicts with a write in progress; those reads are counted as refused, not as answers.
