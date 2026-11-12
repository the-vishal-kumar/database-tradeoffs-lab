# 14. Postgres or DynamoDB for a new SaaS?

**Question:** Postgres or DynamoDB for a new SaaS?

```bash
make pgdyn
# or:
docker compose --profile acid up -d --wait postgres dynamodb
docker compose run --rm go go run ./cmd/pgdyn
# flag: -orders 200000
```

The same 200,000 orders (1,000 customers, five statuses, a year of dates) in PostgreSQL (one table, an index on `(customer_id, created_at)`) and in DynamoDB Local (one table designed for the known access pattern: `pk = CUSTOMER#id`, `sk = ORDER#timestamp#id`). Three queries:

1. **The access pattern both were designed for:** a customer's 20 latest orders.
2. **An ad-hoc report nobody planned:** paid orders over 900 in March, across all customers.
3. **A new access pattern added later:** the latest orders with a given status, across customers, after adding an index (PostgreSQL) or a global secondary index (DynamoDB).

Results: [results.md](results.md). [`internal/pgdyn/pgdyn_test.go`](../internal/pgdyn/pgdyn_test.go) checks that the designed query reads 20 items, the ad-hoc report scans the whole table in DynamoDB but returns the same rows as PostgreSQL, and both answer the new pattern after its index. It needs the `acid` profile (`make pgdyn-test`), so CI skips it.

## Notes

- DynamoDB Local latencies are not representative; compare items read and capacity units.
- A global secondary index is a second copy of the projected attributes: every write to the table also writes the index (extra write capacity), and the index is eventually consistent with the table.
