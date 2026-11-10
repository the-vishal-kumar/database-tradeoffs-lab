// Pins every result the post quotes. Needs MONGO_URL (make test starts MongoDB).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { run } from './demo.mjs';

test('validation, TTL, indexes and the 16 MB limit behave as the post says', { skip: !process.env.MONGO_URL, timeout: 120_000 }, async () => {
  const r = await run(process.env.MONGO_URL, 5_000);
  const v = r.validation;

  // Mongoose validates only what goes through Mongoose document saves...
  assert.equal(v.mongooseSaveBad, 'ValidationError');
  assert.equal(v.driverInsertBad, 'ok', 'the native driver bypasses Mongoose');
  assert.equal(v.mongooseUpdateBadDefault, 'ok', 'update validators are off by default');
  assert.equal(v.mongooseUpdateBadRunValidators, 'ValidationError');
  assert.equal(v.mongooseUpdateIncNegative, 'ok', 'runValidators does not cover $inc');

  // ...while $jsonSchema is enforced by the server on every write (error 121).
  for (const k of ['dbDriverInsertBad', 'dbDriverUpdateBad', 'dbDriverIncNegative', 'strictUpdateOfOldInvalidDoc', 'moderateInsertBad']) {
    assert.match(v[k], /^121\b/, k);
  }
  assert.equal(v.dbDriverInsertGood, 'ok');
  assert.equal(v.dbMongooseSaveGood, 'ok');
  assert.equal(v.invalidDocsAlreadyStored, 1, 'a validator does not check documents already stored');
  assert.equal(v.moderateUpdateOfOldInvalidDoc, 'ok');

  // A TTL index in a shared collection deletes every type that has the field.
  assert.deepEqual(r.ttl.oneCollection, { sessionsLeft: 0, quotesLeft: 0 });
  assert.deepEqual(r.ttl.separateCollections, { sessionsLeft: 0, quotesLeft: 1 });
  assert.deepEqual(r.ttl.oneCollectionPartialTTL, { sessionsLeft: 0, quotesLeft: 1 });

  // Unrelated indexes in a shared collection hold a key per document.
  assert.equal(r.indexes.everything.customerIdKeys, 5_000);
  assert.equal(r.indexes.everythingPartial.customerIdKeys, 0);

  // One document can't pass 16 MB (BSONObjectTooLarge).
  assert.equal(r.docLimit.error, '10334');
  assert.ok(r.docLimit.finalDocBytes < r.docLimit.limitBytes);
});
