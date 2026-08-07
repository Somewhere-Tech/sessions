import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { build } from 'esbuild';

const scratch = await mkdtemp(join(tmpdir(), 'sessions-pin-smoke-'));
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

  const { groupWorkingSet, isPinned, pinnedFirst, pinnedSessionIds } =
    await import(`${pathToFileURL(output).href}?v=${Date.now()}`);
  const session = (id, values = {}) => ({
    id,
    exited: false,
    createdAt: 1,
    lastDataAt: 1,
    ...values
  });

  // The pin is the daemon's boolean. A daemon too old to send it must read as
  // "not pinned" rather than as undefined leaking into a filter.
  assert.equal(isPinned(session('pinned', { pinned: true })), true);
  assert.equal(isPinned(session('plain', { pinned: false })), false);
  assert.equal(isPinned(session('old-daemon')), false);

  // The list is what the mark is for: pinned first, everything else in exactly
  // the order it already had.
  const ordered = [
    session('first-unpinned'),
    session('pinned-late', { pinned: true }),
    session('second-unpinned'),
    session('pinned-later', { pinned: true })
  ];
  assert.deepEqual(
    pinnedFirst(ordered).map(({ id }) => id),
    ['pinned-late', 'pinned-later', 'first-unpinned', 'second-unpinned'],
    'pinned sessions must float to the top without reordering either half'
  );
  assert.deepEqual(
    ordered.map(({ id }) => id),
    ['first-unpinned', 'pinned-late', 'second-unpinned', 'pinned-later'],
    'pinnedFirst must not mutate its input'
  );
  assert.deepEqual(pinnedSessionIds(ordered), ['pinned-late', 'pinned-later']);

  // A pinned session stays in focus even when it was set aside, which is the
  // behaviour groupWorkingSet already had and that the real pins now feed.
  const sessions = [
    session('aside-and-pinned', { setAsideAt: 100, pinned: true }),
    session('aside-only', { setAsideAt: 100 }),
    session('running', {})
  ];
  const grouped = groupWorkingSet(sessions, [], pinnedSessionIds(sessions));
  assert.equal(grouped.runningIds.has('aside-and-pinned'), true,
    'a pinned session that was also set aside must stay in the focused group');
  assert.equal(grouped.setAsideIds.has('aside-and-pinned'), false);
  assert.equal(grouped.setAsideIds.has('aside-only'), true);

  // An ended conversation the user pinned stays reachable rather than dropping
  // into the ended pile, matching how an open one behaves.
  const withEnded = [session('ended-pinned', { exited: true, pinned: true })];
  const endedGroups = groupWorkingSet(withEnded, [], pinnedSessionIds(withEnded));
  assert.equal(endedGroups.runningIds.has('ended-pinned'), true);
  assert.equal(endedGroups.ended.some(({ id }) => id === 'ended-pinned'), false);

  console.log('pin smoke: ok');
} finally {
  await rm(scratch, { recursive: true, force: true });
}
