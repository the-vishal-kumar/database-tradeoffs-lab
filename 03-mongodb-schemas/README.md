# 03 — MongoDB schemas: Mongoose vs `$jsonSchema` vs one collection for everything

Companion to the post *"If MongoDB is schemaless, why do we write Mongoose schemas? Why not one collection for everything?"*

```bash
make mongo
# or, without make:
docker compose up -d --wait mongo
docker compose run --rm node sh -c 'npm ci && node demo.mjs'
```

Runs against MongoDB 8.2 with Mongoose 8, and prints what each write actually did. Measured results: [results.md](results.md).

| # | What it shows |
|---|---|
| 1 | The same invalid order written four ways: through Mongoose `save()`, through the native driver (what any other service or script uses), and through Mongoose updates with and without `runValidators`. Then the same writes after a `$jsonSchema` validator is added to the collection, including documents that were already invalid and the `strict` vs `moderate` validation levels |
| 2 | A TTL index meant for sessions, in one shared collection, deleting quotes that happen to have the same field. Compared with separate collections, and fixed with a partial TTL index |
| 3 | 50,000 sessions inserted into one shared collection carrying other types' indexes, vs the same collection with partial indexes, vs a `sessions` collection |
| 4 | One document per customer with every order embedded, growing until MongoDB's 16 MB document limit refuses the write |

[`demo.test.mjs`](demo.test.mjs) pins every result (`node --test`), and CI runs it against MongoDB 8.2.

## Notes

- MongoDB 8.0 refuses to start on Linux kernels 6.19 and newer ([SERVER-121912](https://jira.mongodb.org/browse/SERVER-121912)); 8.2 starts. The demo pins `mongo:8.2`.
- `ttlMonitorSleepSecs=1` (set in `compose.yaml`) makes the TTL monitor run every second instead of every 60, so scenario 2 takes seconds. Don't set it in production.
- Not measured here: shard keys (a collection has one, so one shared collection forces one key on every type) and access control (MongoDB grants privileges per collection, not per document type). The post explains both without a demo.
