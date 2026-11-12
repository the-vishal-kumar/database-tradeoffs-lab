# Results: B+tree vs LSM tree

Measured on 2026-11-10 with `make engines` on a laptop (Intel i7-8550U, NVMe SSD), inside Docker. Three runs agreed on write amplification (bbolt 45.4× / 53.6× every time; Pebble 5.4 to 5.6× / 7.4 to 7.7×) and on the read gap (bbolt point reads 1.2 to 1.4 µs, Pebble 14 to 23 µs). Write throughput varies with the laptop's fsync latency. All data fits in the page cache, so reads measure CPU and structure, not disk: that is why the Bloom filters barely help here.

1000000 keys of 16 bytes, values of 100 random bytes, batches of 1000 writes (one fsync each). Go go1.27.3, both engines embedded in this process; "written to disk" is the kernel's write_bytes for the process.

| Engine | Phase | Ops/s | p50 | p99 | Written to disk | Write amplification |
|---|---|---:|---:|---:|---:|---:|
| B+tree (bbolt) | insert 1000000 keys (random order, batches of 1000, fsync per batch) | 48474 | 19 ms | 31 ms | 5272 MB | 45.4× |
| B+tree (bbolt) | overwrite every key once | 48995 | 19 ms | 30 ms | 6222 MB | 53.6× |
| B+tree (bbolt) | point reads (keys that exist) | 509126 | 1.4 µs | 11.8 µs | - | - |
| B+tree (bbolt) | point reads (keys that don't exist) | 645663 | 1.2 µs | 6.4 µs | - | - |
| B+tree (bbolt) | range scans of 100 keys | 331977 | 2.8 µs | 5.6 µs | - | - |
| LSM tree (Pebble, defaults) | insert 1000000 keys (random order, batches of 1000, fsync per batch) | 190210 | 3.24 ms | 18 ms | 627 MB | 5.4× |
| LSM tree (Pebble, defaults) | overwrite every key once | 231503 | 1.17 ms | 11 ms | 853 MB | 7.4× |
| LSM tree (Pebble, defaults) | point reads (keys that exist) | 53469 | 14.2 µs | 55.8 µs | - | - |
| LSM tree (Pebble, defaults) | point reads (keys that don't exist) | 38966 | 23.4 µs | 67.7 µs | - | - |
| LSM tree (Pebble, defaults) | range scans of 100 keys | 16771 | 56.3 µs | 133.1 µs | - | - |
| LSM tree (Pebble, Bloom filters) | insert 1000000 keys (random order, batches of 1000, fsync per batch) | 241886 | 1.60 ms | 14 ms | 651 MB | 5.6× |
| LSM tree (Pebble, Bloom filters) | overwrite every key once | 215169 | 1.08 ms | 5.54 ms | 873 MB | 7.5× |
| LSM tree (Pebble, Bloom filters) | point reads (keys that exist) | 47853 | 19.9 µs | 53.7 µs | - | - |
| LSM tree (Pebble, Bloom filters) | point reads (keys that don't exist) | 45709 | 21.1 µs | 55.5 µs | - | - |
| LSM tree (Pebble, Bloom filters) | range scans of 100 keys | 18894 | 51.4 µs | 110.4 µs | - | - |

| Engine | Live data (keys + values) | Size on disk at the end | Space amplification |
|---|---:|---:|---:|
| B+tree (bbolt) | 116 MB | 220 MB | 1.89× |
| LSM tree (Pebble, defaults) | 116 MB | 179 MB | 1.54× |
| LSM tree (Pebble, Bloom filters) | 116 MB | 181 MB | 1.56× |
