// Static mongosh seed for the owned MongoDB fixture of
// `make test-integration DB_DRIVER=mongodb`, run once per container by
// TestMongoDBIntegration.
//
// Runner contract:
// - The test sends one script to `mongosh --quiet --norc
//   mongodb://127.0.0.1:27017/admin` inside the owned container. Before this
//   file it authenticates `db` as root with the password read from
//   /fixture/admin-password and declares `const codecCases`: the
//   testdata/codecs.json cases that have `extjson` and no `"seed": false`,
//   embedded as JSON objects. After this file it prints its success marker.
// - Any uncaught error, including a failed server command, fails the run.
// - Passwords are read from /fixture files through fs, never from arguments or
//   the environment.
// - This file declares no top-level names, so the prefix cannot collide with it.
//
// mongosh converts Extended JSON through JavaScript values. A corpus case whose
// meaning depends on exact number text, duplicate keys, integer-like key order
// or a date JavaScript cannot represent must set "seed": false.
//
// The seed also initiates the authorization-disabled single-node replica set
// dm0 on 127.0.0.1:27018 and creates `reader` there, which admission must
// refuse.
{
  const password = require("fs").readFileSync("/fixture/reader-password", "utf8").trim();
  const read = (name) => ({ role: "read", db: name });
  const adminRole = (name) => ({ role: name, db: "admin" });
  const app = db.getSiblingDB("app");

  const items = [];
  for (let i = 1; i <= 12; i++) {
    items.push({
      _id: NumberInt(i),
      name: "item-" + String(i).padStart(2, "0"),
      group: i % 2 === 1 ? "a" : "b",
      qty: NumberInt(i * 10),
      tags: ["t" + (i % 3), "u" + (i % 4)],
      attrs: { color: ["red", "green", "blue"][i % 3], size: NumberInt(i % 5) },
      loc: { type: "Point", coordinates: [-122 + i / 100, 37 + i / 100] },
    });
  }
  app.items.insertMany(items);
  // With `_id_`, items has 11 indexes; expires_at is absent, so TTL never deletes.
  app.runCommand({
    createIndexes: "items",
    indexes: [
      { key: { name: 1 }, name: "items_name", unique: true },
      { key: { group: 1, qty: -1 }, name: "items_group_qty" },
      { key: { qty: 1 }, name: "items_partial", partialFilterExpression: { qty: { $gte: 50 } } },
      { key: { expires_at: 1 }, name: "items_ttl", expireAfterSeconds: 3600 },
      { key: { name: "text" }, name: "items_text" },
      { key: { loc: "2dsphere" }, name: "items_geo" },
      { key: { group: "hashed" }, name: "items_hashed" },
      { key: { "attrs.$**": 1 }, name: "items_wildcard" },
      { key: { name: 1, group: 1 }, name: "items_name_ci", collation: { locale: "en", strength: 2 } },
      { key: { tags: 1 }, name: "items_hidden", hidden: true },
    ],
  });
  // Six items (odd ids) are in group a.
  app.createView("v_items", "items", [{ $match: { group: "a" } }, { $project: { name: 1, qty: 1 } }]);

  app.createCollection("capped_log", { capped: true, size: 65536, max: 100 });
  app.capped_log.insertMany([{ _id: NumberInt(1), msg: "started" }, { _id: NumberInt(2), msg: "ready" }]);

  app.createCollection("metrics", { timeseries: { timeField: "ts", metaField: "sensor", granularity: "minutes" } });
  const metrics = [];
  for (let i = 0; i < 6; i++) {
    metrics.push({ ts: new Date(Date.UTC(2026, 0, 1, 0, i)), sensor: "s" + (i % 2), value: i + 0.5 });
  }
  app.metrics.insertMany(metrics);

  app.createCollection("validated", {
    validator: {
      $jsonSchema: {
        bsonType: "object",
        required: ["name"],
        properties: { name: { bsonType: "string" }, qty: { bsonType: "int", minimum: 0 } },
      },
    },
    validationLevel: "moderate",
    validationAction: "error",
    collation: { locale: "en", strength: 2 },
  });
  app.validated.insertOne({ _id: NumberInt(1), name: "valid", qty: NumberInt(1) });

  app.createCollection("a.b");
  app.getCollection("a.b").insertOne({ _id: NumberInt(1), dotted: true });

  const codecs = [];
  for (const c of codecCases) {
    codecs.push(EJSON.parse(JSON.stringify({ _id: c.name, v: c.extjson }), { relaxed: false }));
  }
  app.codecs.insertMany(codecs);

  // big holds 50,001 small documents so a default find (max_rows 50,000) truncates.
  for (let start = 1; start <= 50001; start += 10000) {
    const batch = [];
    for (let i = start; i < start + 10000 && i <= 50001; i++) {
      batch.push({ _id: NumberInt(i), k: NumberInt(i % 1000) });
    }
    app.big.insertMany(batch);
  }

  // large holds 24 documents of 1 MiB, more than one 16 MiB first batch.
  const blob = "x".repeat(1 << 20);
  const large = [];
  for (let i = 1; i <= 24; i++) {
    large.push({ _id: NumberInt(i), blob });
  }
  app.large.insertMany(large);

  db.getSiblingDB("hidden").target.insertOne({ _id: NumberInt(1), note: "readable by reader" });
  db.getSiblingDB("secret").rows.insertOne({ _id: NumberInt(1), note: "unreadable by reader" });

  const admin = db.getSiblingDB("admin");
  const roles = [
    ["items_find", [{ resource: { db: "app", collection: "items" }, actions: ["find"] }], []],
    ["insert_target", [{ resource: { db: "app", collection: "items" }, actions: ["insert"] }], []],
    ["writer_app", [], [{ role: "readWrite", db: "app" }]],
    ["bridge", [], [adminRole("writer_app")]],
    ["self_view", [{ resource: { db: "admin", collection: "" }, actions: ["viewUser"] }], []],
    ["cred_find", [{ resource: { db: "admin", collection: "system.users" }, actions: ["find"] }], []],
  ];
  for (const [role, privileges, inherited] of roles) {
    admin.createRole({ role, privileges, roles: inherited });
  }

  // Expected admission: reader, scoped, narrow and any_reader pass; monitor
  // passes from MongoDB 8.2; every other account fails at read_only.
  const users = [
    ["reader", [read("app"), read("hidden")]],
    ["narrow", [adminRole("items_find")]],
    ["any_reader", [adminRole("readAnyDatabase")]],
    ["monitor", [adminRole("clusterMonitor"), read("app")]],
    ["rw", [{ role: "readWrite", db: "app" }]],
    ["ddl", [{ role: "dbAdmin", db: "app" }]],
    ["user_admin", [{ role: "userAdmin", db: "app" }]],
    ["rw_any", [adminRole("readWriteAnyDatabase")]],
    ["cluster_admin", [adminRole("clusterManager")]],
    ["backup", [adminRole("backup")]],
    ["one_insert", [read("app"), adminRole("insert_target")]],
    ["nested", [adminRole("bridge")]],
    ["view_user", [adminRole("self_view")]],
    ["cred_reader", [adminRole("cred_find")]],
  ];
  for (const [user, granted] of users) {
    admin.createUser({ user, pwd: password, roles: granted, mechanisms: ["SCRAM-SHA-256"] });
  }
  app.createUser({ user: "scoped", pwd: password, roles: [read("app")], mechanisms: ["SCRAM-SHA-256"] });

  const noauth = connect("mongodb://127.0.0.1:27018/admin?directConnection=true");
  noauth.runCommand({ replSetInitiate: { _id: "dm0", members: [{ _id: 0, host: "127.0.0.1:27018" }] } });
  let primary = false;
  for (let i = 0; i < 300 && !primary; i++) {
    primary = noauth.runCommand({ hello: 1 }).isWritablePrimary === true;
    if (!primary) {
      sleep(100);
    }
  }
  if (!primary) {
    throw new Error("authorization-disabled replica set did not become primary within 30 s");
  }
  noauth.createUser({ user: "reader", pwd: password, roles: [read("app")], mechanisms: ["SCRAM-SHA-256"] });
}
