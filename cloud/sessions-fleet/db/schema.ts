import { schema, table, id, text, timestamp, json, owner } from 'somewhere/db';

export default schema({
  machines: table({
    id: id({ uuid: true }),
    name: text(),
    machine_public_key: text(),
    endpoints_json: json(),
    daemon_version: text(),
    last_seen_at: timestamp(),
    created_at: timestamp({ default: 'now' }),
    updated_at: timestamp({ default: 'now' }),
  }, {
    scope: owner(),
    indexes: [['last_seen_at']],
  }),
  machine_nonces: table({
    id: id({ uuid: true }),
    machine_id: text(),
    nonce: text(),
    created_at: timestamp(),
  }, {
    scope: owner(),
    indexes: [['created_at'], ['machine_id']],
    unique: [['machine_id', 'nonce']],
  }),
});
