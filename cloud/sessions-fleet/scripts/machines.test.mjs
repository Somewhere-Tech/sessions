import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { stripTypeScriptTypes } from 'node:module';
import test from 'node:test';

// Execute the production route/helper without a deployment, credentials, SDK,
// or generated files. Node 22.6+ supplies the TypeScript syntax stripping.
async function sourceModule(relativePath, imports = {}) {
  let source = stripTypeScriptTypes(await readFile(new URL(relativePath, import.meta.url), 'utf8'));
  for (const [specifier, url] of Object.entries(imports)) {
    source = source.replaceAll(`'${specifier}'`, JSON.stringify(url));
  }
  return `data:text/javascript;base64,${Buffer.from(source).toString('base64')}`;
}

const httpURL = await sourceModule('../functions/_lib/http.ts');
const machinesURL = await sourceModule('../functions/_lib/machines.ts', { './http': httpURL });
const { signedMachineRequest } = await import(machinesURL);
const indexURL = await sourceModule('../functions/api/machines/index.ts', {
  '../../_lib/http': httpURL, '../../_lib/machines': machinesURL,
});
const { default: machineIndex } = await import(indexURL);
const baseTime = Date.UTC(2026, 8, 29, 12);

async function fixture() {
  const keys = await crypto.subtle.generateKey('Ed25519', true, ['sign', 'verify']);
  const publicKey = Buffer.from(await crypto.subtle.exportKey('raw', keys.publicKey)).toString('base64url');
  const nonces = new Map();
  const machine = { id: 'fixture-machine', machine_public_key: publicKey };
  const sw = {
    auth: { async requireUser(req) { assert.equal(req.headers.get('Authorization'), 'Bearer fixture'); return { id: 'fixture-owner' }; } },
    db: {
      async from(table) { assert.equal(table, 'machines'); return { data: [machine] }; },
      async remove(table, { where }) {
        assert.equal(table, 'machine_nonces');
        for (const [key, value] of nonces) if (value.created_at < where.created_at.lt) nonces.delete(key);
      },
      async insert(table, value, options) {
        assert.equal(table, 'machine_nonces');
        assert.equal(options.onConflict, 'ignore');
        const key = JSON.stringify([value.machine_id, value.nonce]);
        if (nonces.has(key)) return { changes: 0 };
        nonces.set(key, value);
        return { changes: 1 };
      },
    },
  };
  return { sw, keys };
}

async function signedRequest(keys, futureSeconds = 0) {
  const timestamp = String(Math.floor(Date.now() / 1000) + futureSeconds);
  const nonce = crypto.randomUUID();
  const path = '/api/machines/index';
  const hash = Buffer.from(await crypto.subtle.digest('SHA-256', new Uint8Array())).toString('hex');
  const bytes = new TextEncoder().encode(`fixture-machine${timestamp}${nonce}GET${path}${hash}`);
  const signature = Buffer.from(await crypto.subtle.sign('Ed25519', keys.privateKey, bytes)).toString('base64url');
  return new Request(`https://fixture.invalid${path}`, { headers: {
    Authorization: 'Bearer fixture', 'X-Sessions-Machine-ID': 'fixture-machine',
    'X-Sessions-Timestamp': timestamp, 'X-Sessions-Nonce': nonce, 'X-Sessions-Signature': signature,
  } });
}

test('production signed helper refuses future-dated replay throughout the inclusive validity window', async (t) => {
  t.mock.timers.enable({ apis: ['Date'], now: baseTime });
  const { sw, keys } = await fixture();
  const req = await signedRequest(keys, 300);
  await signedMachineRequest(req, sw, '');
  for (const advance of [301_000, 299_000]) {
    t.mock.timers.tick(advance);
    await assert.rejects(signedMachineRequest(req, sw, ''), { code: 'NONCE_REPLAYED' });
  }
  t.mock.timers.tick(1_000);
  await assert.rejects(signedMachineRequest(req, sw, ''), { code: 'STALE_SIGNATURE' });
});

test('production directory route requires machine signature even with valid owner bearer', async (t) => {
  t.mock.timers.enable({ apis: ['Date'], now: baseTime });
  const { sw, keys } = await fixture();
  const unsigned = new Request('https://fixture.invalid/api/machines/index', { headers: { Authorization: 'Bearer fixture' } });
  const refused = await machineIndex(unsigned, sw);
  assert.equal(refused.status, 400);
  assert.equal((await refused.json()).code, 'VALIDATION_ERROR');
  const accepted = await machineIndex(await signedRequest(keys), sw);
  assert.equal(accepted.status, 200);
  assert.equal((await accepted.json()).machines[0].id, 'fixture-machine');
});

test('fractional-second validity cannot outlive nonce retention', async (t) => {
  t.mock.timers.enable({ apis: ['Date'], now: baseTime + 1 });
  const { sw, keys } = await fixture();
  const req = await signedRequest(keys, 300);
  await signedMachineRequest(req, sw, '');
  t.mock.timers.tick(600_998);
  await assert.rejects(signedMachineRequest(req, sw, ''), { code: 'NONCE_REPLAYED' });
  t.mock.timers.tick(1);
  await assert.rejects(signedMachineRequest(req, sw, ''), { code: 'STALE_SIGNATURE' });
});

test('async directory lookup or replay cleanup cannot admit an expired signed request', async (t) => {
  t.mock.timers.enable({ apis: ['Date'], now: baseTime });
  for (const operation of ['from', 'remove']) {
    const { sw, keys } = await fixture();
    const req = await signedRequest(keys);
    const original = sw.db[operation];
    sw.db[operation] = async (...args) => {
      const result = await original(...args);
      t.mock.timers.tick(301_000);
      return result;
    };
    await assert.rejects(signedMachineRequest(req, sw, ''), { code: 'STALE_SIGNATURE' });
  }
});
