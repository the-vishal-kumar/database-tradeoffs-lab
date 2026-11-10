# Results: why a Redis lock is not enough

Measured on 2026-11-10 with `make dlock` on a laptop (Intel i7-8550U, 8 threads, 10 GB RAM), everything in Docker on one machine. Scenarios 1 and 2 are scripted and give the same result every run. Scenario 3 is random (which instance starts first, which executions pause); across three runs the Redis lock let the job run twice in **16, 11 and 6** of 300 runs (37, 41 and 17 paused executions), and the other two strategies in 0.

PostgreSQL 17.11, Redis 7.4.12, go-redis 9.23.0.

### 1. A takes the lock (lease 200ms), pauses for 250ms; B takes the lock and writes; A wakes up and writes

| Strategy | B got the lock while A thought it held it | A's write after the pause | Final value |
|---|---|---|---|
| Redis lock (SET NX PX), safe release | **yes** | applied: overwrote B's newer write | written by A, after its pause |
| Redis lock + fencing token checked on write | **yes** | rejected: token 1 < 2 | written by B |
| PostgreSQL advisory lock in the writing transaction | no | applied: A still held the lock, so B waited and went second | written by B |
| Advisory lock, A's connection killed during the pause | **yes** | failed: A's transaction died with its connection | written by B |

### 2. A's lease expires, B takes the lock, then A releases "its" lock

| Release | C can take the lock while B is still working |
|---|---|
| DEL lock (release without checking the owner) | **yes** |
| compare-and-delete in Lua (release only your own lock) | no |

### 3. A job scheduled on 3 instances, 300 scheduled runs

Instances start up to 50ms apart; the job takes 5ms; 10% of executions pause for 60ms; Redis lease 30ms.

| Strategy | Runs where the job ran twice | Runs nobody did | Executions paused |
|---|---:|---:|---:|
| Redis lock (SET NX PX, safe release) + done check | 6 | 0 | 17 |
| PostgreSQL advisory lock + done check, in one transaction | 0 | 0 | 33 |
| Claim a row: INSERT ... ON CONFLICT DO NOTHING | 0 | 0 | 38 |
