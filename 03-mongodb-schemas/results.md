# Results: Mongoose vs $jsonSchema vs one collection

Measured on 2026-11-10 with `make mongo`: MongoDB 8.2.12 (official `mongo:8.2` image; 8.0 won't start on Linux 6.19+ kernels), Mongoose 8.24.8 with its bundled Node.js driver, on a laptop (Intel i7-8550U, 8 threads, 10 GB RAM). The TTL monitor runs every second here (`ttlMonitorSleepSecs=1`) instead of the default 60.

## 1. Who enforces the schema?

The same invalid order (`customerId` not an ObjectId, `status: "bogus"`, `total: -5`, an item with `qty: 0` and no `sku`):

| Write | Result |
|---|---|
| Mongoose `new Order(bad).save()` | rejected: `ValidationError` |
| Native driver `insertOne(bad)` (any other service, script or shell) | **accepted** |
| Mongoose `updateOne({$set: {status: "bogus"}})`, default options | **accepted** (update validators are off by default) |
| Same, with `{runValidators: true}` | rejected: `ValidationError` |
| Mongoose `updateOne({$inc: {total: -1000}}, {runValidators: true})` | **accepted** (update validators don't cover `$inc`) |
| With `mongoose.set('runValidators', true)`: `$set status "bogus"` / `$inc total -1000` | rejected: `ValidationError` / **accepted** |

After adding a `$jsonSchema` validator (`collMod`, `validationLevel: "strict"`, `validationAction: "error"`):

| Write | Result |
|---|---|
| Native driver `insertOne(bad)` | rejected: error 121 (DocumentValidationFailure) |
| Native driver `updateOne($set status "bogus")` | rejected: error 121 (DocumentValidationFailure) |
| Native driver `updateOne($inc total -1000)` | rejected: error 121 (DocumentValidationFailure) |
| Valid order through the driver / through Mongoose | accepted / accepted |
| Invalid documents stored *before* the validator (found with `{$nor: [{$jsonSchema: …}]}`) | **1 still there**: validators check writes, not existing data |
| Update an unrelated field of that old invalid document, `strict` | rejected: error 121 (DocumentValidationFailure) |
| Same, `validationLevel: "moderate"` | accepted |
| New invalid insert, `moderate` | rejected: error 121 (DocumentValidationFailure) |

## 2. A TTL index in one shared collection

Sessions should expire; quotes keep an `expiresAt` ("valid until") for display and must be kept. Both expired a minute ago.

| Layout | Sessions left | Quotes left |
|---|---:|---:|
| One `everything` collection, TTL index on `expiresAt` | 0 | **0** |
| Separate `sessions` and `quotes` collections | 0 | 1 |
| One collection, TTL index with `partialFilterExpression: {type: "session"}` | 0 | 1 |

## 3. Indexes in one shared collection

Inserting 50,000 session documents. The shared collection has the indexes other types need (`customerId`, `status+createdAt`, `sku`, `email`, `token`); sessions use only `token`. Median of 3 runs.

| Layout | Indexes | Insert time | Index size | Keys in the `customerId` index |
|---|---:|---:|---:|---:|
| One collection, plain indexes | 6 | 712 ms | 1.79 MB | **50,000** (a null key per session) |
| One collection, partial indexes (`$exists`) | 6 | 386 ms | 1.04 MB | 0 |
| Separate `sessions` collection | 2 | 364 ms | 1.02 MB | — |

(Index counts include `_id`.)

## 4. One document per customer, embedding every order

Pushing 1 MiB of order history at a time into one document: 15 pushes succeeded, the next failed with **error 10334 (BSONObjectTooLarge)**. The document stopped at 15,728,980 bytes; the limit is 16,777,216 (16 MB).
