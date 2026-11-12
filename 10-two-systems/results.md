# Results: two databases, no distributed transaction

Measured on 2026-11-10 with `make twosys` on a laptop, PostgreSQL 17 in Docker (both databases on one server). Crashes and declines come from fixed seeds, so the counts repeat; times vary.

An earlier run with `max_prepared_transactions = 50` failed at order 207 with *"maximum number of prepared transactions reached"*: in-doubt transactions had used up the limit, so no new two-phase commit could start until they were resolved. The compose file now allows 400 so the comparison can finish.

PostgreSQL, orders in database `lab` and payments in database `payments`. 1000 orders per strategy; a 5% chance of a crash at every crash point; 5% of cards declined.

| Strategy | Crashes | Inconsistent before recovery (outbox: before the relay runs) | Left in doubt / pending | Charges redelivered | Inconsistent after recovery | Took |
|---|---:|---:|---|---:|---:|---:|
| dual write | 47 | 47 | - | 0 | 47 | 1.6 s |
| two-phase commit | 139 | 0 | 223 prepared transactions | 0 | 0 | 4.8 s |
| saga | 96 | 47 | 96 orders stuck pending (all 1000 were visible as pending for a while) | 0 | 0 | 2.5 s |
| outbox | 51 | 1000 | - | 48 | 0 | 2.7 s |

### An in-doubt transaction holds its locks

