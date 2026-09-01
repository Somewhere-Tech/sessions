import type { ClaudeSettings } from '../../types';
import { serverDisplayName, type ServerConfig } from '../../lib/servers';
import {
  apiFetch,
  featureJSON,
  httpBase,
  httpBaseForServer,
  json,
  serverFetch
} from './core';

export interface SearchMatch {
  session_id: string;
  provider_session_id?: string;
  // Client-added metadata match. The daemon returns message matches; Search
  // also adds one of these when a retained conversation title matches the
  // user's query so named chats are first-class search results.
  title_match?: boolean;
  name: string;
  tool: 'claude' | 'codex' | 'shell';
  role: 'user' | 'assistant' | 'tool';
  kind?: 'delegation' | 'handoff' | 'status' | 'automation';
  timestamp: string | null;
  message_index: number;
  message_id: string;
  snippet: string;
  match_start: number;
  match_end: number;
  score: number;
  cwd: string;
  machine: string;
  creator_kind?: string;
  creator_id?: string;
  context_before?: HistoryMessage[];
  context_after?: HistoryMessage[];
}

// One session's whole contribution to a result set. The daemon computes this
// across the entire index, so `hits`, `first_hit_at` and `last_hit_at` describe
// the session, not the page of messages that came back with it.
export interface SearchSessionHits {
  session_id: string;
  name: string;
  cwd?: string;
  tool?: 'claude' | 'codex' | 'shell';
  machine?: string;
  hits: number;
  title_match?: boolean;
  score: number;
  first_hit_at?: string;
  last_hit_at?: string;
  // Drawn from the returned page: a session that matched but did not reach the
  // page has counts and timestamps without snippets.
  snippets?: string[];
}

export interface SearchMachineState {
  alias: string;
  name: string;
  endpoint?: string;
  status: string;
  error?: string;
}

export interface SearchResponse {
  matches: SearchMatch[];
  total: number;
  machines?: SearchMachineState[];
  partial?: boolean;
  // Everything below is optional on purpose: a daemon older than the rollup
  // omits all of it and the client must still work from `matches`/`total`.
  sessions?: SearchSessionHits[];
  // The expression that actually ran, after stopword removal, path expansion
  // and conjunction. Differs from what was sent whenever the query was
  // rewritten.
  effective_query?: string;
  // Which rung of the relaxation ladder produced these results: 'strict',
  // 'quorum', 'broad' or 'raw'. Deliberately typed as a plain string so a
  // daemon that adds a rung is not silently misread as one of these.
  match_mode?: string;
  // Counts across the whole index, not the returned page.
  total_hits?: number;
  total_sessions?: number;
  // The rollup is incomplete: its counts are lower bounds.
  rollup_partial?: boolean;
}

// A daemon is not a type checker. Anything the rollup says is either
// well-formed or dropped here, so no view has to defend itself against a
// missing array, a NaN count or a null timestamp.
function normalizeSearchResponse(body: SearchResponse): SearchResponse {
  const count = (value: unknown): number | undefined =>
    typeof value === 'number' && Number.isFinite(value) && value >= 0 ? Math.floor(value) : undefined;
  const text = (value: unknown): string | undefined =>
    typeof value === 'string' && value.trim() ? value : undefined;
  const sessions = Array.isArray(body.sessions)
    ? body.sessions
      .filter((session): session is SearchSessionHits => Boolean(session) && typeof session.session_id === 'string')
      .map((session) => ({
        ...session,
        name: typeof session.name === 'string' ? session.name : '',
        hits: count(session.hits) ?? 0,
        score: typeof session.score === 'number' && Number.isFinite(session.score) ? session.score : 0,
        cwd: text(session.cwd),
        tool: session.tool === 'claude' || session.tool === 'codex' || session.tool === 'shell' ? session.tool : undefined,
        machine: text(session.machine),
        first_hit_at: text(session.first_hit_at),
        last_hit_at: text(session.last_hit_at),
        snippets: Array.isArray(session.snippets) ? session.snippets.filter((snippet) => typeof snippet === 'string') : undefined
      }))
    : undefined;
  return {
    ...body,
    matches: Array.isArray(body.matches) ? body.matches : [],
    total: count(body.total) ?? 0,
    ...(sessions ? { sessions } : { sessions: undefined }),
    effective_query: text(body.effective_query),
    match_mode: text(body.match_mode),
    total_hits: count(body.total_hits),
    total_sessions: count(body.total_sessions),
    rollup_partial: body.rollup_partial === true,
    partial: body.partial === true
  };
}

export type AIProvider = 'codex' | 'claude';
export interface AISettings { provider: AIProvider }
export interface SmartSearchPlan { provider: AIProvider; query: string }

export async function searchServer(
  server: ServerConfig,
  options: {
    query: string;
    mode: 'ranked' | 'exact' | 'regex';
    role?: string;
    tool?: string;
    session?: string;
    name?: string;
    cwd?: string;
    since?: string;
    until?: string;
    context?: number;
    timeline?: boolean;
    limit?: number;
  },
  signal?: AbortSignal
): Promise<SearchResponse> {
  const query = new URLSearchParams({ q: options.query, limit: String(options.limit ?? 100) });
  if (options.mode === 'ranked') query.set('ranked', 'true');
  if (options.mode === 'regex') query.set('regex', 'true');
  if (options.role) query.set('role', options.role);
  if (options.tool) query.set('tool', options.tool);
  if (options.session) query.set('session', options.session);
  if (options.name) query.set('name', options.name);
  if (options.cwd) query.set('cwd', options.cwd);
  if (options.since) query.set('since', options.since);
  if (options.until) query.set('until', options.until);
  if (options.context !== undefined) query.set('context', String(options.context));
  if (options.timeline) query.set('timeline', 'true');
  const r = await serverFetch(server, `${httpBaseForServer(server)}/api/search?${query.toString()}`, { signal });
  return normalizeSearchResponse(await json<SearchResponse>(r));
}

export async function fetchAISettings(signal?: AbortSignal): Promise<AISettings> {
  const r = await apiFetch(`${httpBase()}/api/ai/settings`, { signal });
  return featureJSON<AISettings>(r, 'Smart features');
}

export async function updateAISettings(settings: AISettings): Promise<AISettings> {
  const r = await apiFetch(`${httpBase()}/api/ai/settings`, {
    method: 'PUT',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(settings)
  });
  return featureJSON<AISettings>(r, 'Smart features');
}

export async function fetchClaudeSettings(signal?: AbortSignal): Promise<ClaudeSettings> {
  const r = await apiFetch(`${httpBase()}/api/claude/settings`, { signal });
  return featureJSON<ClaudeSettings>(r, 'Claude defaults');
}

export async function updateClaudeSettings(settings: ClaudeSettings): Promise<ClaudeSettings> {
  const r = await apiFetch(`${httpBase()}/api/claude/settings`, {
    method: 'PUT',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(settings)
  });
  return featureJSON<ClaudeSettings>(r, 'Claude defaults');
}

export interface OnboardingState {
  version: number;
  complete: boolean;
  remoteControl: 'pending' | 'enabled' | 'local-only';
  delegatedAccess: 'pending' | 'inherit' | 'autonomous';
  supported?: boolean;
}

export async function fetchOnboardingState(signal?: AbortSignal): Promise<OnboardingState> {
  const r = await apiFetch(`${httpBase()}/api/onboarding`, { signal });
  // Older daemons cannot enable Sessions' new Remote Control default, so
  // allowing their UI through is safe and keeps mixed-version Fleet usable.
  if (r.status === 404) {
    return { version: 0, complete: true, remoteControl: 'local-only', delegatedAccess: 'inherit', supported: false };
  }
  return { ...(await json<OnboardingState>(r)), supported: true };
}

export async function updateOnboardingPreference(
  remoteControl: 'enabled' | 'local-only',
  delegatedAccess: 'inherit' | 'autonomous'
): Promise<OnboardingState> {
  const r = await apiFetch(`${httpBase()}/api/onboarding`, {
    method: 'PUT',
    headers: {
      'content-type': 'application/json',
      'X-Sessions-User-Consent': 'onboarding'
    },
    body: JSON.stringify({ remoteControl, delegatedAccess })
  });
  return { ...(await featureJSON<OnboardingState>(r, 'Onboarding')), supported: true };
}

export async function planSmartSearch(query: string, signal?: AbortSignal): Promise<SmartSearchPlan> {
  const r = await apiFetch(`${httpBase()}/api/search/plan`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ query }),
    signal
  });
  return featureJSON<SmartSearchPlan>(r, 'AI search');
}

export interface HistorySession {
  id: string;
  name: string;
  tool: 'claude' | 'codex' | 'shell';
  provider_session_id?: string;
  cwd: string;
  machine: string;
  creator_kind?: string;
  creator_id?: string;
  created_at: number;
  last_activity_at: number;
  // When the conversation itself was last written to, read from the transcript
  // rather than from the Sessions record. A shutdown sweep that drains a dozen
  // finished runners moves every `last_activity_at` to the same instant without
  // a word being said, so this is the field to order a browse by. Absent on
  // records with no transcript behind them, and on daemons older than it.
  conversation_updated_at?: number;
  message_count: number;
  conversation_available: boolean;
  external?: boolean;
  prompt_history_only?: boolean;
  reopened_as?: string;
  resumed_from?: string;
  moved_to_endpoint?: string;
  moved_to_session_id?: string;
  moved_from_endpoint?: string;
  moved_from_session_id?: string;
  // One row that could not be read on this pass. The session is still listed,
  // named and addressable — losing one file must never lose the conversation.
  unreadable?: boolean;
  unreadable_reason?: string;
  skipped_records?: number;
}

export interface HistoryResponse {
  schemaVersion: number;
  sessions: HistorySession[];
  unreadable_sessions?: number;
  skipped_records?: number;
  transcripts_unread?: boolean;
}

/** One machine's answer to "every conversation you have recorded". */
export interface HistoryListing {
  sessions: HistorySession[];
  /** Rows the daemon listed but could not read. Always a count, never silence. */
  unreadableSessions: number;
  skippedRecords: number;
  /** True on the cheap view, which stats transcripts without parsing them. */
  transcriptsUnread: boolean;
}

export interface HistoryMessage {
  index: number;
  id: string;
  role: 'user' | 'assistant' | 'tool';
  kind?: 'delegation' | 'handoff' | 'status' | 'automation';
  text: string;
  timestamp: string | null;
  author?: {
    kind: 'session';
    id: string;
    name: string;
    client: string;
  };
}

export interface HistoryTranscript {
  schemaVersion: number;
  session: HistorySession;
  messages: HistoryMessage[];
  truncated?: boolean;
  has_more?: boolean;
  next_index?: number;
}

const HISTORY_CONTROL_MESSAGE = /^\s*(?:<(?:local-command|command-(?:name|message|args)|system-reminder|recommended_plugins|environment_context)\b|#\s*(?:AGENTS|CLAUDE)\.md instructions\b)/i;
const HISTORY_ANSI = /(?:\u001b|\\x1b)\[[0-?]*[ -/]*[@-~]/g;
const HISTORY_OSC = new RegExp('(?:\\u001b|\\\\x1b)\\][\\s\\S]*?(?:\\u0007|(?:\\u001b|\\\\x1b)\\\\)', 'g');

function stripHistoryANSI(value: string): string {
  return value.replace(HISTORY_OSC, '').replace(HISTORY_ANSI, '');
}

function normalizeHistoryTranscript(transcript: HistoryTranscript): HistoryTranscript {
  return {
    ...transcript,
    messages: transcript.messages
      .filter((message) => !HISTORY_CONTROL_MESSAGE.test(message.text))
      .map((message) => ({ ...message, text: stripHistoryANSI(message.text) }))
  };
}

export function normalizeResumePreview(text?: string): string {
  const clean = stripHistoryANSI(text ?? '').trim();
  const command = clean.match(/<command-name>([^<]+)<\/command-name>/i)?.[1]?.trim();
  if (command) return `Local command · ${command}`;
  if (HISTORY_CONTROL_MESSAGE.test(clean)) return 'Provider control message';
  return clean;
}

export async function fetchServerHistory(
  server: ServerConfig,
  signal?: AbortSignal
): Promise<HistorySession[]> {
  const r = await serverFetch(server, `${httpBaseForServer(server)}/api/history?summary=true`, { signal });
  if (!r.ok) throw new Error(`Sessions history ${r.status}`);
  return (await json<HistoryResponse>(r)).sessions;
}

// The full listing, which is what a conversation browser needs and what
// `sessions history` reads. `?summary=true` above deliberately stats each
// transcript without parsing it, so on that view `message_count` is 0 for
// every row — a browser built on it could neither show how big a conversation
// is nor tell an empty shell from a real one. The daemon caches its per-file
// counts by size and mtime, so the extra cost is paid once per changed file.
export async function fetchServerHistoryListing(
  server: ServerConfig,
  signal?: AbortSignal
): Promise<HistoryListing> {
  const r = await serverFetch(server, `${httpBaseForServer(server)}/api/history`, { signal });
  if (!r.ok) throw new Error(`Sessions history ${r.status}`);
  const body = await json<HistoryResponse>(r);
  return {
    sessions: body.sessions ?? [],
    unreadableSessions: body.unreadable_sessions ?? 0,
    skippedRecords: body.skipped_records ?? 0,
    transcriptsUnread: body.transcripts_unread === true
  };
}

export async function fetchServerHistoryTranscript(
  server: ServerConfig,
  sessionId: string,
  signal?: AbortSignal,
  window?: {
    preview?: boolean;
    start?: number;
    end?: number;
    role?: 'user' | 'assistant' | 'tool';
    anchor?: number;
    messageId?: string;
  }
): Promise<HistoryTranscript> {
  const query = new URLSearchParams({ format: 'json' });
  if (window?.start !== undefined) query.set('start', String(window.start));
  if (window?.end !== undefined) query.set('end', String(window.end));
  if (window?.role) query.set('role', window.role);
  if (window?.messageId) {
    query.set('anchor', String(window.anchor ?? 0));
    query.set('message_id', window.messageId);
  }
  const variant = window?.preview ? '/preview' : window ? '/window' : '';
  const r = await serverFetch(
    server,
    `${httpBaseForServer(server)}/api/history/${encodeURIComponent(sessionId)}${variant}?${query.toString()}`,
    { signal }
  );
  if (r.status === 404) {
    const body = await r.text().catch(() => '');
    try {
      const parsed = JSON.parse(body) as { error?: string };
      if (parsed.error === 'history session not found') {
        throw new Error(`This conversation is no longer available on ${serverDisplayName(server, true)}.`);
      }
    } catch (error) {
      if (error instanceof Error && error.message.startsWith('This conversation')) throw error;
    }
    throw new Error('Conversation viewing is not available on this runtime. Update Sessions or connect to a current sessionsd.');
  }
  if (r.status === 409) {
    throw new Error('This conversation changed after the search result was created. Go back and run the search again to refresh the bookmark.');
  }
  return normalizeHistoryTranscript(await json<HistoryTranscript>(r));
}
