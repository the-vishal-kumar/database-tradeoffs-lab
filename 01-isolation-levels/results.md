# Results: isolation levels

Measured on 2026-11-10 with `make isolation` (the official `postgres:17-alpine` image, default configuration). Regenerate with `make isolation > 01-isolation-levels/results.md` and re-add this header.

```text
PostgreSQL 17.11

| Scenario | Read Uncommitted | Read Committed | Repeatable Read | Serializable |
|---|---|---|---|---|
| Dirty read | prevented | prevented | prevented | prevented |
| Non-repeatable read | anomaly | anomaly | prevented | prevented |
| Phantom read | anomaly | anomaly | prevented | prevented |
| Lost update (read, then write) | anomaly | anomaly | aborted (40001), retry | aborted (40001), retry |
| Lost update, fixed with UPDATE v = v + 1 | prevented | prevented | aborted (40001), retry | aborted (40001), retry |
| Lost update, fixed with SELECT … FOR UPDATE | prevented | prevented | aborted (40001), retry | aborted (40001), retry |
| Write skew (on-call doctors) | anomaly | anomaly | anomaly | aborted (40001), retry |
| Write skew, fixed with SELECT … FOR UPDATE | prevented | prevented | aborted (40001), retry | aborted (40001), retry |
| Lost update, with a retry on 40001 | anomaly | anomaly | prevented | prevented |
| Write skew, with a retry on 40001 | anomaly | anomaly | anomaly | prevented |

- Dirty read, Read Uncommitted: T2 read 100, the committed balance
- Dirty read, Read Committed: T2 read 100, the committed balance
- Dirty read, Repeatable Read: T2 read 100, the committed balance
- Dirty read, Serializable: T2 read 100, the committed balance
- Non-repeatable read, Read Uncommitted: T1 read 100, then 50
- Non-repeatable read, Read Committed: T1 read 100, then 50
- Non-repeatable read, Repeatable Read: T1 read 100, then 100
- Non-repeatable read, Serializable: T1 read 100, then 100
- Phantom read, Read Uncommitted: T1 counted 2 rows, then 3
- Phantom read, Read Committed: T1 counted 2 rows, then 3
- Phantom read, Repeatable Read: T1 counted 2 rows, then 2
- Phantom read, Serializable: T1 counted 2 rows, then 2
- Lost update (read, then write), Read Uncommitted: T2 overwrote T1's increment; v = 1
- Lost update (read, then write), Read Committed: T2 overwrote T1's increment; v = 1
- Lost update (read, then write), Repeatable Read: one transaction was aborted; v = 1
- Lost update (read, then write), Serializable: one transaction was aborted; v = 1
- Lost update, fixed with UPDATE v = v + 1, Read Uncommitted: both increments kept; v = 2
- Lost update, fixed with UPDATE v = v + 1, Read Committed: both increments kept; v = 2
- Lost update, fixed with UPDATE v = v + 1, Repeatable Read: one transaction was aborted; v = 1
- Lost update, fixed with UPDATE v = v + 1, Serializable: one transaction was aborted; v = 1
- Lost update, fixed with SELECT … FOR UPDATE, Read Uncommitted: both increments kept; v = 2
- Lost update, fixed with SELECT … FOR UPDATE, Read Committed: both increments kept; v = 2
- Lost update, fixed with SELECT … FOR UPDATE, Repeatable Read: one transaction was aborted; v = 1
- Lost update, fixed with SELECT … FOR UPDATE, Serializable: one transaction was aborted; v = 1
- Write skew (on-call doctors), Read Uncommitted: both committed; nobody is on call
- Write skew (on-call doctors), Read Committed: both committed; nobody is on call
- Write skew (on-call doctors), Repeatable Read: both committed; nobody is on call
- Write skew (on-call doctors), Serializable: one transaction was aborted; 1 doctor(s) on call
- Write skew, fixed with SELECT … FOR UPDATE, Read Uncommitted: both committed; 1 doctor(s) on call
- Write skew, fixed with SELECT … FOR UPDATE, Read Committed: both committed; 1 doctor(s) on call
- Write skew, fixed with SELECT … FOR UPDATE, Repeatable Read: one transaction was aborted; 1 doctor(s) on call
- Write skew, fixed with SELECT … FOR UPDATE, Serializable: one transaction was aborted; 1 doctor(s) on call
- Lost update, with a retry on 40001, Read Uncommitted: v = 1
- Lost update, with a retry on 40001, Read Committed: v = 1
- Lost update, with a retry on 40001, Repeatable Read: T2 aborted, committed on attempt 2; v = 2
- Lost update, with a retry on 40001, Serializable: T2 aborted, committed on attempt 2; v = 2
- Write skew, with a retry on 40001, Read Uncommitted: nobody is on call
- Write skew, with a retry on 40001, Read Committed: nobody is on call
- Write skew, with a retry on 40001, Repeatable Read: nobody is on call
- Write skew, with a retry on 40001, Serializable: T2 aborted, committed on attempt 2; 1 doctor(s) on call
```
