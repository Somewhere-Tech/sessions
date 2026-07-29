import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { build } from 'esbuild';

const scratch = await mkdtemp(join(tmpdir(), 'sessions-resume-partial-smoke-'));
const output = join(scratch, 'sessionsd.mjs');

const storage = new Map();
globalThis.window = {
  location: {
    protocol: 'http:',
    hostname: '127.0.0.1',
    port: '8787',
    host: '127.0.0.1:8787',
    origin: 'http://127.0.0.1:8787'
  },
  localStorage: {
    getItem: (key) => storage.get(key) ?? null,
    setItem: (key, value) => storage.set(key, String(value)),
    removeItem: (key) => storage.delete(key)
  }
};
globalThis.localStorage = globalThis.window.localStorage;

const provider = 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee';
const laneId = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
const sourceSessionId = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
const requests = [];
globalThis.fetch = async (_input, init) => {
  const body = JSON.parse(String(init?.body ?? '{}'));
  requests.push(body);
  if (!body.repairLaneId) {
    return new Response(JSON.stringify({
      ok: false,
      partial: true,
      laneId,
      adoption: {
        path: provider,
        tool: 'codex',
        cwd: '/work',
        providerUuid: provider,
        cmd: 'codex',
        args: ['resume', provider]
      },
      warning: 'The live successor needs ledger repair.',
      missingAnnotations: ['source-linked'],
      repair: { target: provider, laneId, sourceSessionId }
    }), { status: 202, headers: { 'content-type': 'application/json' } });
  }
  return new Response(JSON.stringify({
    ok: true,
    laneId,
    adoption: {
      path: provider,
      tool: 'codex',
      cwd: '/work',
      providerUuid: provider,
      cmd: 'codex',
      args: ['resume', provider]
    }
  }), { status: 200, headers: { 'content-type': 'application/json' } });
};

try {
  await build({
    entryPoints: ['src/api/sessionsd.ts'],
    bundle: true,
    format: 'esm',
    outfile: output,
    platform: 'node',
    target: 'node20',
    logLevel: 'silent'
  });
  const { adoptConversation, repairAdoption } = await import(
    `${pathToFileURL(output).href}?v=${Date.now()}`
  );

  const partial = await adoptConversation(provider, sourceSessionId);
  assert.equal(partial.partial, true);
  assert.equal(partial.laneId, laneId);
  assert.deepEqual(partial.missingAnnotations, ['source-linked']);
  assert.ok(partial.repair);

  const repaired = await repairAdoption(partial.repair);
  assert.equal(repaired.ok, true);
  assert.equal(repaired.laneId, laneId);
  assert.equal(requests.length, 2);
  assert.equal(requests[0].repairLaneId, undefined);
  assert.equal(requests[1].repairLaneId, laneId);
  assert.equal(requests[1].sourceSessionId, sourceSessionId);

  console.log('resume partial-success smoke: ok');
} finally {
  await rm(scratch, { recursive: true, force: true });
}
