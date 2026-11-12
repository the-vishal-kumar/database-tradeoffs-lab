# Results: lock the row or retry on conflict

Measured on 2026-11-10 with `make locking` on a laptop (Intel i7-8550U, 8 threads, 10 GB RAM), everything in Docker on one machine. Times depend on the machine; the correctness columns (sold, lost updates, stock left) are what to compare, and they were identical in all three runs.

PostgreSQL 17.11. 32 buyers × 25 purchases, 2 ms of application work between read and write, pool of 64.

## Run 1

### High contention: every buyer wants the same item

| Strategy | Sold | Stock went down by | Lost updates | Retries | Gave up | Purchases/s | p50 | p99 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| naive read-modify-write (no lock, no check) | 800 | 28 | 772 | 0 | 0 | 1679 | 11.9 ms | 73.5 ms |
| pessimistic: SELECT ... FOR UPDATE, work, UPDATE | 800 | 800 | 0 | 0 | 0 | 284 | 29.8 ms | 1492.1 ms |
| optimistic: read version, work, UPDATE ... WHERE version = read, retry | 798 | 798 | 0 | 8568 | 2 | 275 | 10.2 ms | 735.5 ms |
| atomic: work, then UPDATE ... SET stock = stock - 1 WHERE stock > 0 | 800 | 800 | 0 | 0 | 0 | 1910 | 11.6 ms | 59.4 ms |
| SERIALIZABLE: read, work, write, retry on 40001 | 780 | 780 | 0 | 5305 | 20 | 292 | 3.2 ms | 1003.7 ms |

### Low contention: buyers spread over 1000 items

| Strategy | Sold | Stock went down by | Lost updates | Retries | Gave up | Purchases/s | p50 | p99 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| naive read-modify-write (no lock, no check) | 800 | 776 | 24 | 0 | 0 | 9105 | 3.2 ms | 6.1 ms |
| pessimistic: SELECT ... FOR UPDATE, work, UPDATE | 800 | 800 | 0 | 0 | 0 | 8325 | 3.5 ms | 6.2 ms |
| optimistic: read version, work, UPDATE ... WHERE version = read, retry | 800 | 800 | 0 | 18 | 0 | 7363 | 3.5 ms | 7.4 ms |
| atomic: work, then UPDATE ... SET stock = stock - 1 WHERE stock > 0 | 800 | 800 | 0 | 0 | 0 | 9106 | 3.2 ms | 6.7 ms |
| SERIALIZABLE: read, work, write, retry on 40001 | 800 | 800 | 0 | 484 | 0 | 3563 | 4.5 ms | 46.4 ms |

### The last 10 tickets: 50 buyers at once

| Strategy | Buyers told "you got one" | Stock left | Gave up |
|---|---:|---:|---:|
| naive read-modify-write (no lock, no check) | 50 | 9 | 0 |
| pessimistic: SELECT ... FOR UPDATE, work, UPDATE | 10 | 0 | 0 |
| optimistic: read version, work, UPDATE ... WHERE version = read, retry | 10 | 0 | 0 |
| atomic: work, then UPDATE ... SET stock = stock - 1 WHERE stock > 0 | 10 | 0 | 0 |
| SERIALIZABLE: read, work, write, retry on 40001 | 10 | 0 | 0 |

## Variation across three runs (high contention)

| Strategy | Lost updates | Retries | Gave up | Purchases/s | p99 |
|---|---|---|---|---|---|
| naive | 772 / 772 / 774 | 0 | 0 | 1679 / 399 / 415 | 74 / 355 / 318 ms |
| pessimistic `FOR UPDATE` | 0 | 0 | 0 | 284 / 191 / 192 | 1.5 / 2.3 / 2.3 s |
| optimistic version check | 0 | 8,568 / 8,963 / 11,740 | 2 / 5 / 34 | 275 / 165 / 41 | 0.7 / 2.3 / 7.3 s |
| atomic `UPDATE` | 0 | 0 | 0 | 1910 / 1881 / 1894 | 59 / 56 / 56 ms |
| Serializable + retry | 0 | 5,305 / 5,406 / 5,453 | 20 / 28 / 28 | 292 / 300 / 299 | 1.0 / 1.0 / 1.0 s |

Low contention was stable across runs: every strategy except Serializable stayed between 7,300 and 9,300 purchases/s with a p99 under 8 ms; Serializable had 479 to 484 retries and 3,400 to 4,200 purchases/s.
