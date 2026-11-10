# Results: PostgreSQL vs a DynamoDB single-table design

Measured on 2026-11-10 with `make pgdyn` on a laptop, everything in Docker. **DynamoDB Local is a Java emulator reached over HTTP: its latencies say nothing about real DynamoDB** (single-digit milliseconds for a well-designed query). The items read and read capacity units (RCU) follow DynamoDB's documented accounting and carry over to the real service.

200000 orders from 1,000 customers over 2026, loaded into both in 47 s. PostgreSQL: one table, index on (customer_id, created_at). DynamoDB Local: one table, pk = CUSTOMER#id, sk = ORDER#timestamp#id.

| Query | Store | p50 | Rows returned | Items read | Read capacity consumed | Note |
|---|---|---:|---:|---:|---:|---|
| a customer's 20 latest orders | PostgreSQL | 0.11 ms | 20 | — | — |  |
| a customer's 20 latest orders | DynamoDB | 3.19 ms | 20 | 20 | 0.5 RCU |  |
| report: paid orders over 900 in March, all customers | PostgreSQL | 13 ms | 401 | — | — | no index for it: a sequential scan, planned automatically |
| report: paid orders over 900 in March, all customers | DynamoDB | 23560 ms | 401 | 200000 | 2689.0 RCU | Scan of the whole table, filter applied after reading |
| latest 20 orders with status 'placed', all customers | PostgreSQL | 0.13 ms | 20 | — | — | after `CREATE INDEX` (257 ms) |
| latest 20 orders with status 'placed', all customers | DynamoDB | 1.92 ms | 20 | 20 | 0.5 RCU | after adding a GSI (status, created): 12.8 s locally; in AWS, a backfill of every item |
