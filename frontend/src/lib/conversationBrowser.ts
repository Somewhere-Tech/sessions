import type { HistorySession } from '../api/sessionsd';
import { classifySession, providerConversationId } from './sessionStatus';
import type { SessionInfo } from '../types';

// ───────────────────────────────────────────────────────────────────────────
// What a recorded conversation is, and how it comes back.
//
// This is the browser's half of `sessions history` (runtime/cmd/sessions/
// history.go). The CLI and this module must agree, because a user who reads
// "resumable" in the app and gets a refusal from the daemon has been lied to
// by whichever of the two was guessing. The rule both hold to is the one
// `sessions recover` holds to: offer the action that works, or offer none and
// say why. Never a button that fails.
//
// The order below is the order in conversationRecovery() in history.go, and it
// matters:
//
//   1. live      — the daemon refuses to resume a conversation that is already
//                  bound to a running session (session.ConversationLiveError,
//                  "attach ... or re-run with --force to take over"). This is
//                  the row a user is most likely to click, so getting it wrong
//                  is the most expensive mistake available here.
//   2. moved     — the conversation was continued on another machine. Resuming
//                  it here forks it, so the row points at the machine instead.
//   3. unreadable— the transcript could not be read on this pass. The row is
//                  still listed, named and addressable; only its actions go.
//   4. unrecoverable — neither the provider nor Sessions still holds it. This
//                  is the state that must say so rather than offer a button.
//   5. resumable — everything else, including a conversation whose provider
//                  deleted its own transcript but whose Sessions copy survives.
//                  That case is exactly why resume goes through Sessions'
//                  adopt route with a history id rather than the provider's
//                  native --resume flag.
// ───────────────────────────────────────────────────────────────────────────

export type ConversationStatus = 'resumable' | 'live' | 'moved' | 'unreadable' | 'unrecoverable';

export type ConversationTool = 'claude' | 'codex' | 'shell';

export interface ConversationRow {
  /** Unique across the fleet: one conversation on one machine. */
  key: string;
  serverId: string;
  serverName: string;
  /** The Sessions history id — the handle every read and resume goes through. */
  id: string;
  providerSessionId?: string;
  name: string;
  tool: ConversationTool;
  cwd: string;
  messages: number;
  /**
   * When the conversation was last spoken in. Prefers the transcript's own
   * mtime over the Sessions record's activity stamp for the reason documented
   * on HistorySession.conversation_updated_at. Zero when neither is known.
   */
  lastActiveAt: number;
  status: ConversationStatus;
  /** Why this row cannot be resumed. Empty for a resumable one. */
  reason: string;
  /** The live session holding this conversation, for attach. */
  liveSessionId?: string;
  /** What that live session is called — from the one classifier, not re-derived. */
  liveLabel?: string;
  /** False when there is no transcript to read: no preview, no reader. */
  readable: boolean;
  promptHistoryOnly: boolean;
  external: boolean;
}

export interface ConversationSource {
  serverId: string;
  serverName: string;
  sessions: HistorySession[];
  /** Everything the same daemon currently has running. */
  live: SessionInfo[];
}

export interface BrowseFilters {
  tool: '' | 'claude' | 'codex';
  /** Inclusive bounds in epoch ms. Zero means unbounded. */
  since: number;
  until: number;
  /** Glob over the conversation name, `PM*` / `*builder*`, as Search uses. */
  name: string;
  /** Substring of the working directory, as Search uses. */
  cwd: string;
  /**
   * Off by default. A browse answers with conversations you could plausibly
   * want: shell lanes, empty shells and conversations nothing can read are the
   * rows a person scrolls past. The count of what this hid is always shown.
   */
  all: boolean;
}

export const DEFAULT_BROWSE_FILTERS: BrowseFilters = {
  tool: '', since: 0, until: 0, name: '', cwd: '', all: false
};

export function conversationRecovery(
  session: HistorySession,
  live: SessionInfo | undefined
): { status: ConversationStatus; reason: string } {
  if (live) {
    return {
      status: 'live',
      reason: 'This conversation is running right now. Open the live session instead of starting a second one on top of it.'
    };
  }
  if (session.moved_to_endpoint) {
    return {
      status: 'moved',
      reason: `This conversation was continued on ${session.moved_to_endpoint}. Resume it there — resuming here would fork it.`
    };
  }
  if (session.unreadable) {
    return {
      status: 'unreadable',
      reason: session.unreadable_reason?.trim() || 'This conversation could not be read on this pass.'
    };
  }
  if (!session.conversation_available) {
    return {
      status: 'unrecoverable',
      reason: 'Neither the provider nor Sessions still holds this conversation.'
    };
  }
  return { status: 'resumable', reason: '' };
}

export function buildConversationRows(sources: ConversationSource[]): ConversationRow[] {
  const rows: ConversationRow[] = [];
  for (const source of sources) {
    // Two ways a conversation can be live: the daemon's own record for it is
    // running, or some other running session is bound to the same provider
    // conversation. The daemon's guard keys on the provider UUID, so both have
    // to be caught or the app offers a resume the daemon will refuse.
    const liveByID = new Map<string, SessionInfo>();
    const liveByProvider = new Map<string, SessionInfo>();
    for (const session of source.live) {
      // `exited` is the daemon's own field and the one /api/sessions and the
      // CLI read for "is this runtime still there". sessionStatus stays the
      // owner of what the session is *called*; see liveLabel below.
      if (session.exited) continue;
      liveByID.set(session.id, session);
      const providerID = providerConversationId(session);
      if (providerID) liveByProvider.set(providerID, session);
    }
    for (const session of source.sessions) {
      const live = liveByID.get(session.id)
        ?? (session.provider_session_id ? liveByProvider.get(session.provider_session_id) : undefined);
      const { status, reason } = conversationRecovery(session, live);
      rows.push({
        key: `${source.serverId}:${session.id}`,
        serverId: source.serverId,
        serverName: source.serverName,
        id: session.id,
        providerSessionId: session.provider_session_id?.trim() || undefined,
        name: session.name?.trim() ?? '',
        tool: session.tool,
        cwd: session.cwd ?? '',
        messages: session.message_count ?? 0,
        lastActiveAt: session.conversation_updated_at || session.last_activity_at || 0,
        status,
        reason,
        liveSessionId: live?.id,
        liveLabel: live ? classifySession(live).label : undefined,
        readable: session.conversation_available === true && session.unreadable !== true,
        promptHistoryOnly: session.prompt_history_only === true || isPromptHistoryOnly(session.id),
        external: session.external === true
      });
    }
  }
  return rows.sort((left, right) => (right.lastActiveAt - left.lastActiveAt) || left.key.localeCompare(right.key));
}

export function filterConversations(rows: ConversationRow[], filters: BrowseFilters): ConversationRow[] {
  const namePattern = globPattern(filters.name);
  const cwd = filters.cwd.trim().toLocaleLowerCase();
  return rows.filter((row) => {
    if (filters.tool && row.tool !== filters.tool) return false;
    if (!filters.all) {
      // Shell lanes are not conversations, an empty shell has nothing to
      // recognise, and a row nothing can bring back is not what someone
      // looking for their conversation is scrolling for.
      if (row.tool !== 'claude' && row.tool !== 'codex') return false;
      if (row.messages <= 0) return false;
      if (row.status === 'unrecoverable' || row.status === 'unreadable') return false;
    }
    if (cwd && !row.cwd.toLocaleLowerCase().includes(cwd)) return false;
    if (namePattern && !namePattern.test(row.name)) return false;
    // A conversation with no recorded time cannot be placed on a timeline, so
    // a date filter has to exclude it rather than guess where it belongs.
    if (filters.since && (row.lastActiveAt === 0 || row.lastActiveAt < filters.since)) return false;
    if (filters.until && (row.lastActiveAt === 0 || row.lastActiveAt > filters.until)) return false;
    return true;
  });
}

export interface ResumeTarget {
  providerSessionId: string;
  sourceSessionId?: string;
  historyId?: string;
}

/**
 * What to hand the adopt route for this row, or null when nothing may be
 * offered. The three fields are the same ones Search passes, so a resume from
 * the browser and a resume from a search result reach the daemon identically.
 */
export function conversationResumeTarget(row: ConversationRow): ResumeTarget | null {
  if (row.status !== 'resumable') return null;
  if (row.tool !== 'claude' && row.tool !== 'codex') return null;
  return {
    providerSessionId: row.providerSessionId || row.id,
    sourceSessionId: managedSourceSessionID(row.id),
    // With no provider handle — or only Claude's prompt index — the conversation
    // comes back from Sessions' own copy, addressed by its history id.
    historyId: !row.providerSessionId || row.promptHistoryOnly ? row.id : undefined
  };
}

/**
 * A Sessions-managed runtime id, or undefined for a record that only ever
 * existed inside the provider's own store.
 */
export function managedSourceSessionID(sessionID: string): string | undefined {
  return sessionID.startsWith('provider:') ? undefined : sessionID;
}

/** Claude's prompt index: the requests survived, the full transcript did not. */
export function isPromptHistoryOnly(sessionID: string): boolean {
  return sessionID.startsWith('provider-history:');
}

// ── Row formatting shared with Search ──────────────────────────────────────
// Both surfaces draw the same conversation row, so they read a path, a machine
// name, a date and a count the same way. These lived privately in SearchView
// until the browser needed them; a second copy would have drifted.

export function compactPath(value: string): string {
  return value.replace(/^\/(Users|home)\/[^/]+/, '~');
}

export function compactMachineName(value: string): string {
  const clean = value.trim().replace(/\.local$/i, '');
  if (/^mac[-\s]?mini(?:[-\s]?\d+)?$/i.test(clean)) return 'Mac mini';
  if (/^macbook(?:[-\s]?pro)?(?:[-\s]?\d+)?$/i.test(clean)) return 'MacBook';
  return clean.replace(/-/g, ' ') || 'Unknown computer';
}

export function plural(count: number, singular: string, pluralForm = `${singular}s`): string {
  return `${count} ${count === 1 ? singular : pluralForm}`;
}

export function relativeDate(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
}

/** "3d ago" — how long ago, which is how people remember a conversation. */
export function conversationAge(at: number, now = Date.now()): string {
  const difference = now - at;
  if (!at || difference < 0) return '';
  if (difference < 60_000) return 'just now';
  if (difference < 3_600_000) return `${Math.floor(difference / 60_000)}m ago`;
  if (difference < 86_400_000) return `${Math.floor(difference / 3_600_000)}h ago`;
  if (difference < 2_592_000_000) return `${Math.floor(difference / 86_400_000)}d ago`;
  return `${Math.floor(difference / 2_592_000_000)}mo ago`;
}

// Same glob dialect the Search filters use (`PM*`, `*builder*`), so one typed
// filter means the same thing whether the surface is browsing or searching.
function globPattern(value: string): RegExp | null {
  const trimmed = value.trim();
  if (!trimmed) return null;
  const escaped = trimmed.replace(/[.+?^${}()|[\]\\]/g, '\\$&').replaceAll('*', '.*');
  try {
    return new RegExp(escaped, 'i');
  } catch {
    return null;
  }
}
