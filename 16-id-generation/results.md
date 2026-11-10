# Primary keys: bigint vs UUIDv4 vs UUIDv7

PostgreSQL 17.11 in Docker, `shared_buffers` = 128 MB. 5,000,000 rows inserted into each table in batches of 50,000, each row a key plus about 100 bytes. Measured 2026-11-10.

| Key | Insert time | Rows/s | WAL written | Primary key index | Leaf density | Table |
|---|---:|---:|---:|---:|---:|---:|
| bigint | 24.4 s | 204,918 | 1081 MB | 107 MB | 90.1% | 673 MB |
| uuidv4 | 33.7 s | 148,368 | 1235 MB | 196 MB | 69.3% | 710 MB |
| uuidv7 | 21.2 s | 235,849 | 1166 MB | 150 MB | 90.0% | 710 MB |

