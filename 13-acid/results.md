# Results: transfers with and without transactions

Measured on 2026-11-10 with `make acid` on a laptop: PostgreSQL 17, MongoDB 8.2 as a one-node replica set, and DynamoDB Local 3.3.2, all in Docker. Crashes and transfers come from fixed seeds, so the counts repeat closely; readers race with writers, so their counts vary.

An earlier run read all 100 accounts in one MongoDB batch and one DynamoDB page: with transactional writers, those plain reads saw 0 wrong totals out of 1,908 and 1,813, because a read that fits in one batch happened to see one moment. Neither store promises that across documents or items, and on any collection bigger than one batch the reader sees several moments, so the run below pages in batches of 10. DynamoDB Local is an emulator: it follows the API's transaction semantics, not real DynamoDB's latency or capacity.

100 accounts × 1,000 = 100,000 in total. 8 workers x 1,000 transfers (8,000 total), each of 1 to 100; 2% of transfers crash half-way. A reader adds up all balances, over and over, while they run.

| Store | Transfers | Declined (no funds) | Crashed | Conflicts retried | Final total | Negative balances |
|---|---:|---:|---:|---:|---:|---:|
| PostgreSQL, two separate UPDATEs | 7809 | 63 | 128 | 0 | **93731** | 0 |
| PostgreSQL, one transaction | 7817 | 55 | 128 | 0 | **100000** | 0 |
| MongoDB, two separate updateOne calls | 7812 | 60 | 128 | 0 | **93731** | 0 |
| MongoDB, multi-document transaction | 7820 | 52 | 128 | 831 | **100000** | 0 |
| DynamoDB, two separate UpdateItem calls | 7813 | 59 | 128 | 0 | **93731** | 0 |
| DynamoDB, TransactWriteItems | 7820 | 52 | 128 | 0 | **100000** | 0 |

### What a reader sees while transfers run (no crashes, 4000 transfers)

| Writers | Reader | Totals read | Totals that weren't 100,000 |
|---|---|---:|---:|
| PostgreSQL, two separate UPDATEs | `SELECT sum(balance)` (one statement = one snapshot) | 3050 | **2932** |
| PostgreSQL, one transaction | `SELECT sum(balance)` (one statement = one snapshot) | 2362 | **0** |
| MongoDB, two separate updateOne calls | `find()` over all accounts, in batches of 10 | 655 | **609** |
| MongoDB, two separate updateOne calls | `find()` in a transaction with `readConcern: snapshot` | 317 | **312** |
| MongoDB, multi-document transaction | `find()` over all accounts, in batches of 10 | 467 | **418** |
| MongoDB, multi-document transaction | `find()` in a transaction with `readConcern: snapshot` | 299 | **0** |
| DynamoDB, two separate UpdateItem calls | `Scan` with `ConsistentRead`, pages of 10 | 379 | **378** |
| DynamoDB, two separate UpdateItem calls | `TransactGetItems` (all 100 items) | 165 | **162** |
| DynamoDB, TransactWriteItems | `Scan` with `ConsistentRead`, pages of 10 | 182 | **181** |
| DynamoDB, TransactWriteItems | `TransactGetItems` (all 100 items) | 42 | **0** |
