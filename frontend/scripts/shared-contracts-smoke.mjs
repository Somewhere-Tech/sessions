// Two contracts that used to exist in more than one copy, and drifted.
//
//   1. adopt-then-repair — App.tsx swallowed a failed repair into
//      console.warn while ResumeDialog surfaced it, so "did my history
//      annotations finish?" had two answers depending on where the user
//      started.
//   2. the machine-pairing approval poll — three hand-rolled copies whose
//      user-facing strings had already diverged ("The other machine denied
//      this request" vs "The other Mac denied this request").
//
// Both now have exactly one implementation. These tests exercise the shared
// module's behaviour and then assert that no surface has grown its own copy.

import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { build } from 'esbuild';

const source = async (path) => readFile(new URL(`../${path}`, import.meta.url), 'utf8');

const scratch = await mkdtemp(join(tmpdir(), 'sessions-shared-contracts-'));
const output = join(scratch, 'adopt.mjs');

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

const laneId = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
const provider = 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee';

const partialBody = {
  ok: false,
  partial: true,
  laneId,
  warning: 'The live successor needs ledger repair.',
  missingAnnotations: ['source-linked'],
  repair: { target: provider, laneId }
};

let repairBehaviour = 'succeeds';
const requests = [];
globalThis.fetch = async (_input, init) => {
  const body = JSON.parse(String(init?.body ?? '{}'));
  requests.push(body);
  if (!body.repairLaneId) {
    return new Response(JSON.stringify(partialBody), {
      status: 202, headers: { 'content-type': 'application/json' }
    });
  }
  if (repairBehaviour === 'rejects') {
    return new Response('ledger is locked', { status: 503 });
  }
  if (repairBehaviour === 'still-partial') {
    return new Response(JSON.stringify(partialBody), {
      status: 202, headers: { 'content-type': 'application/json' }
    });
  }
  return new Response(JSON.stringify({ ok: true, laneId }), {
    status: 200, headers: { 'content-type': 'application/json' }
  });
};

try {
  await build({
    entryPoints: ['src/lib/adoptConversation.ts'],
    bundle: true,
    format: 'esm',
    outfile: output,
    platform: 'node',
    target: 'node20',
    logLevel: 'silent'
  });
  const { adoptConversationWithRepair, adoptionWarning, runAdoptionRepair } = await import(
    `${pathToFileURL(output).href}?v=${Date.now()}`
  );

  // A partial adoption is repaired automatically. Repair is record-only, so
  // the lane is the same lane — the first call already created the one live
  // successor and a second runtime must never appear.
  requests.length = 0;
  repairBehaviour = 'succeeds';
  const healed = await adoptConversationWithRepair(provider);
  assert.equal(requests.length, 2, 'the repair must be attempted without asking');
  assert.equal(requests[1].repairLaneId, laneId);
  assert.equal(healed.result.laneId, laneId);
  assert.equal(healed.unresolved, false);
  assert.equal(healed.repairError, null);
  assert.equal(adoptionWarning(healed), null, 'a healed adoption says nothing');

  // A repair that fails is reported, never swallowed — and the caller still
  // gets the live lane so the user is taken to their conversation.
  requests.length = 0;
  repairBehaviour = 'rejects';
  const failed = await adoptConversationWithRepair(provider);
  assert.equal(failed.result.laneId, laneId, 'the live lane survives a failed repair');
  assert.equal(failed.unresolved, true);
  assert.match(failed.repairError, /503/);
  const failedText = adoptionWarning(failed);
  assert.match(failedText, /ledger repair/);
  assert.match(failedText, /Missing: source-linked/);
  assert.match(failedText, /automatic repair failed/);

  // Still partial after the automatic pass: a manual retry stays available.
  requests.length = 0;
  repairBehaviour = 'still-partial';
  const stillPartial = await adoptConversationWithRepair(provider);
  assert.equal(stillPartial.unresolved, true);
  assert.equal(stillPartial.repairError, null);
  assert.ok(stillPartial.repair, 'a retry token must be offered');
  repairBehaviour = 'succeeds';
  const retried = await runAdoptionRepair(stillPartial.repair);
  assert.equal(retried.unresolved, false);
  assert.equal(adoptionWarning(retried), null);
} finally {
  await rm(scratch, { recursive: true, force: true });
}

// ── Single-implementation checks ───────────────────────────────────────────
const [app, resumeDialog, resumeActions, paidStartPlan, paidStartHook, forkConfirmation, pairingHook, connectScreen, fleetView, connectionsView] = await Promise.all([
  source('src/App.tsx'),
  source('src/components/ResumeDialog.tsx'),
  source('src/components/ResumeActions.tsx'),
  source('src/components/PaidStartPlan.tsx'),
  source('src/hooks/usePaidStartPlan.ts'),
  source('src/components/ForkConfirmationDialog.tsx'),
  source('src/hooks/useMachineAccessPairing.ts'),
  source('src/components/ConnectScreen.tsx'),
  source('src/components/FleetView.tsx'),
  source('src/components/ConnectionsView.tsx')
]);

assert.match(resumeDialog, /<ResumeActions/, 'ResumeDialog.tsx must delegate continuation actions');
for (const [name, surface] of [['ResumeActions.tsx', resumeActions], ['ForkConfirmationDialog.tsx', forkConfirmation]]) {
  assert.match(surface, /<PaidStartPlan/, `${name} must use the shared paid-start confirmation`);
  assert.match(surface, /usePaidStartPlan/, `${name} must use the shared paid-start state`);
}
assert.match(paidStartPlan, /Nothing runs until you press Start/,
  'the shared paid-start confirmation must make the no-start boundary explicit');
assert.match(paidStartPlan, /Access policy/,
  'the shared paid-start confirmation must disclose access');
assert.match(paidStartHook, /isDefault/,
  'the shared paid-start hook must preselect the provider default model');
assert.doesNotMatch(app, /resumeExactSession|\bawait forkConversation\(/,
  'App.tsx must open confirmation instead of starting resume or fork directly');
assert.doesNotMatch(app, /console\.warn\(['"`]Sessions resumed/,
  'a failed repair must never be downgraded to a console warning');

assert.match(pairingHook, /denied: 'The other machine denied this request\.'/);
for (const [name, surface] of [
  ['FleetView.tsx', fleetView],
  ['ConnectionsView.tsx', connectionsView]
]) {
  assert.match(surface, /useMachineAccessPairing/, `${name} must use the shared approval poll`);
  assert.doesNotMatch(surface, /claimNative(Machine|Tailnet|Nearby)Access/, `${name} must not poll for itself`);
  assert.doesNotMatch(surface, /denied this request/, `${name} must not carry its own denial wording`);
  // The drifted copy said "The other Mac denied this request" / "…at the
  // other Mac". Approval outcomes are machine-neutral now; unrelated LAN
  // pairing copy is not what this checks.
  assert.doesNotMatch(surface, /at the other Mac/, `${name} must not carry its own expiry wording`);
}
assert.match(connectScreen, /scanPairingCode/,
  'ConnectScreen.tsx must use consent-by-possession pairing');
assert.doesNotMatch(connectScreen, /claimNative(Machine|Tailnet|Nearby)Access/,
  'ConnectScreen.tsx must not reintroduce a request/accept poll');

console.log('shared contracts smoke: ok');
