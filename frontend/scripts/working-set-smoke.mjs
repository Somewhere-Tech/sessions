import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { build } from 'esbuild';

const scratch = await mkdtemp(join(tmpdir(), 'sessions-working-set-smoke-'));
const output = join(scratch, 'working-set.mjs');

try {
  await build({
    entryPoints: ['src/lib/workingSet.ts'],
    bundle: true,
    format: 'esm',
    outfile: output,
    platform: 'node',
    target: 'node20',
    logLevel: 'silent'
  });

  const { collapseConversationRuntimes, groupWorkingSet, humanEngagementAt, isAgentLedChild } = await import(`${pathToFileURL(output).href}?v=${Date.now()}`);
  const session = (id, values = {}) => ({
    id,
    exited: false,
    createdAt: 1,
    lastDataAt: 1,
    ...values
  });
  const sessions = [
    session('aside-manager', { setAsideAt: 100 }),
    session('live-child', { parentSessionId: 'aside-manager' }),
    session('finished-child', { parentSessionId: 'live-child', exited: true }),
    session('aside-child', { parentSessionId: 'aside-manager', setAsideAt: 110 }),
    session('ended-manager', { exited: true }),
    session('live-grandchild', { parentSessionId: 'ended-manager' }),
    session('ended-alone', { exited: true, setAsideAt: 90 })
  ];

  const grouped = groupWorkingSet(sessions, [], []);
  assert.deepEqual(grouped.runningRoots.map(({ id }) => id), ['live-child', 'ended-manager']);
  assert.deepEqual(grouped.setAsideRoots.map(({ id }) => id), ['aside-manager']);
  assert.deepEqual(grouped.ended.map(({ id }) => id), ['ended-alone']);
  assert.deepEqual(grouped.pinnedRoots.map(({ id }) => id), [], 'no pins, no Pinned group');
  assert.equal(grouped.runningIds.has('live-grandchild'), true);
  assert.equal(grouped.runningIds.has('finished-child'), true, 'finished children stay grouped under an in-focus manager');
  assert.equal(grouped.setAsideIds.has('aside-child'), true);

  const opened = groupWorkingSet(sessions, ['aside-manager'], []);
  assert.equal(opened.runningIds.has('aside-manager'), true);
  assert.equal(opened.setAsideIds.has('aside-manager'), false);

  // A pin lifts a session out of whichever group the machinery filed it under —
  // including the Later pile it had been swept into — and into the user's own.
  // The details of that move belong to scripts/pin-smoke.mjs; what matters here
  // is that this reducer no longer leaves it in two places or in the wrong one.
  const pinned = groupWorkingSet(sessions, [], ['aside-child']);
  assert.equal(pinned.pinnedIds.has('aside-child'), true);
  assert.equal(pinned.runningIds.has('aside-child'), false);
  assert.equal(pinned.setAsideIds.has('aside-child'), false);
  assert.deepEqual(pinned.pinnedRoots.map(({ id }) => id), ['aside-child']);

  const openedHistory = groupWorkingSet(sessions, ['ended-alone'], []);
  assert.equal(openedHistory.runningIds.has('ended-alone'), true, 'open history stays in focus while it is being read');
  assert.equal(openedHistory.ended.some(({ id }) => id === 'ended-alone'), false);

  assert.equal(humanEngagementAt(session('human', {
    createdAt: 10,
    lastDataAt: 999,
    lastUserMessageAt: 25,
    lastHumanMessageAt: 20
  })), 20);
  assert.equal(humanEngagementAt(session('provider-only', {
    createdAt: 10,
    lastUserMessageAt: 25,
    lastHumanMessageAt: null
  })), 10);
  assert.equal(humanEngagementAt(session('legacy', {
    createdAt: 10,
    lastUserMessageAt: 25
  })), 25);
  assert.equal(isAgentLedChild(session('lane', { kind: 'lane' })), true);
  assert.equal(isAgentLedChild(session('explicit-user', { delegationKind: 'user' })), false);

  const oneConversation = collapseConversationRuntimes([
    session('old-runtime', {
      tool: 'claude-code', claudeSessionId: 'provider-id', exited: true,
      createdAt: 10, parentSessionId: 'manager'
    }),
    session('live-runtime', {
      tool: 'claude-code', conversationId: 'provider-id', createdAt: 20,
      parentSessionId: 'old-runtime'
    }),
    session('child', { parentSessionId: 'old-runtime' })
  ]);
  assert.deepEqual(oneConversation.map(({ id }) => id), ['live-runtime', 'child']);
  assert.equal(oneConversation.find(({ id }) => id === 'live-runtime').parentSessionId, 'manager');
  assert.equal(oneConversation.find(({ id }) => id === 'child').parentSessionId, 'live-runtime');

  const cycle = [
    session('cycle-a', { parentSessionId: 'cycle-b', setAsideAt: 1 }),
    session('cycle-b', { parentSessionId: 'cycle-a', exited: true })
  ];
  const cycleGroups = groupWorkingSet(cycle, [], []);
  assert.equal(cycleGroups.runningIds.size, 0);
  assert.equal(cycleGroups.setAsideIds.has('cycle-a'), true);

  console.log('working-set smoke: ok');
} finally {
  await rm(scratch, { recursive: true, force: true });
}
