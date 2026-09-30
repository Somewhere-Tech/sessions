import type { SessionInfo } from '../types';

// The first frame, written down — without rewriting it every three seconds.
//
// From the storage inventory on the Mini, 11 September: WebKit's
// localstorage.sqlite3-wal had grown to 8,503,029,072 bytes against a 1,531,904
// byte database, and the largest key in it was the session cache at about
// 1.43 MB. The app refreshes every three seconds and the cache was one value
// holding every machine, serialised and written on each refresh whether
// anything had changed or not. Eight gigabytes of write-ahead log is what a
// 1.4 MB value written twenty times a minute looks like to a database that
// cannot checkpoint while the app holds it open. The MacBook had ~195 MB of the
// same thing.
//
// So: one key per machine, a bounded row, and a write only when the bytes have
// actually changed — at most one per machine every thirty seconds, plus a flush
// when the tab goes away. Nothing here changes what the person sees: this is
// the cold-start frame, overwritten by the live listing about a second later.

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

// Text a first frame shows in one line. Anything longer is the conversation
// leaking into the cache, which is what made the value 1.43 MB.
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
const timers = new Map<string, number>();
const lastWriteAt = new Map<string, number>();
// The signature of what was last written, so an unchanged refresh writes
// nothing. Length plus an FNV-1a hash of the serialised slice: two different
// slices of the same length colliding would cost one stale cold-start frame
// until the next change, which is cheaper than keeping a second copy of the
// string in memory.
const lastSignature = new Map<string, string>();
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
 * Record this machine's rows. The write is debounced and skipped entirely when
 * the bytes are identical to the last ones written, which is what a refresh
 * against an idle daemon produces.
 */
export function cacheMachineSessions(
  serverId: string | null, sessions: SessionInfo[], activeId: string | null
): void {
  if (!serverId) return;
  pending.set(serverId, { sessions, activeId });
  const since = Date.now() - (lastWriteAt.get(serverId) ?? 0);
  if (since >= CACHE_WRITE_INTERVAL_MS) {
    writePending(serverId);
    return;
  }
  if (timers.has(serverId)) return;
  const timer = window.setTimeout(() => {
    timers.delete(serverId);
    writePending(serverId);
  }, CACHE_WRITE_INTERVAL_MS - since);
  timers.set(serverId, timer);
}

/** Write whatever is waiting, now: the tab is going away. */
export function flushCachedSessions(): void {
  for (const serverId of [...pending.keys()]) {
    const timer = timers.get(serverId);
    if (timer !== undefined) {
      window.clearTimeout(timer);
      timers.delete(serverId);
    }
    writePending(serverId);
  }
}

function writePending(serverId: string): void {
  const entry = pending.get(serverId);
  if (!entry) return;
  pending.delete(serverId);
  const text = JSON.stringify(boundedMachine(entry.sessions, entry.activeId));
  const signature = signatureOf(text);
  // An unchanged refresh is the common case, and it is the one that was
  // costing gigabytes of write-ahead log.
  if (lastSignature.get(serverId) === signature) return;
  try {
    window.localStorage.setItem(keyFor(serverId), text);
    rememberLastMachine(serverId);
    lastSignature.set(serverId, signature);
    lastWriteAt.set(serverId, Date.now());
  } catch {
    // quota / private mode — drop the cache silently
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
 * import: the point of the slice is that the 1.43 MB value stops existing, not
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
