// Mongoose validation vs MongoDB's own $jsonSchema validator vs "one
// collection for everything", measured against a real MongoDB.
//
//   MONGO_URL=mongodb://localhost:57017 node demo.mjs
//
// Every scenario returns what actually happened (an error code, a count, a
// size), so the post and the tests quote the same numbers.

import mongoose from 'mongoose';
import { setTimeout as sleep } from 'node:timers/promises';
import { pathToFileURL } from 'node:url';

const { Schema } = mongoose;

// ---- the model -------------------------------------------------------------

export const orderSchema = new Schema(
  {
    customerId: { type: Schema.Types.ObjectId, required: true },
    status: { type: String, enum: ['pending', 'paid', 'shipped'], required: true },
    total: { type: Number, min: 0, required: true },
    items: {
      type: [{ sku: { type: String, required: true }, qty: { type: Number, min: 1, required: true } }],
      validate: (v) => v.length > 0,
    },
  },
  { collection: 'orders', versionKey: false },
);

// The same rules, enforced by MongoDB itself on every write, whoever writes.
export const orderJsonSchema = {
  bsonType: 'object',
  required: ['customerId', 'status', 'total', 'items'],
  properties: {
    customerId: { bsonType: 'objectId' },
    status: { enum: ['pending', 'paid', 'shipped'] },
    total: { bsonType: ['double', 'int', 'long', 'decimal'], minimum: 0 },
    items: {
      bsonType: 'array',
      minItems: 1,
      items: {
        bsonType: 'object',
        required: ['sku', 'qty'],
        properties: { sku: { bsonType: 'string' }, qty: { bsonType: ['int', 'long', 'double'], minimum: 1 } },
      },
    },
  },
};

const badOrder = () => ({ customerId: 'not-an-id', status: 'bogus', total: -5, items: [{ qty: 0 }] });
const goodOrder = () => ({ customerId: new mongoose.Types.ObjectId(), status: 'pending', total: 42, items: [{ sku: 'A-1', qty: 2 }] });

// code returns MongoDB's error code, Mongoose's error name, or 'ok'.
async function code(fn) {
  try {
    await fn();
    return 'ok';
  } catch (err) {
    if (err?.code !== undefined) return `${err.code} ${err.codeName ?? ''}`.trim();
    return err?.name ?? String(err);
  }
}

// ---- scenarios -------------------------------------------------------------

/** Who enforces the schema: Mongoose, the driver, or MongoDB? */
async function validation(db) {
  await db.dropDatabase();
  const Order = mongoose.model('Order', orderSchema);
  const raw = db.collection('orders');
  const r = {};

  r.mongooseSaveBad = await code(() => new Order(badOrder()).save());
  r.driverInsertBad = await code(() => raw.insertOne(badOrder()));

  const { _id } = await new Order(goodOrder()).save();
  r.mongooseUpdateBadDefault = await code(() => Order.updateOne({ _id }, { $set: { status: 'bogus' } }));
  await raw.updateOne({ _id }, { $set: { status: 'pending' } });
  r.mongooseUpdateBadRunValidators = await code(() =>
    Order.updateOne({ _id }, { $set: { status: 'bogus' } }, { runValidators: true }),
  );
  r.mongooseUpdateIncNegative = await code(() => Order.updateOne({ _id }, { $inc: { total: -1000 } }, { runValidators: true }));
  await raw.updateOne({ _id }, { $set: { total: 42 } });

  // Now let MongoDB enforce it.
  await db.command({ collMod: 'orders', validator: { $jsonSchema: orderJsonSchema }, validationLevel: 'strict', validationAction: 'error' });
  r.dbDriverInsertBad = await code(() => raw.insertOne(badOrder()));
  r.dbDriverUpdateBad = await code(() => raw.updateOne({ _id }, { $set: { status: 'bogus' } }));
  r.dbDriverIncNegative = await code(() => raw.updateOne({ _id }, { $inc: { total: -1000 } }));
  r.dbDriverInsertGood = await code(() => raw.insertOne(goodOrder()));
  r.dbMongooseSaveGood = await code(() => new Order(goodOrder()).save());

  // A validator only checks writes: the bad document inserted before it is
  // still there, and $jsonSchema in a query finds it.
  r.invalidDocsAlreadyStored = await raw.countDocuments({ $nor: [{ $jsonSchema: orderJsonSchema }] });
  const invalid = await raw.findOne({ $nor: [{ $jsonSchema: orderJsonSchema }] });
  r.strictUpdateOfOldInvalidDoc = await code(() => raw.updateOne({ _id: invalid._id }, { $set: { note: 'x' } }));
  await db.command({ collMod: 'orders', validationLevel: 'moderate' });
  r.moderateUpdateOfOldInvalidDoc = await code(() => raw.updateOne({ _id: invalid._id }, { $set: { note: 'x' } }));
  r.moderateInsertBad = await code(() => raw.insertOne(badOrder()));

  mongoose.deleteModel('Order');
  return r;
}

/** One "everything" collection: a TTL index meant for sessions deletes quotes. */
async function ttl(db) {
  const r = {};
  const past = new Date(Date.now() - 60_000);
  const everything = db.collection('everything');
  await everything.createIndex({ expiresAt: 1 }, { expireAfterSeconds: 0 });
  await everything.insertMany([
    { type: 'session', token: 's1', expiresAt: past },
    // An expired quote is still a business record: "valid until" for display.
    { type: 'quote', number: 'Q-1', total: 500, expiresAt: past },
  ]);

  const sessions = db.collection('sessions');
  const quotes = db.collection('quotes');
  await sessions.createIndex({ expiresAt: 1 }, { expireAfterSeconds: 0 });
  await sessions.insertOne({ token: 's1', expiresAt: past });
  await quotes.insertOne({ number: 'Q-1', total: 500, expiresAt: past });

  // The fix within one collection: a partial TTL index.
  const partial = db.collection('everything_partial');
  await partial.createIndex({ expiresAt: 1 }, { expireAfterSeconds: 0, partialFilterExpression: { type: 'session' } });
  await partial.insertMany([
    { type: 'session', token: 's1', expiresAt: past },
    { type: 'quote', number: 'Q-1', total: 500, expiresAt: past },
  ]);

  // The TTL monitor runs every ttlMonitorSleepSecs (1 s here, 60 s by default).
  for (let i = 0; i < 30; i++) {
    await sleep(1000);
    if ((await everything.countDocuments({ type: 'session' })) === 0) break;
  }
  await sleep(2000);
  r.oneCollection = { sessionsLeft: await everything.countDocuments({ type: 'session' }), quotesLeft: await everything.countDocuments({ type: 'quote' }) };
  r.separateCollections = { sessionsLeft: await sessions.countDocuments(), quotesLeft: await quotes.countDocuments() };
  r.oneCollectionPartialTTL = { sessionsLeft: await partial.countDocuments({ type: 'session' }), quotesLeft: await partial.countDocuments({ type: 'quote' }) };
  return r;
}

/** One collection: every type pays for every index (unless indexes are partial). */
async function indexes(db, n) {
  const sessionDocs = () =>
    Array.from({ length: n }, (_, i) => ({ type: 'session', token: `t${i}`, userId: i, expiresAt: new Date(Date.now() + 3_600_000) }));
  // Indexes an "everything" collection collects over time: one or two per type.
  const shared = [{ customerId: 1 }, { status: 1, createdAt: -1 }, { sku: 1 }, { email: 1 }, { token: 1 }];
  const setups = {
    everything: async (c) => { for (const k of shared) await c.createIndex(k); },
    everythingPartial: async (c) => {
      for (const k of shared) await c.createIndex(k, { partialFilterExpression: { [Object.keys(k)[0]]: { $exists: true } } });
    },
    sessions: async (c) => { await c.createIndex({ token: 1 }); },
  };
  const r = { n };
  for (const [name, setup] of Object.entries(setups)) {
    const times = [];
    for (let run = 0; run < 3; run++) {
      const c = db.collection(`idx_${name}`);
      await c.drop().catch(() => {});
      await setup(c);
      const t = performance.now();
      await c.insertMany(sessionDocs(), { ordered: false });
      times.push(performance.now() - t);
    }
    times.sort((a, b) => a - b);
    await db.admin().command({ fsync: 1 }); // flush, so the size statistics are real
    const [st] = await db.collection(`idx_${name}`).aggregate([{ $collStats: { storageStats: {} } }]).toArray();
    r[name] = {
      insertMsMedian: Math.round(times[1]),
      indexes: Object.keys(st.storageStats.indexSizes).length,
      indexBytes: st.storageStats.totalIndexSize,
    };
  }
  // What one unrelated index holds: sessions have no customerId, yet a plain
  // index stores a (null) key for every one of them; a partial one stores none.
  r.everything.customerIdKeys = await countIndexKeys(db.collection('idx_everything'), { customerId: 1 });
  r.everythingPartial.customerIdKeys = await countIndexKeys(db.collection('idx_everythingPartial'), { customerId: 1 });
  return r;
}

// countIndexKeys counts keys in an index by scanning it with an explain.
async function countIndexKeys(coll, key) {
  const ex = await coll.find({}, { hint: key }).explain('executionStats');
  return ex.executionStats.totalKeysExamined;
}

/** Embedding without a bound: one document can't grow past 16 MB. */
async function docLimit(db) {
  const customers = db.collection('customers');
  const chunk = 'x'.repeat(1024 * 1024); // 1 MiB of order history at a time
  const { insertedId } = await customers.insertOne({ name: 'Acme', orders: [] });
  let mib = 0;
  let err = 'ok';
  for (; mib < 40; mib++) {
    err = await code(() => customers.updateOne({ _id: insertedId }, { $push: { orders: { note: chunk } } }));
    if (err !== 'ok') break;
  }
  const [{ size }] = await customers.aggregate([{ $match: { _id: insertedId } }, { $project: { size: { $bsonSize: '$$ROOT' } } }]).toArray();
  return { pushedMiB: mib, error: err, finalDocBytes: size, limitBytes: 16 * 1024 * 1024 };
}

// ---- run -------------------------------------------------------------------

export async function run(url = process.env.MONGO_URL ?? 'mongodb://localhost:57017', n = 50_000) {
  await mongoose.connect(url, { dbName: 'schemas_demo' });
  try {
    const db = mongoose.connection.db;
    const version = (await db.admin().command({ buildInfo: 1 })).version;
    const out = { mongodb: version, mongoose: mongoose.version };
    out.validation = await validation(db);
    out.ttl = await ttl(db);
    out.indexes = await indexes(db, n);
    out.docLimit = await docLimit(db);
    await db.dropDatabase();
    return out;
  } finally {
    await mongoose.disconnect();
  }
}

if (import.meta.url === pathToFileURL(process.argv[1]).href) {
  const out = await run(undefined, Number(process.env.N ?? 50_000));
  console.log(JSON.stringify(out, null, 2));
}
