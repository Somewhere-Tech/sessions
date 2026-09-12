// CAPABILITY: keeping a cold-start frame must not cost the machine gigabytes.
//
// From the storage inventory on the Mini, 11 September: WebKit's
// localstorage.sqlite3-wal was 8,503,029,072 bytes against a 1,531,904 byte
// database, and the largest key in it was `sessions:sessions-cache:v3` at about
// 1.43 MB. The app refreshes every three seconds and wrote that whole value
// each time, changed or not. These tests are about the writes: how many, how
// big, and when.
import { beforeEach, describe, expect, it, vi } from 'vitest';
import {
  CACHED_ROWS_PER_MACHINE,
  CACHE_WRITE_INTERVAL_MS,
  boundedMachine,
  cacheMachineSessions,
  flushCachedSessions,
  lastCachedMachine,
  migrateLegacyCaches,
  readMachineCache
} from '../../src/store/sessionCache';
import { makeSession } from './fake-daemon';
import type { SessionInfo } from '../../src/types';

/** Counts what actually reaches storage, which is what the WAL counts too. */
function countingStorage(): { writes: Array<{ key: string; bytes: number }>; removed: string[] } {
  const writes: Array<{ key: string; bytes: number }> = [];
  const removed: string[] = [];
  // jsdom's localStorage is a proxy; the methods are on Storage.prototype, so
  // that is where a spy has to sit to see what the app actually writes.
  const setItem = Storage.prototype.setItem;
  const removeItem = Storage.prototype.removeItem;
  vi.spyOn(Storage.prototype, 'setItem').mockImplementation(function (this: Storage, key: string, value: string) {
    writes.push({ key, bytes: value.length });
    setItem.call(this, key, value);
  });
  vi.spyOn(Storage.prototype, 'removeItem').mockImplementation(function (this: Storage, key: string) {
    removed.push(key);
    removeItem.call(this, key);
  });
  return { writes, removed };
}

function fleet(count: number, machine: string, since = Date.now()): SessionInfo[] {
  return Array.from({ length: count }, (_, index) => {
    const session = makeSession({
      id: `${machine}-${index}`,
      name: `Rebuilding the index ${index}`,
      tool: index % 2 === 0 ? 'claude-code' : 'codex'
    });
    session.cwd = `/Users/somebody/projects/repo-${index % 17}`;
    session.createdAt = since - index * 60_000;
    session.lastDataAt = since - index * 30_000;
    session.lastSummary = `Read ${index} files and wrote a summary of what changed in the build`;
    return session;
  });
}

/**
 * A row the size real ones are. The Mini's cache held 594 sessions in about
 * 1.43 MB — roughly 2.4 KB each — and that is what the fields this cache used
 * to carry cost: a long last-message summary, a worktree and its repo, the
 * creator ancestry, model and effort, the ending bookkeeping.
 */
function heavyFleet(count: number, machine: string, since = Date.now()): SessionInfo[] {
  return fleet(count, machine, since).map((session, index) => ({
    ...session,
    description: `Continue the release audit for build ${index}. `.repeat(4),
    args: ['--dangerously-skip-permissions', '--resume', `${machine}-conversation-${index}`],
    cols: 213, rows: 58, pid: 40_000 + index,
    runnerProtocol: 7, runnerVersion: '0.2.27',
    model: 'claude-opus-5', effort: 'high', fast: false,
    conversationId: `4f1a${index}-cccc-4ccc-8ccc-cccccccccccc`,
    profile: index % 5 === 0 ? 'work' : undefined,
    configDir: `/Users/somebody/Library/Application Support/Sessions/profiles/claude/work`,
    worktreePath: `/Users/somebody/Sessions-wt/lane-${index}`,
    branch: `codex/lane-${index}`, base: 'main',
    sourceRepo: 'git@github.com:somewhere-tech/sessions.git',
    creatorKind: 'session', creatorId: `${machine}-${Math.max(0, index - 1)}`,
    creatorAncestry: [`${machine}-0`, `${machine}-1`, `${machine}-${Math.max(0, index - 1)}`],
    rootCreatorKind: 'external', rootCreatorId: 'test:local-user',
    provenanceStatus: 'rooted',
    idleDetail: 'The agent finished its turn and is waiting for the next instruction.',
    endedByKind: 'session', endedById: `${machine}-0`, endedByName: 'Release supervisor',
    endedByClient: 'desktop', endReason: 'work complete', endOperationId: `op-${index}`,
    lastSummary: `I read the diff for lane ${index}, ran the gates, and wrote up what changed. ` +
      'The listing stage timing is in place, the ledger cache is keyed on the high-water mark, '.repeat(9),
    claudeCustomTitle: `Lane ${index} release audit`,
    claudeAiTitle: `Reviewing the release audit for lane ${index}`
  }));
}

describe('capability: the session cache is written on change, not on a timer', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    window.localStorage.clear();
    vi.useRealTimers();
  });

  it('writes once, then not at all while the answer is identical', () => {
    vi.useFakeTimers();
    const storage = countingStorage();
    const machine = 'identical';
    const sessions = fleet(40, machine);

    cacheMachineSessions(machine, sessions, sessions[0]!.id);
    const afterFirst = storage.writes.filter((write) => write.key.includes(machine)).length;
    expect(afterFirst).toBe(1);

    // Twenty refreshes over a minute, the way the app polls, with nothing
    // having changed on the daemon.
    for (let tick = 0; tick < 20; tick += 1) {
      cacheMachineSessions(machine, sessions, sessions[0]!.id);
      vi.advanceTimersByTime(3_000);
    }
    flushCachedSessions();
    const total = storage.writes.filter((write) => write.key.includes(machine)).length;
    expect(total).toBe(afterFirst);
  });

  it('writes when something changed, at most once per interval', () => {
    vi.useFakeTimers();
    const storage = countingStorage();
    const machine = 'changing';
    const sessions = fleet(20, machine);
    cacheMachineSessions(machine, sessions, sessions[0]!.id);
    const writesFor = (): number => storage.writes.filter((write) => write.key.includes(machine)).length;
    expect(writesFor()).toBe(1);

    // A session's activity moves on every poll — the busy case.
    for (let tick = 1; tick <= 5; tick += 1) {
      sessions[0]!.lastDataAt = Date.now() + tick * 1_000;
      cacheMachineSessions(machine, [...sessions], sessions[0]!.id);
      vi.advanceTimersByTime(3_000);
    }
    // Still one write: the debounce is holding the changes, not dropping them.
    expect(writesFor()).toBe(1);

    vi.advanceTimersByTime(CACHE_WRITE_INTERVAL_MS);
    expect(writesFor()).toBe(2);
    // And the write that landed carries the newest activity, not the oldest.
    const stored = readMachineCache(machine);
    expect(stored?.sessions.find((row) => row.id === sessions[0]!.id)?.lastDataAt)
      .toBe(sessions[0]!.lastDataAt);
  });

  it('flushes what it is holding when the tab goes away', () => {
    vi.useFakeTimers();
    const storage = countingStorage();
    const machine = 'closing';
    const sessions = fleet(10, machine);
    cacheMachineSessions(machine, sessions, sessions[0]!.id);
    sessions[0]!.name = 'Renamed right before the tab closed';
    cacheMachineSessions(machine, [...sessions], sessions[0]!.id);
    expect(storage.writes.filter((write) => write.key.includes(machine)).length).toBe(1);

    window.dispatchEvent(new Event('pagehide'));

    expect(storage.writes.filter((write) => write.key.includes(machine)).length).toBe(2);
    expect(readMachineCache(machine)?.sessions.find((row) => row.id === sessions[0]!.id)?.name)
      .toBe('Renamed right before the tab closed');
  });
});

describe('capability: the cache is per machine, bounded, and hydrates', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    window.localStorage.clear();
    vi.useRealTimers();
  });

  it('keeps one key per machine and hydrates the one last used', () => {
    const alpha = fleet(5, 'alpha');
    const beta = fleet(5, 'beta');
    cacheMachineSessions('alpha', alpha, alpha[0]!.id);
    cacheMachineSessions('beta', beta, beta[0]!.id);
    flushCachedSessions();

    expect(window.localStorage.getItem('sessions:cache:v4:alpha')).not.toBeNull();
    expect(window.localStorage.getItem('sessions:cache:v4:beta')).not.toBeNull();
    expect(lastCachedMachine()).toBe('beta');
    expect(readMachineCache('alpha')?.sessions).toHaveLength(5);
    // One machine's rows never appear under another's key.
    expect(readMachineCache('alpha')?.sessions.every((row) => row.id.startsWith('alpha'))).toBe(true);
  });

  it('bounds a big machine and keeps what must survive', () => {
    const sessions = heavyFleet(594, 'mini');
    const pinned = sessions[500]!;
    pinned.pinned = true;
    const active = sessions[593]!;

    const bounded = boundedMachine(sessions, active.id);
    expect(bounded.sessions).toHaveLength(CACHED_ROWS_PER_MACHINE);
    expect(bounded.sessions.some((row) => row.id === pinned.id)).toBe(true);
    expect(bounded.sessions.some((row) => row.id === active.id)).toBe(true);
    // Newest first: the row dropped is an old one, not a recent one.
    expect(bounded.sessions.some((row) => row.id === sessions[0]!.id)).toBe(true);

    // The measurement this slice exists for: the Mini's 594 rows, before and
    // after. "Before" is the shape the v3 cache wrote — every field of every
    // row, every machine in one value.
    const before = JSON.stringify({
      version: 3, lastServerId: 'mini', machines: { mini: { sessions, activeId: active.id } }
    }).length;
    const after = JSON.stringify(bounded).length;
    console.log(`594 rows: v3 value ${before} bytes, v4 slice ${after} bytes ` +
      `(${(before / after).toFixed(1)}x smaller, ${Math.round(before / sessions.length)} B/row before)`);
    // The fixture is the size the Mini's really was: about 1.4 MB for 594 rows.
    expect(before).toBeGreaterThan(1_200_000);
    // And a slice is a fraction of it — the bytes a first frame actually needs.
    expect(after).toBeLessThan(before / 5);
  });

  it('moves the one big key to per-machine slices and deletes it', () => {
    const sessions = fleet(12, 'old');
    window.localStorage.setItem('sessions:sessions-cache:v3', JSON.stringify({
      version: 3, lastServerId: 'old', machines: { old: { sessions, activeId: sessions[2]!.id } }
    }));
    window.localStorage.setItem('sessions:sessions-cache:v2', JSON.stringify({ serverId: 'old', sessions, activeId: null }));
    const storage = countingStorage();

    migrateLegacyCaches();

    expect(window.localStorage.getItem('sessions:sessions-cache:v3')).toBeNull();
    expect(window.localStorage.getItem('sessions:sessions-cache:v2')).toBeNull();
    expect(storage.removed).toContain('sessions:sessions-cache:v3');
    const migrated = readMachineCache('old');
    expect(migrated?.sessions).toHaveLength(12);
    expect(migrated?.activeId).toBe(sessions[2]!.id);
    expect(lastCachedMachine()).toBe('old');
    // Migration is not a second copy: the rows kept are the bounded ones.
    expect(Object.keys(migrated!.sessions[0]!)).not.toContain('runnerVersion');
  });

  it('never persists the parts of a row a first frame does not draw', () => {
    const sessions = fleet(3, 'shape');
    sessions[0]!.lastSummary = 'x'.repeat(4_000);
    cacheMachineSessions('shape', sessions, sessions[0]!.id);
    flushCachedSessions();

    const row = readMachineCache('shape')!.sessions[0]!;
    expect(row.lastSummary!.length).toBeLessThanOrEqual(160);
    for (const absent of ['pid', 'cols', 'rows', 'args', 'runnerVersion', 'exitCode', 'model']) {
      expect(row).not.toHaveProperty(absent);
    }
    // And what the first frame does draw is all there.
    expect(row.id).toBe(sessions[0]!.id);
    expect(row.name).toBe(sessions[0]!.name);
    expect(row.tool).toBe(sessions[0]!.tool);
    expect(row.cwd).toBe(sessions[0]!.cwd);
  });
});
