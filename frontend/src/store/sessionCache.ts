import type { SessionInfo } from '../types';

// The first frame, written down — without rewriting it on every refresh.
//
// This is a disposable cold-start frame: what the tab strip and navigator draw
// for the second before the live listing lands. It is neither state nor
// history, and deleting it costs one empty first frame.
//
// It is still not free to keep. WebKit keeps localStorage in an SQLite
// database, so every setItem of this value writes all of it again, and the app
// refreshes every three seconds. When that database's log is checkpointed is
// WebKit's business; this module cannot see it or rely on it, so it limits
// what it asks to write instead.
//
// One key per machine, a bounded row, and two kinds of change:
//
// - What the frame shows — which rows, their names and titles, pins, set-aside,
//   ending, parent and delegation, the open row, a row waiting on you — is
//   written at most once per machine every thirty seconds.
// - Activity alone — timestamps, the last-message summary, a turn starting or
//   finishing — only moves clocks and reorders rows the live listing reorders
//   anyway, so it waits up to ten minutes.
//
// Both are written at once when the tab is hidden or closing. Nothing here
// changes what the person sees once the live listing has landed.

const KEY_PREFIX = 'sessions:cache:v4:';
// Which machine to draw before a machine has been chosen. Its own key, so the
// pointer never drags the rows along with it.
const LAST_MACHINE_KEY = 'sessions:cache:v4-last';
const LEGACY_FLEET_KEY = 'sessions:sessions-cache:v3';
const LEGACY_SINGLE_KEY = 'sessions:sessions-cache:v2';

/**
 * How many rows one machine keeps. The first frame is a tab strip and a
 * navigator; nobody reads row 301 before the live listing lands. Pinned rows
 * and the active row are kept whatever their age, then the newest fill the
 * rest. What is dropped is old rows on a machine with hundreds of them — they
 * come back with the first refresh, about a second later.
 */
export const CACHED_ROWS_PER_MACHINE = 300;

/** At most one write per machine per this interval, unless the tab is going. */
export const CACHE_WRITE_INTERVAL_MS = 30_000;

/**
 * How long a change to activity alone waits, when nothing the frame shows has
 * changed. A working agent moves its timestamps on every refresh; written each
 * time, the cache would be rewritten whole every thirty seconds for as long as
 * anything works.
 */
export const CACHE_ACTIVITY_INTERVAL_MS = 10 * 60_000;

// Text a first frame shows in one line. Anything longer is the conversation
// leaking into the cache, which is what once made this value megabytes.
const NAME_LIMIT = 120;
const SUMMARY_LIMIT = 160;
const PATH_LIMIT = 200;

/**
 * What the cold-start frame needs and nothing else: identity, what it is, where
 * it runs, when it last did something, and the last-message facts the row
 * shows. Deliberately absent — sizes, pids, protocol versions, model and
 * effort, worktree and repo details, creator ancestry, move and end
 * bookkeeping, exit codes, idle detail, provider fault state: none of it is
 * drawn before the live listing replaces this, and together it was most of the
 * bytes. One list, so what is stored and what is typed cannot drift apart.
 */
const CACHED_FIELDS = [
  'id', 'name', 'description', 'tags', 'cmd', 'cwd', 'tool', 'kind', 'profile',
  'createdAt', 'lastDataAt', 'lastUserMessageAt', 'lastHumanMessageAt', 'lastAgentMessageAt',
  'idleReason', 'lastSummary', 'working', 'exited', 'exitedAt', 'pinned', 'setAsideAt',
  'parentSessionId', 'displayParentSessionId', 'delegationKind', 'creatorKind', 'creatorId',
  'claudeCustomTitle', 'claudeAiTitle'
] as const;

/**
 * Fields that move while a session simply works. They are cached, so a first
 * frame can say "4 min ago", but a change to them alone is not a reason to
 * rewrite the value. idleReason is here for 'completed' and 'never-started'
 * only: needing you and having failed are kept in the shape below, so moving
 * into or out of either is written within the write interval like a rename,
 * not held behind the activity.
 */
const ACTIVITY_FIELDS = new Set<string>([
  'lastDataAt', 'lastUserMessageAt', 'lastHumanMessageAt', 'lastAgentMessageAt',
  'lastSummary', 'working', 'idleReason'
]);

export type CachedSession =
  Partial<Pick<SessionInfo, typeof CACHED_FIELDS[number]>>
  & { id: string; tool: SessionInfo['tool']; createdAt: number };

export interface CachedMachine {
  sessions: CachedSession[];
  activeId: string | null;
}

interface PendingWrite {
  sessions: SessionInfo[];
  activeId: string | null;
}

const pending = new Map<string, PendingWrite>();
const timers = new Map<string, { handle: number; due: number }>();
// When a write was last attempted, successful or not: a store refusing writes
// is retried at the same pace as one accepting them, not on every refresh.
const lastAttemptAt = new Map<string, number>();
// Signatures of what was last written: the whole value, so an unchanged
// refresh writes nothing, and its shape (below), so a refresh that changed only
// activity can wait. Length plus an FNV-1a hash: two different values of the
// same length colliding would cost one stale cold-start frame until the next
// change, which is cheaper than keeping a second copy of the string in memory.
const lastWritten = new Map<string, { value: string; shape: string }>();
let lastMachineWritten: string | null = null;

function keyFor(serverId: string): string {
  return KEY_PREFIX + serverId;
}

function signatureOf(text: string): string {
  let hash = 0x811c9dc5;
  for (let index = 0; index < text.length; index += 1) {
    hash ^= text.charCodeAt(index);
    hash = Math.imul(hash, 0x01000193);
  }
  return `${text.length}:${hash >>> 0}`;
}

function clip(value: string | undefined, limit: number): string | undefined {
  if (value === undefined) return undefined;
  return value.length > limit ? value.slice(0, limit) : value;
}

function limitFor(field: string): number {
  if (field === 'cwd') return PATH_LIMIT;
  if (field === 'lastSummary') return SUMMARY_LIMIT;
  return NAME_LIMIT;
}

function boundedSession(session: SessionInfo): CachedSession {
  const row: Record<string, unknown> = {};
  for (const field of CACHED_FIELDS) {
    const value = session[field];
    if (value === undefined) continue;
    row[field] = typeof value === 'string' ? clip(value, limitFor(field)) : value;
  }
  return row as CachedSession;
}

/**
 * What a first frame shows, without the activity: the rows kept, in id order —
 * the stored order follows activity, and four agents taking turns reorder it on
 * every refresh — each without its activity fields, but keeping a reason that
 * means a person is needed.
 */
function shapeOf(machine: CachedMachine): string {
  const rows = [...machine.sessions]
    .sort((left, right) => (left.id < right.id ? -1 : left.id > right.id ? 1 : 0))
    .map((row) => {
      const shape: Record<string, unknown> = {};
      for (const [field, value] of Object.entries(row)) {
        if (!ACTIVITY_FIELDS.has(field)) shape[field] = value;
      }
      if (row.idleReason === 'needs-input' || row.idleReason === 'failed') {
        shape.idleReason = row.idleReason;
      }
      return shape;
    });
  return JSON.stringify({ activeId: machine.activeId, rows });
}

function activityOf(session: SessionInfo): number {
  return session.lastDataAt || session.createdAt || 0;
}

/** The rows this machine keeps, newest first, with what must never be dropped. */
export function boundedMachine(
  sessions: SessionInfo[], activeId: string | null
): CachedMachine {
  const ordered = [...sessions].sort((left, right) => activityOf(right) - activityOf(left));
  const must = (session: SessionInfo): boolean => Boolean(session.pinned) || session.id === activeId;
  // Pinned rows and the open one are kept whatever their age, even past the
  // limit: a first frame missing the tab you are looking at is not a cache.
  const kept = [...ordered.filter(must), ...ordered.filter((session) => !must(session))];
  const limit = Math.max(CACHED_ROWS_PER_MACHINE, kept.filter(must).length);
  return { sessions: kept.slice(0, limit).map(boundedSession), activeId };
}

export function readMachineCache(serverId: string): CachedMachine | null {
  try {
    const raw = window.localStorage.getItem(keyFor(serverId));
    if (!raw) return null;
    const parsed = JSON.parse(raw) as CachedMachine | null;
    if (!parsed || !Array.isArray(parsed.sessions)) return null;
    return { sessions: parsed.sessions, activeId: parsed.activeId ?? null };
  } catch {
    // Corrupt or unavailable storage is the same as no cache: an empty first
    // frame, and the live listing a second later.
    return null;
  }
}

export function lastCachedMachine(): string | null {
  try {
    return window.localStorage.getItem(LAST_MACHINE_KEY);
  } catch {
    return null;
  }
}

/**
 * Record this machine's rows. Nothing is written when the bytes are identical
 * to the last ones written, which is what a refresh against an idle daemon
 * produces; a change to what the frame shows is written within thirty seconds,
 * and a change to activity alone within ten minutes.
 */
export function cacheMachineSessions(
  serverId: string | null, sessions: SessionInfo[], activeId: string | null
): void {
  if (!serverId) return;
  pending.set(serverId, { sessions, activeId });
  const since = Date.now() - (lastAttemptAt.get(serverId) ?? 0);
  if (since >= CACHE_WRITE_INTERVAL_MS) {
    writePending(serverId, false);
    return;
  }
  writeLater(serverId, CACHE_WRITE_INTERVAL_MS - since);
}

/** Write whatever is waiting, now, activity included: the tab is going away. */
export function flushCachedSessions(): void {
  for (const serverId of [...pending.keys()]) writePending(serverId, true);
}

// One timer per machine, due at the earliest time anything waiting may be
// written. A later request never pushes an earlier one back.
function writeLater(serverId: string, delay: number): void {
  const due = Date.now() + delay;
  const existing = timers.get(serverId);
  if (existing && existing.due <= due) return;
  if (existing) window.clearTimeout(existing.handle);
  const handle = window.setTimeout(() => {
    timers.delete(serverId);
    writePending(serverId, false);
  }, delay);
  timers.set(serverId, { handle, due });
}

function writePending(serverId: string, immediately: boolean): void {
  const entry = pending.get(serverId);
  if (!entry) return;
  const machine = boundedMachine(entry.sessions, entry.activeId);
  const text = JSON.stringify(machine);
  const value = signatureOf(text);
  const written = lastWritten.get(serverId);
  // An unchanged refresh is the common case: nothing to write, nothing to wait for.
  if (written?.value === value) {
    pending.delete(serverId);
    return;
  }
  const shape = signatureOf(shapeOf(machine));
  const since = Date.now() - (lastAttemptAt.get(serverId) ?? 0);
  if (!immediately && written?.shape === shape && since < CACHE_ACTIVITY_INTERVAL_MS) {
    // Only activity moved. Hold the newest rows; the timer or the tab going
    // away writes them.
    writeLater(serverId, CACHE_ACTIVITY_INTERVAL_MS - since);
    return;
  }
  pending.delete(serverId);
  const timer = timers.get(serverId);
  if (timer) {
    window.clearTimeout(timer.handle);
    timers.delete(serverId);
  }
  lastAttemptAt.set(serverId, Date.now());
  try {
    window.localStorage.setItem(keyFor(serverId), text);
    lastWritten.set(serverId, { value, shape });
    rememberLastMachine(serverId);
  } catch {
    // Quota or private mode. The cache keeps whatever it last held, and the
    // next refresh after the write interval tries again.
  }
}

function rememberLastMachine(serverId: string): void {
  if (lastMachineWritten === serverId) return;
  try {
    window.localStorage.setItem(LAST_MACHINE_KEY, serverId);
    lastMachineWritten = serverId;
  } catch {
    // The pointer is a convenience; without it the first frame is empty.
  }
}

/**
 * Move the one big key to per-machine slices and delete it. Runs once, at
 * import: the point of the slice is that the old combined value stops existing, not
 * that it stops growing.
 */
export function migrateLegacyCaches(): void {
  try {
    const raw = window.localStorage.getItem(LEGACY_FLEET_KEY);
    if (raw) {
      const parsed = JSON.parse(raw) as {
        lastServerId?: string | null;
        machines?: Record<string, { sessions?: SessionInfo[]; activeId?: string | null }>;
      } | null;
      for (const [serverId, machine] of Object.entries(parsed?.machines ?? {})) {
        if (readMachineCache(serverId)) continue;
        // Through the normal path, so a migrated slice is bounded exactly like
        // a written one. The first write for a machine is immediate.
        cacheMachineSessions(serverId, machine?.sessions ?? [], machine?.activeId ?? null);
      }
      if (typeof parsed?.lastServerId === 'string') rememberLastMachine(parsed.lastServerId);
      window.localStorage.removeItem(LEGACY_FLEET_KEY);
    }
    window.localStorage.removeItem(LEGACY_SINGLE_KEY);
  } catch {
    // A cache that cannot be migrated is a cache that starts empty.
  }
}

if (typeof window !== 'undefined' && typeof window.addEventListener === 'function') {
  // A tab that is closing or hidden has no later chance to write, and this is
  // the one moment where writing immediately is worth it.
  window.addEventListener('pagehide', flushCachedSessions);
  window.addEventListener('visibilitychange', () => {
    if (typeof document !== 'undefined' && document.visibilityState === 'hidden') {
      flushCachedSessions();
    }
  });
}
