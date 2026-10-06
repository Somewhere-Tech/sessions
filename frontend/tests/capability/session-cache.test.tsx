// CAPABILITY: keeping a cold-start frame must not cost the machine gigabytes.
//
// The cache is one bounded value per machine, and the app refreshes every
// three seconds. Each localStorage write stores the whole value again, so what
// matters is how many writes a refresh loop asks for, how big they are, and
// when they happen. These tests count what reaches storage; they say nothing
// about how WebKit keeps or checkpoints it.
import { beforeEach, describe, expect, it, vi } from 'vitest';
import {
  CACHED_ROWS_PER_MACHINE,
  CACHE_ACTIVITY_INTERVAL_MS,
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

  it('writes a change to what the frame shows at most once per interval', () => {
    vi.useFakeTimers();
    const storage = countingStorage();
    const machine = 'changing';
    const sessions = fleet(20, machine);
    cacheMachineSessions(machine, sessions, sessions[0]!.id);
    const writesFor = (): number => storage.writes.filter((write) => write.key.includes(machine)).length;
    expect(writesFor()).toBe(1);

    // A rename on every poll — a change the first frame draws.
    for (let tick = 1; tick <= 5; tick += 1) {
      sessions[0]!.name = `Renamed ${tick}`;
      cacheMachineSessions(machine, [...sessions], sessions[0]!.id);
      vi.advanceTimersByTime(3_000);
    }
    // Still one write: the debounce is holding the changes, not dropping them.
    expect(writesFor()).toBe(1);

    vi.advanceTimersByTime(CACHE_WRITE_INTERVAL_MS);
    expect(writesFor()).toBe(2);
    // And the write that landed carries the newest name, not the oldest.
    expect(readMachineCache(machine)?.sessions.find((row) => row.id === sessions[0]!.id)?.name)
      .toBe('Renamed 5');
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

/** The four workers taking turns: rows spread through the list, not the top four. */
const WORKERS = [0, 7, 14, 21];

/**
 * The app's refresh loop: one cacheMachineSessions call every three seconds,
 * each with a fresh copy of the rows the way a listing response is. `step`
 * mutates the daemon's view before each refresh.
 */
function refreshFor(
  machine: string, sessions: SessionInfo[], durationMs: number,
  step: (tick: number) => void, activeId: () => string | null = () => sessions[0]!.id
): void {
  for (let tick = 0; tick < durationMs / 3_000; tick += 1) {
    step(tick);
    cacheMachineSessions(machine, sessions.map((session) => ({ ...session })), activeId());
    vi.advanceTimersByTime(3_000);
  }
}

/** Refresh `n` moves only worker n%4's activity, so their order rotates every time. */
function takeTurns(sessions: SessionInfo[], workers = WORKERS): (tick: number) => void {
  for (const index of workers) sessions[index]!.working = true;
  return (tick) => {
    const worker = sessions[workers[tick % workers.length]!]!;
    worker.lastDataAt = Date.now();
    worker.lastAgentMessageAt = Date.now();
    worker.lastSummary = `Step ${tick}: read the diff and ran the gates again`;
  };
}

function cachedRow(machine: string, id: string) {
  return readMachineCache(machine)?.sessions.find((row) => row.id === id);
}

describe('capability: activity alone waits; what the frame shows does not', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    window.localStorage.clear();
    vi.useFakeTimers();
  });

  it('four workers taking turns for an hour rewrite the value once per activity interval', () => {
    const storage = countingStorage();
    const machine = 'turns';
    const sessions = fleet(312, machine);
    const step = takeTurns(sessions);

    // The fixture really does reorder: two consecutive refreshes put a
    // different worker first, so a signature over the stored order would churn.
    step(0);
    const first = boundedMachine(sessions, null).sessions[0]!.id;
    vi.advanceTimersByTime(3_000);
    step(1);
    expect(boundedMachine(sessions, null).sessions[0]!.id).not.toBe(first);

    refreshFor(machine, sessions, 3_600_000, step);
    const writes = storage.writes.filter((write) => write.key === `sessions:cache:v4:${machine}`);
    // One on the first refresh, then one per activity interval — not one per
    // thirty seconds (120 an hour).
    expect(writes.length).toBeLessThanOrEqual(1 + 3_600_000 / CACHE_ACTIVITY_INTERVAL_MS);
    expect(writes.length).toBeGreaterThanOrEqual(3_600_000 / CACHE_ACTIVITY_INTERVAL_MS);

    // Going away writes what was held: every worker's newest activity.
    flushCachedSessions();
    for (const index of WORKERS) {
      expect(cachedRow(machine, sessions[index]!.id)?.lastDataAt).toBe(sessions[index]!.lastDataAt);
      expect(cachedRow(machine, sessions[index]!.id)?.lastSummary).toBe(sessions[index]!.lastSummary);
    }
  });

  it('a turn starting or finishing and a moving summary wait with the activity', () => {
    const storage = countingStorage();
    const machine = 'turn-flips';
    const sessions = fleet(30, machine);
    const writesFor = (): number => storage.writes.filter((write) => write.key.includes(machine)).length;
    cacheMachineSessions(machine, sessions, sessions[0]!.id);
    expect(writesFor()).toBe(1);

    const busy = takeTurns(sessions);
    refreshFor(machine, sessions, CACHE_ACTIVITY_INTERVAL_MS - 6_000, (tick) => {
      busy(tick);
      if (tick % 5 === 0) {
        const worker = sessions[WORKERS[(tick / 5) % WORKERS.length]!]!;
        worker.working = !worker.working;
        worker.idleReason = worker.working ? undefined : 'completed';
      }
    });
    expect(writesFor()).toBe(1);

    // With no further refresh, the held activity lands on its own: a daemon
    // that stopped answering stops the refreshes, not the write.
    vi.advanceTimersByTime(6_000);
    expect(writesFor()).toBe(2);
    expect(cachedRow(machine, sessions[0]!.id)?.lastDataAt).toBe(sessions[0]!.lastDataAt);
  });

  type Change = [string, (sessions: SessionInfo[], view: { active: string }) => void,
    (machine: string, sessions: SessionInfo[]) => void];
  const changes: Change[] = [
    ['rename', (s) => { s[3]!.name = 'Release audit, second pass'; },
      (m, s) => expect(cachedRow(m, s[3]!.id)?.name).toBe('Release audit, second pass')],
    ['provider title', (s) => { s[3]!.claudeCustomTitle = 'Audit'; },
      (m, s) => expect(cachedRow(m, s[3]!.id)?.claudeCustomTitle).toBe('Audit')],
    ['selecting another row', (s, view) => { view.active = s[5]!.id; },
      (m, s) => expect(readMachineCache(m)?.activeId).toBe(s[5]!.id)],
    ['pin', (s) => { s[9]!.pinned = true; },
      (m, s) => expect(cachedRow(m, s[9]!.id)?.pinned).toBe(true)],
    ['unpin', (s) => { s[1]!.pinned = false; },
      (m, s) => expect(cachedRow(m, s[1]!.id)?.pinned).toBe(false)],
    ['set aside', (s) => { s[4]!.setAsideAt = Date.now(); },
      (m, s) => expect(cachedRow(m, s[4]!.id)?.setAsideAt).toBe(s[4]!.setAsideAt)],
    ['created', (s) => { s.push(makeSession({ id: 'frame-new', name: 'New lane', createdAt: Date.now() - 600_000_000, lastDataAt: Date.now() - 600_000_000 })); },
      (m) => expect(cachedRow(m, 'frame-new')?.name).toBe('New lane')],
    ['ended', (s) => { s[6]!.exited = true; s[6]!.exitedAt = Date.now(); },
      (m, s) => expect(cachedRow(m, s[6]!.id)?.exited).toBe(true)],
    ['delegated', (s) => { s[8]!.parentSessionId = s[2]!.id; s[8]!.delegationKind = 'agent'; },
      (m, s) => expect(cachedRow(m, s[8]!.id)?.parentSessionId).toBe(s[2]!.id)],
    ['shown under another manager', (s) => { s[8]!.displayParentSessionId = s[3]!.id; },
      (m, s) => expect(cachedRow(m, s[8]!.id)?.displayParentSessionId).toBe(s[3]!.id)],
    ['account', (s) => { s[3]!.profile = 'work'; },
      (m, s) => expect(cachedRow(m, s[3]!.id)?.profile).toBe('work')],
    ['waiting on a person', (s) => { s[2]!.working = false; s[2]!.idleReason = 'needs-input'; },
      (m, s) => expect(cachedRow(m, s[2]!.id)?.idleReason).toBe('needs-input')],
    ['answered', (s) => { s[10]!.working = true; s[10]!.idleReason = undefined; },
      (m, s) => expect(cachedRow(m, s[10]!.id)?.idleReason).toBeUndefined()],
    ['failed', (s) => { s[2]!.working = false; s[2]!.idleReason = 'failed'; },
      (m, s) => expect(cachedRow(m, s[2]!.id)?.idleReason).toBe('failed')]
  ];

  it.each(changes)('writes %s within the write interval while workers keep the activity moving', (label, change, check) => {
    const storage = countingStorage();
    const machine = `frame-${label.replace(/\W+/g, '-')}`;
    const sessions = fleet(30, machine).map((session, index) => (
      session.id === `${machine}-frame-new` ? session : { ...session, id: `${index}-${machine}` }
    ));
    sessions[1]!.pinned = true;
    sessions[10]!.idleReason = 'needs-input';
    const view = { active: sessions[0]!.id };
    const writesFor = (): number => storage.writes.filter((write) => write.key.includes(machine)).length;
    const busy = takeTurns(sessions);

    // Settle: the first write, then a minute of activity alone, which waits.
    refreshFor(machine, sessions, 60_000, busy, () => view.active);
    expect(writesFor()).toBe(1);

    change(sessions, view);
    refreshFor(machine, sessions, CACHE_WRITE_INTERVAL_MS + 3_000, busy, () => view.active);
    expect(writesFor()).toBe(2);
    check(machine, sessions);
    // The write is the whole value, so it carries the newest activity too.
    expect(cachedRow(machine, sessions[WORKERS[0]!]!.id)?.lastDataAt)
      .toBeGreaterThan(sessions[WORKERS[0]!]!.createdAt);
  });

  it('keeps machines apart: one machine working never writes or delays another', () => {
    const storage = countingStorage();
    const busy = fleet(40, 'busy-host');
    const quiet = fleet(40, 'quiet-host');
    const step = takeTurns(busy);
    const forKey = (machine: string): number =>
      storage.writes.filter((write) => write.key === `sessions:cache:v4:${machine}`).length;

    for (let tick = 0; tick < 200; tick += 1) {
      step(tick);
      if (tick === 100) quiet[2]!.name = 'Renamed on the quiet machine';
      cacheMachineSessions('busy-host', busy.map((session) => ({ ...session })), busy[0]!.id);
      cacheMachineSessions('quiet-host', quiet.map((session) => ({ ...session })), quiet[0]!.id);
      vi.advanceTimersByTime(3_000);
    }
    expect(forKey('quiet-host')).toBe(2);
    expect(cachedRow('quiet-host', quiet[2]!.id)?.name).toBe('Renamed on the quiet machine');
    expect(forKey('busy-host')).toBeLessThanOrEqual(2);
    expect(readMachineCache('busy-host')?.sessions.every((row) => row.id.startsWith('busy-host'))).toBe(true);
  });

  it('survives storage that refuses writes, retrying at the write pace and never throwing', () => {
    const machine = 'refusing';
    const sessions = fleet(30, machine);
    cacheMachineSessions(machine, sessions, sessions[0]!.id);
    const before = readMachineCache(machine);
    let attempts = 0;
    let refusing = true;
    const setItem = Storage.prototype.setItem;
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(function (this: Storage, key: string, value: string) {
      if (key.startsWith('sessions:cache:v4:')) {
        attempts += 1;
        if (refusing) throw new DOMException('The quota has been exceeded.', 'QuotaExceededError');
      }
      setItem.call(this, key, value);
    });

    vi.advanceTimersByTime(CACHE_WRITE_INTERVAL_MS);
    expect(() => refreshFor(machine, sessions, 120_000, (tick) => {
      sessions[0]!.name = `Renamed during the outage ${tick}`;
    })).not.toThrow();
    // A structural change pending for two minutes: one attempt per interval,
    // not one per refresh.
    expect(attempts).toBeGreaterThan(0);
    expect(attempts).toBeLessThanOrEqual(1 + 120_000 / CACHE_WRITE_INTERVAL_MS);
    expect(() => flushCachedSessions()).not.toThrow();
    // The last frame that did land is still readable.
    expect(readMachineCache(machine)?.sessions).toEqual(before?.sessions);

    // Storage comes back: the newest rows land within the write interval.
    refusing = false;
    refreshFor(machine, sessions, CACHE_WRITE_INTERVAL_MS + 3_000, () => {
      sessions[0]!.name = 'After the outage';
    });
    expect(cachedRow(machine, sessions[0]!.id)?.name).toBe('After the outage');
  });

  it('writes the newest activity when the tab is hidden or closing, and nothing when nothing changed', () => {
    const storage = countingStorage();
    const machine = 'going-away';
    const sessions = fleet(30, machine);
    const writesFor = (): number => storage.writes.filter((write) => write.key.includes(machine)).length;
    const step = takeTurns(sessions);
    refreshFor(machine, sessions, 90_000, step);
    expect(writesFor()).toBe(1);

    const visibility = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden');
    // The browser fires it at the document, bubbling, so it reaches window.
    document.dispatchEvent(new Event('visibilitychange', { bubbles: true }));
    expect(writesFor()).toBe(2);
    for (const index of WORKERS) {
      expect(cachedRow(machine, sessions[index]!.id)?.lastDataAt).toBe(sessions[index]!.lastDataAt);
    }
    visibility.mockRestore();

    // Hidden again with nothing new: no write.
    cacheMachineSessions(machine, sessions.map((session) => ({ ...session })), sessions[0]!.id);
    window.dispatchEvent(new Event('pagehide'));
    expect(writesFor()).toBe(2);

    // One more turn, then the page closes: it lands.
    step(99);
    cacheMachineSessions(machine, sessions.map((session) => ({ ...session })), sessions[0]!.id);
    window.dispatchEvent(new Event('pagehide'));
    expect(writesFor()).toBe(3);
    expect(cachedRow(machine, sessions[WORKERS[99 % 4]!]!.id)?.lastDataAt)
      .toBe(sessions[WORKERS[99 % 4]!]!.lastDataAt);
  });

  it('keeps a big fleet bounded while workers deep in the list take turns', () => {
    const storage = countingStorage();
    const machine = 'deep';
    const sessions = heavyFleet(594, machine);
    const pinned = sessions[520]!;
    pinned.pinned = true;
    const active = sessions[593]!;
    // One worker starts beyond the 300 rows kept; its first output brings it in.
    const workers = [0, 150, 299, 450];
    const step = takeTurns(sessions, workers);

    refreshFor(machine, sessions, 3_600_000, step, () => active.id);
    flushCachedSessions();
    const writes = storage.writes.filter((write) => write.key === `sessions:cache:v4:${machine}`);
    // The first write, one per activity interval, one for the row that joined
    // the kept set, and the closing flush.
    expect(writes.length).toBeLessThanOrEqual(1 + 3_600_000 / CACHE_ACTIVITY_INTERVAL_MS + 2);
    const stored = readMachineCache(machine)!;
    expect(stored.sessions.length).toBe(CACHED_ROWS_PER_MACHINE);
    expect(stored.sessions.some((row) => row.id === pinned.id)).toBe(true);
    expect(stored.sessions.some((row) => row.id === active.id)).toBe(true);
    for (const index of workers) {
      expect(stored.sessions.some((row) => row.id === sessions[index]!.id)).toBe(true);
    }
    expect(Math.max(...writes.map((write) => write.bytes))).toBeLessThan(400_000);
  });

  it('starts cold from what the last run wrote, and does not start rewriting it', async () => {
    const machine = 'cold';
    const sessions = fleet(40, machine);
    sessions[3]!.pinned = true;
    cacheMachineSessions(machine, sessions, sessions[3]!.id);
    flushCachedSessions();

    // A new page: a fresh copy of the module, nothing in memory but storage.
    vi.resetModules();
    const fresh = await import('../../src/store/sessionCache');
    expect(fresh.lastCachedMachine()).toBe(machine);
    const cold = fresh.readMachineCache(machine)!;
    expect(cold.activeId).toBe(sessions[3]!.id);
    expect(cold.sessions.find((row) => row.id === sessions[3]!.id)?.pinned).toBe(true);
    expect(cold.sessions.map((row) => row.id)).toEqual(boundedMachine(sessions, sessions[3]!.id).sessions.map((row) => row.id));

    const storage = countingStorage();
    const step = takeTurns(sessions);
    for (let tick = 0; tick < (CACHE_ACTIVITY_INTERVAL_MS - 6_000) / 3_000; tick += 1) {
      step(tick);
      fresh.cacheMachineSessions(machine, sessions.map((session) => ({ ...session })), sessions[3]!.id);
      vi.advanceTimersByTime(3_000);
    }
    // The new page does not know what the old one wrote, so it writes once,
    // then holds activity like any other.
    expect(storage.writes.filter((write) => write.key.includes(machine)).length).toBe(1);
  });
});

describe('capability: the live listing is never held back by the cache', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    window.localStorage.clear();
    vi.useRealTimers();
  });

  it('shows the newest activity at once while the cached copy waits', async () => {
    // One module graph for the store, the fake and the cache it writes through.
    vi.resetModules();
    const { installFakeDaemon, useFakeMachines } = await import('./fake-daemon');
    const { useSessions } = await import('../../src/store/sessions');
    const { flushCachedSessions, readMachineCache } = await import('../../src/store/sessionCache');
    const worker = makeSession({ id: 'live-worker', name: 'Live worker', working: true });
    const machines = [{ id: 'live-host', name: 'MacBook', host: 'localhost', port: 8787, isDefault: true,
      sessions: [worker, makeSession({ id: 'live-other', name: 'Other' })] }];
    installFakeDaemon(machines);
    useFakeMachines(machines);

    await useSessions.getState().refresh();
    const firstCached = readMachineCache('live-host')?.sessions.find((row) => row.id === 'live-worker')?.lastDataAt;
    expect(firstCached).toBe(worker.lastDataAt);

    const later = worker.lastDataAt + 120_000;
    machines[0]!.sessions[0] = { ...worker, lastDataAt: later };
    await useSessions.getState().refresh();
    expect(useSessions.getState().sessions.find((session) => session.id === 'live-worker')?.lastDataAt).toBe(later);
    expect(readMachineCache('live-host')?.sessions.find((row) => row.id === 'live-worker')?.lastDataAt).toBe(firstCached);

    flushCachedSessions();
    expect(readMachineCache('live-host')?.sessions.find((row) => row.id === 'live-worker')?.lastDataAt).toBe(later);
  });
});
