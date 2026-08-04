import type { ClaudeSettings, CreateSessionRequest, SessionInfo, DirectoryCandidate } from '../types';
import { getActiveServer, getServer, isLocalServer, serverDisplayName, useServers, type ServerConfig } from '../lib/servers';
import { isTauri } from '../lib/tauriBridge';

// Thrown when the daemon returns HTTP 401 (token required / wrong token).
// Callers (UI components) can instanceof-check this to show an auth prompt
// rather than a generic error toast.  The `.code` property lets non-class
// checks work too: `err.code === 'auth'`.
export class AuthError extends Error {
  readonly code = 'auth' as const;
  constructor() {
    super('sessionsd: authentication required — check your server token (401)');
    this.name = 'AuthError';
  }
}

// All REST/WS calls resolve their base URL through the active server at
// call time. Switching servers in the dropdown changes what subsequent
// fetches/WebSockets target without any other plumbing.
//
// Every configured endpoint is authoritative. We use relative same-origin
// URLs only when the selected server actually matches the page origin (the
// embedded-daemon build). A hosted shell selecting http://localhost:8787
// must keep that exact target; substituting window.location would send API
// calls to sessions.somewhere.tech instead of the user's daemon.
function useSameOriginDaemon(s: ServerConfig): boolean {
  if (isTauri()) return false;
  const pageScheme = window.location.protocol === 'https:' ? 'https' : 'http';
  const pagePort = window.location.port
    ? Number(window.location.port)
    : (pageScheme === 'https' ? 443 : 80);
  const sameHost = s.host.toLowerCase() === window.location.hostname.toLowerCase()
    || (isLocalServer(s) && ['localhost', '127.0.0.1', '::1', '[::1]'].includes(window.location.hostname.toLowerCase()));
  return sameHost && (s.scheme ?? 'http') === pageScheme && s.port === pagePort;
}

function hostForUrl(host: string): string {
  return host.includes(':') && !host.startsWith('[') ? `[${host}]` : host;
}

function httpBaseForServer(s: ServerConfig): string {
  if (useSameOriginDaemon(s)) {
    return window.location.origin;
  }
  // Honour the selected endpoint exactly. Falling back to HTTP keeps older
  // stored configs (which predate the scheme field) compatible.
  const scheme = s.scheme ?? 'http';
  return `${scheme}://${hostForUrl(s.host)}:${s.port}`;
}

function httpBase(): string {
  return httpBaseForServer(getActiveServer());
}

function requestedServer(serverId?: string): ServerConfig {
  return serverId ? getServer(serverId) : getActiveServer();
}

function wsBase(): string {
  const s = getActiveServer();
  if (useSameOriginDaemon(s)) {
    const scheme = window.location.protocol === 'https:' ? 'wss' : 'ws';
    return `${scheme}://${window.location.host}`;
  }
  // Mirror the http→https / ws→wss mapping so TLS connections work end-to-end.
  const scheme = s.scheme === 'https' ? 'wss' : 'ws';
  return `${scheme}://${hostForUrl(s.host)}:${s.port}`;
}

// Returns `{ Authorization: 'Bearer <token>' }` when the supplied server has
// a token configured, or an empty object when open (no auth).
function authHeaders(s: ServerConfig): Record<string, string> {
  return s.token ? { Authorization: `Bearer ${s.token}` } : {};
}

// Shared fetch path for active-server and explicit fleet requests. Injects
// auth when requested and translates 401 into the existing AuthError.
async function serverFetch(
  server: ServerConfig,
  input: RequestInfo | URL,
  init?: RequestInit,
  authenticate = true
): Promise<Response> {
  const extra = authenticate ? authHeaders(server) : {};
  const merged: RequestInit = {
    ...init,
    headers: { ...extra, ...(init?.headers as Record<string, string> | undefined) }
  };
  const res = await fetch(input, merged);
  if (res.status === 401) {
    if (server.isDefault && useSameOriginDaemon(server)) {
      useServers.getState().markTokenRequired(server.id);
    }
    throw new AuthError();
  }
  return res;
}

async function apiFetch(input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
  return serverFetch(getActiveServer(), input, init);
}

async function json<T>(res: Response): Promise<T> {
  if (!res.ok) {
    const text = await res.text().catch(() => '');
    throw new Error(`sessionsd ${res.status}: ${text || res.statusText}`);
  }
  return res.json() as Promise<T>;
}

async function featureJSON<T>(res: Response, feature: string): Promise<T> {
  if (res.status === 404) {
    throw new Error(`${feature} is not available on this runtime. Update Sessions or connect to a current sessionsd.`);
  }
  return json<T>(res);
}

export async function listSessions(serverId?: string): Promise<SessionInfo[]> {
  // The operations inbox is a lifecycle/history surface, not only a live
  // process switcher. Exited sessions are required for Finished/Failed
  // filters and for preserving parent-child provenance after a parent ends.
  const server = serverId ? getServer(serverId) : getActiveServer();
  const r = await serverFetch(
    server,
    `${httpBaseForServer(server)}/api/sessions?include_exited=1`
  );
  const body = await json<{ sessions: SessionInfo[] }>(r);
  return body.sessions.map(normalizeSessionInfo);
}

export async function updateDisplayParent(
  sessionId: string,
  parentSessionId: string | null
): Promise<string> {
  const r = await apiFetch(`${httpBase()}/api/sessions/${encodeURIComponent(sessionId)}/display-parent`, {
    method: 'PUT',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ parentSessionId: parentSessionId ?? '' })
  });
  const body = await featureJSON<{ displayParentSessionId: string }>(r, 'Session drag-and-drop grouping');
  return body.displayParentSessionId;
}

export async function updateSessionName(
  sessionId: string,
  name: string
): Promise<string> {
  const r = await apiFetch(`${httpBase()}/api/sessions/${encodeURIComponent(sessionId)}/name`, {
    method: 'PUT',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ name })
  });
  const body = await featureJSON<{ name: string }>(r, 'Session rename');
  return body.name;
}

export async function updateSetAside(
  sessionId: string,
  setAside: boolean
): Promise<number | null> {
  const r = await apiFetch(`${httpBase()}/api/sessions/${encodeURIComponent(sessionId)}/set-aside`, {
    method: 'PUT',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ setAside })
  });
  const body = await featureJSON<{ setAsideAt: number | null }>(r, 'Session working-set organization');
  return body.setAsideAt;
}

export interface MachineAccessRequest {
  request_id: string;
  client_id: string;
  name: string;
  login: string;
  user_name?: string;
  transport: 'tailnet' | 'nearby';
  address?: string;
  created_at: string;
  expires_at: string;
  status: 'pending';
}

export async function listMachineAccessRequests(
  server: ServerConfig = getActiveServer()
): Promise<MachineAccessRequest[] | null> {
  let r = await serverFetch(server, `${httpBaseForServer(server)}/api/access/requests`);
  if (r.status === 404) {
    r = await serverFetch(server, `${httpBaseForServer(server)}/api/tailnet/access/requests`);
  }
  if (r.status === 404) return null;
  const body = await json<{ requests: MachineAccessRequest[] }>(r);
  return body.requests;
}

export async function decideMachineAccessRequest(
  requestId: string,
  decision: 'accept' | 'deny',
  server: ServerConfig = getActiveServer()
): Promise<void> {
  let r = await serverFetch(
    server,
    `${httpBaseForServer(server)}/api/access/requests/${encodeURIComponent(requestId)}`,
    {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ decision })
    }
  );
  if (r.status === 404) {
    r = await serverFetch(
      server,
      `${httpBaseForServer(server)}/api/tailnet/access/requests/${encodeURIComponent(requestId)}`,
      {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ decision })
      }
    );
  }
  await json<MachineAccessRequest>(r);
}

type WireSessionInfo = SessionInfo & {
  config_dir?: string;
  worktree_path?: string;
  source_repo?: string;
  creator_kind?: string;
  creator_id?: string;
  parent_session_id?: string;
  delegation_kind?: 'user' | 'agent';
  display_parent_session_id?: string;
  creator_ancestry?: string[];
  root_creator_kind?: string;
  root_creator_id?: string;
  provenance_status?: string;
  reopened_as?: string;
  resumed_from?: string;
  moved_to_endpoint?: string;
  moved_to_session_id?: string;
  moved_from_endpoint?: string;
  moved_from_session_id?: string;
  ended_by_kind?: string;
  ended_by_id?: string;
  ended_by_name?: string;
  ended_by_client?: string;
  end_reason?: string;
  end_operation_id?: string;
};

function normalizeSessionInfo(session: SessionInfo): SessionInfo {
  const wire = session as WireSessionInfo;
  return {
    ...session,
    args: Array.isArray(session.args) ? session.args : [],
    configDir: session.configDir ?? wire.config_dir,
    worktreePath: session.worktreePath ?? wire.worktree_path,
    sourceRepo: session.sourceRepo ?? wire.source_repo,
    creatorKind: session.creatorKind ?? wire.creator_kind,
    creatorId: session.creatorId ?? wire.creator_id,
    parentSessionId: session.parentSessionId ?? wire.parent_session_id,
    delegationKind: session.delegationKind ?? wire.delegation_kind,
    displayParentSessionId: session.displayParentSessionId ?? wire.display_parent_session_id,
    creatorAncestry: session.creatorAncestry ?? wire.creator_ancestry,
    rootCreatorKind: session.rootCreatorKind ?? wire.root_creator_kind,
    rootCreatorId: session.rootCreatorId ?? wire.root_creator_id,
    provenanceStatus: session.provenanceStatus ?? wire.provenance_status,
    reopenedAs: session.reopenedAs ?? wire.reopened_as,
    resumedFrom: session.resumedFrom ?? wire.resumed_from,
    movedToEndpoint: session.movedToEndpoint ?? wire.moved_to_endpoint,
    movedToSessionId: session.movedToSessionId ?? wire.moved_to_session_id,
    movedFromEndpoint: session.movedFromEndpoint ?? wire.moved_from_endpoint,
    movedFromSessionId: session.movedFromSessionId ?? wire.moved_from_session_id,
    endedByKind: session.endedByKind ?? wire.ended_by_kind,
    endedById: session.endedById ?? wire.ended_by_id,
    endedByName: session.endedByName ?? wire.ended_by_name,
    endedByClient: session.endedByClient ?? wire.ended_by_client,
    endReason: session.endReason ?? wire.end_reason,
    endOperationId: session.endOperationId ?? wire.end_operation_id
  };
}

export interface ServerHealth {
  ok: boolean;
  name: string;
  version: string;
  listen: { host: string; port: number };
  lan: { enabled: boolean; url: string | null };
  access?: { open: boolean };
  system?: { os: string; arch: string };
  compatibility?: {
    api: { current: number; minimumClient: number; maximumClient: number };
    runner: { current: number; minimum: number; maximum: number };
  };
  discovering: boolean;
  sessionsLoaded: number;
}

export const API_PROTOCOL_VERSION = 1;

function validateServerHealth(health: ServerHealth): ServerHealth {
  if (!health.ok || health.name !== 'sessionsd') {
    throw new Error('unexpected health response');
  }
  const range = health.compatibility?.api;
  if (
    range
    && (API_PROTOCOL_VERSION < range.minimumClient || API_PROTOCOL_VERSION > range.maximumClient)
  ) {
    throw new Error(
      `This client uses Sessions API ${API_PROTOCOL_VERSION}, but the machine accepts ${range.minimumClient}–${range.maximumClient}. Update Sessions on this device or the host.`
    );
  }
  return health;
}

export async function fetchActiveServerHealth(signal?: AbortSignal): Promise<ServerHealth> {
  const server = getActiveServer();
  const r = await serverFetch(server, `${httpBaseForServer(server)}/api/health`, { signal }, false);
  return validateServerHealth(await json<ServerHealth>(r));
}

export interface LANState {
  enabled: boolean;
  url: string | null;
  bonjour?: {
    advertised: boolean;
    service: string;
    error?: string;
  };
}

export async function fetchLANState(signal?: AbortSignal): Promise<LANState> {
  const r = await apiFetch(`${httpBase()}/api/lan`, { signal });
  return json<LANState>(r);
}

export async function setLANEnabled(enabled: boolean): Promise<LANState> {
  const r = await apiFetch(`${httpBase()}/api/lan`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ enabled })
  });
  return json<LANState>(r);
}

// Fleet probes never mutate the active-server store. Every request is
// resolved from the supplied config so all configured daemons can be polled
// concurrently by the browser without proxying through another sessionsd.
export async function fetchServerHealth(
  server: ServerConfig,
  signal?: AbortSignal
): Promise<ServerHealth> {
  const r = await serverFetch(
    server,
    `${httpBaseForServer(server)}/api/health`,
    { signal },
    false
  );
  return validateServerHealth(await json<ServerHealth>(r));
}

export interface ServerMachineIdentity {
  machineId: string;
  name: string;
}

export async function fetchServerMachineIdentity(
  server: ServerConfig,
  signal?: AbortSignal
): Promise<ServerMachineIdentity> {
  const r = await serverFetch(
    server,
    `${httpBaseForServer(server)}/api/machine`,
    { signal }
  );
  const body = await json<{ machine_id?: string; machineId?: string; name: string }>(r);
  const machineId = (body.machineId ?? body.machine_id ?? '').trim();
  const name = body.name?.trim();
  if (!machineId || !name) throw new Error('machine identity response was incomplete');
  return { machineId, name };
}

export async function listServerSessions(
  server: ServerConfig,
  signal?: AbortSignal
): Promise<SessionInfo[]> {
  const r = await serverFetch(
    server,
    `${httpBaseForServer(server)}/api/sessions?include_exited=1`,
    { signal }
  );
  const body = await json<{ sessions: SessionInfo[] }>(r);
  return body.sessions.map(normalizeSessionInfo);
}

export interface AccountProfileSession {
  id: string;
  name?: string;
}

export interface AccountProfile {
  tool: 'claude' | 'codex';
  name: string;
  path: string;
  sessions: AccountProfileSession[];
  last_used: number;
}

async function profilesForServer(server: ServerConfig, signal?: AbortSignal): Promise<AccountProfile[]> {
  const r = await serverFetch(server, `${httpBaseForServer(server)}/api/profiles`, { signal });
  if (r.status === 404 || r.status === 501) return [];
  const body = await json<{ profiles: AccountProfile[] }>(r);
  return body.profiles;
}

export async function fetchProfiles(signal?: AbortSignal, serverId?: string): Promise<AccountProfile[]> {
  return profilesForServer(requestedServer(serverId), signal);
}

export async function listServerProfiles(
  server: ServerConfig,
  signal?: AbortSignal
): Promise<AccountProfile[]> {
  return profilesForServer(server, signal);
}

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

export interface SearchResponse { matches: SearchMatch[]; total: number }

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
  return json<SearchResponse>(r);
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
}

export interface HistoryResponse {
  schemaVersion: number;
  sessions: HistorySession[];
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

const HISTORY_CONTROL_MESSAGE = /^\s*<(?:local-command|command-(?:name|message|args)|system-reminder)\b/i;
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

function normalizeResumePreview(text?: string): string {
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

export async function createSession(req: CreateSessionRequest, serverId?: string): Promise<SessionInfo> {
  const { creatorSessionId, ...body } = req;
  const server = requestedServer(serverId);
  const r = await serverFetch(server, `${httpBaseForServer(server)}/api/sessions`, {
    method: 'POST',
    headers: {
      'content-type': 'application/json',
      ...(creatorSessionId ? { 'X-Sessions-Creator-Session': creatorSessionId } : {})
    },
    body: JSON.stringify(body)
  });
  return normalizeSessionInfo(await json<SessionInfo>(r));
}

export interface ArchiveResult {
  items: Array<{
    id: string;
    name?: string;
    status: 'archived' | 'skipped';
    reason?: string;
  }>;
}

export async function archiveSessions(ids: string[]): Promise<ArchiveResult> {
  const r = await apiFetch(`${httpBase()}/api/retention/archive`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ ids })
  });
  return featureJSON<ArchiveResult>(r, 'Session archive');
}

export interface ProviderStatus {
  id: 'claude' | 'codex';
  name: string;
  installed: boolean;
  version?: string;
  latestVersion?: string;
  lastCheckedAt?: string;
  updateAvailable: boolean;
}

export async function fetchProviderStatuses(signal?: AbortSignal): Promise<ProviderStatus[]> {
  const r = await apiFetch(`${httpBase()}/api/providers`, { signal });
  const body = await featureJSON<{ providers: ProviderStatus[] }>(r, 'Provider status');
  return body.providers;
}

export async function updateProvider(id: ProviderStatus['id']): Promise<{ provider: ProviderStatus; output: string }> {
  const r = await apiFetch(`${httpBase()}/api/providers/${id}/update`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: '{}'
  });
  return featureJSON<{ provider: ProviderStatus; output: string }>(r, 'Provider update');
}

export async function updateSessionTags(
  sessionId: string,
  tags: Record<string, string>
): Promise<Record<string, string>> {
  const r = await apiFetch(`${httpBase()}/api/sessions/${encodeURIComponent(sessionId)}/tags`, {
    method: 'PUT',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ tags })
  });
  const body = await json<{ tags: Record<string, string> | null }>(r);
  return body.tags ?? {};
}

export async function updateSessionModel(
  sessionId: string,
  model: string,
  effort: string
): Promise<SessionInfo> {
  const r = await apiFetch(`${httpBase()}/api/sessions/${encodeURIComponent(sessionId)}/model`, {
    method: 'PUT',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ model, effort })
  });
  return normalizeSessionInfo(await featureJSON<SessionInfo>(r, 'Live model controls'));
}

export interface SessionModelOption {
  id: string;
  displayName: string;
  hidden: boolean;
  isDefault: boolean;
  defaultReasoningEffort: string;
  supportedReasoningEfforts: Array<{ reasoningEffort: string; description: string }>;
}

export async function listNewSessionCodexModels(signal?: AbortSignal, serverId?: string): Promise<SessionModelOption[]> {
  const server = requestedServer(serverId);
  const r = await serverFetch(server, `${httpBaseForServer(server)}/api/models/codex`, { signal });
  const body = await featureJSON<{ models?: SessionModelOption[] }>(r, 'Codex model choices');
  return body.models ?? [];
}

export async function listSessionModelOptions(sessionId: string): Promise<SessionModelOption[]> {
  const r = await apiFetch(`${httpBase()}/api/sessions/${encodeURIComponent(sessionId)}/model-options`);
  const body = await featureJSON<{ models?: SessionModelOption[] }>(r, 'Live model options');
  return body.models ?? [];
}

export interface UsageTokens {
  inputTokens: number;
  outputTokens: number;
  cacheCreationTokens: number;
  cacheReadTokens: number;
  reasoningTokens: number;
}

export interface UsageRow {
  key: string;
  start?: string;
  provider?: string;
  sessionId?: string;
  providerSessionId?: string;
  tags?: Record<string, string>;
  models: string[];
  tokens: UsageTokens;
  costUSD: number;
  recordedCostUSD: number;
  calculatedCostUSD: number;
  entries: number;
  missingPricingEntries: number;
}

export interface UsageEvent {
  eventKey: string;
  groupKey: string;
  start?: string;
  provider: string;
  providerSessionId?: string;
  sessionId?: string;
  tags?: Record<string, string>;
  model?: string;
  tokens: UsageTokens;
  costUSD: number;
  recordedCostUSD: number;
  calculatedCostUSD: number;
  hasRecordedCost: boolean;
  missingPricing: boolean;
}

export interface UsageReport {
  schemaVersion: number;
  machine: string;
  generatedAt: string;
  group: 'daily' | 'weekly' | 'monthly' | 'session' | 'tag' | 'provider' | 'model';
  mode: 'auto' | 'calculate' | 'display';
  dimension?: string;
  pricing: { source: string; revision: string; url: string; note: string };
  scan: { filesSeen: number; filesRead: number; linesRead: number; entriesSeen: number };
  rows: UsageRow[];
  totals: UsageRow;
  eventsIncluded?: boolean;
  events?: UsageEvent[];
}

export interface UsageOptions {
  group: UsageReport['group'];
  mode: UsageReport['mode'];
  provider?: 'claude' | 'codex';
  since?: string;
  until?: string;
  dimension?: string;
  includeEvents?: boolean;
}

export async function fetchUsage(options: UsageOptions, signal?: AbortSignal): Promise<UsageReport> {
	return fetchUsageForServer(getActiveServer(), options, signal);
}

export async function fetchUsageForServer(
	server: ServerConfig,
	options: UsageOptions,
	signal?: AbortSignal
): Promise<UsageReport> {
	const query = new URLSearchParams({ group: options.group, mode: options.mode });
  if (options.provider) query.set('provider', options.provider);
  if (options.since) query.set('since', options.since);
  if (options.until) query.set('until', options.until);
  if (options.dimension) query.set('dimension', options.dimension);
	if (options.includeEvents) query.set('events', '1');
	const r = await serverFetch(server, `${httpBaseForServer(server)}/api/usage?${query.toString()}`, { signal });
	return featureJSON<UsageReport>(r, 'Usage');
}

export type RecapProvider = 'off' | 'codex' | 'claude';

export interface RecapSettings {
  provider: RecapProvider;
}

export interface RecapActivity {
  id: string;
  name: string;
  description?: string;
  summary?: string;
  outcome: 'working' | 'idle' | 'done' | 'error' | 'observed';
  tool: string;
  cwd: string;
  branch?: string;
  sourceRepo?: string;
  tags?: Record<string, string>;
  createdAt: number;
  lastActivityAt: number;
  exitedAt?: number;
  parentSessionId?: string;
  creatorAncestry?: string[];
  provenanceStatus?: string;
  source?: 'sessions' | 'provider';
  origin?: string;
  providerSessionId?: string;
}

export interface RecapDocument {
  date: string;
  provider: Exclude<RecapProvider, 'off'>;
  generatedAt: string;
  inputDigest: string;
  markdown: string;
}

export interface RecapDay {
  date: string;
  timezone: string;
  settings: RecapSettings;
  activities: RecapActivity[];
  usage: UsageRow;
  document: RecapDocument | null;
  documentStale: boolean;
}

export async function fetchRecapSettings(signal?: AbortSignal): Promise<RecapSettings> {
  const r = await apiFetch(`${httpBase()}/api/recap/settings`, { signal });
  return featureJSON<RecapSettings>(r, 'Daily');
}

export async function updateRecapSettings(settings: RecapSettings): Promise<RecapSettings> {
  const r = await apiFetch(`${httpBase()}/api/recap/settings`, {
    method: 'PUT',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(settings)
  });
  return featureJSON<RecapSettings>(r, 'Daily');
}

export async function fetchRecap(date: string, signal?: AbortSignal): Promise<RecapDay> {
  const query = new URLSearchParams({ date });
  const r = await apiFetch(`${httpBase()}/api/recap?${query.toString()}`, { signal });
  return featureJSON<RecapDay>(r, 'Daily');
}

export async function fetchRecapDates(signal?: AbortSignal): Promise<string[]> {
  const r = await apiFetch(`${httpBase()}/api/recap/dates`, { signal });
  const payload = await featureJSON<{ dates?: unknown }>(r, 'Daily');
  return Array.isArray(payload.dates)
    ? payload.dates.filter((date): date is string => typeof date === 'string')
    : [];
}

export async function generateRecap(date: string, force = false): Promise<RecapDay> {
  const r = await apiFetch(`${httpBase()}/api/recap/generate`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ date, force })
  });
  return featureJSON<RecapDay>(r, 'Daily');
}

export interface Snapshot {
  text: string;
  // Server seq# the snapshot represents. Pass this to wsUrl as
  // lastSeq so the WS skips the replay of frames already painted
  // into the snapshot — the difference between "buffer fills top
  // to bottom over 3-5s" and "buffer is just there".
  seq: number;
}

// Fetch the runner's current xterm-headless snapshot (ANSI-coded text).
// Used by usePrettyParser instead of serializing the LOCAL browser xterm,
// because the local one wraps to viewport width while the runner stays
// at the PTY's fixed cols. This means Sessions view is consistent across
// clients of any size — phone, mac, agent — they all see the same
// canonical snapshot the runner has.
//
// `cols`: when set, sessionsd reflows the snapshot ANSI-aware to that
// visible width before sending. The Reflowed view passes its viewport
// width here so long prose wraps to fit without horizontal scroll while
// box-drawing / table lines stay intact.
export async function snapshot(sessionId: string, cols?: number, includeScrollback = false): Promise<Snapshot | null> {
  const query = new URLSearchParams();
  if (cols && cols > 0) query.set('cols', String(cols | 0));
  if (includeScrollback && !cols) query.set('scrollback', '1');
  const params = query.size > 0 ? `?${query.toString()}` : '';
  const r = await apiFetch(`${httpBase()}/api/sessions/${encodeURIComponent(sessionId)}/snapshot${params}`);
  if (r.status === 404) return null;
  if (!r.ok) {
    const text = await r.text().catch(() => '');
    throw new Error(`sessionsd snapshot ${r.status}: ${text || r.statusText}`);
  }
  const text = await r.text();
  const seq = Number(r.headers.get('X-Sessions-Seq') ?? '0') || 0;
  return { text, seq };
}

export interface EventsResponse {
  events: import('../types').ClaudeSessionEvent[];
  nextIndex: number;
  totalCount: number;
  startIndex: number;
  endIndex: number;
}

// Fetch Claude JSONL events for a session, with optional windowing.
//
//   tail: only return the last N events in the selected window
//   since: return events from server-side log index N onwards
//          (incremental polling — pass previous nextIndex to fetch
//          only what's new since last time)
//   before: end the selected window before absolute index N
//
// Without params: returns the full ring buffer. Avoid this — live
// sessions hold ~15-20 MB of JSONL in memory. Every response carries
// nextIndex so the caller can resume from there.
export async function fetchClaudeEvents(
  sessionId: string,
  opts?: { tail?: number; since?: number; before?: number }
): Promise<EventsResponse | null> {
  const params = new URLSearchParams();
  if (opts?.tail != null) params.set('tail', String(opts.tail));
  if (opts?.since != null) params.set('since', String(opts.since));
  if (opts?.before != null) params.set('before', String(opts.before));
  const qs = params.toString();
  const url = `${httpBase()}/api/sessions/${encodeURIComponent(sessionId)}/events${qs ? '?' + qs : ''}`;
  const r = await apiFetch(url);
  if (r.status === 404) return null;
  if (!r.ok) throw new Error(`sessionsd events ${r.status}`);
  return json<EventsResponse>(r);
}

// Resumable provider conversation metadata. Scanned locally from Claude and
// Codex stores. The picker binds the chosen provider UUID through the audited
// recovery/adopt route; no transcript is copied.
export interface ResumableSession {
  sessionId: string;
  tool: 'claude' | 'codex';
  origin?: string;
  title?: string;
  historyId?: string;
  promptHistoryOnly?: boolean;
  runs?: ResumableRun[];
  cwd: string;
  modifiedAt: number;
  firstUserMessage: string;
  sizeBytes: number;
}
export interface ResumableRun {
  sessionId: string;
  name?: string;
  startedAt: number;
  lastActivityAt: number;
  machine?: string;
  reopenedAs?: string;
  resumedFrom?: string;
  movedToEndpoint?: string;
  movedToSessionId?: string;
  movedFromEndpoint?: string;
  movedFromSessionId?: string;
}
export async function fetchResumableSessions(): Promise<ResumableSession[]> {
  return resumableSessionsForServer(getActiveServer());
}

export async function fetchServerResumableSessions(
  server: ServerConfig,
  signal?: AbortSignal
): Promise<ResumableSession[]> {
  return resumableSessionsForServer(server, signal);
}

async function resumableSessionsForServer(
  server: ServerConfig,
  signal?: AbortSignal
): Promise<ResumableSession[]> {
  let r = await serverFetch(
    server,
    `${httpBaseForServer(server)}/api/resumable-conversations`,
    { signal }
  );
  if (r.status === 404) {
    // Compatibility with the public 0.1 runtime while the native app and
    // daemon are updated as one package.
    r = await serverFetch(
      server,
      `${httpBaseForServer(server)}/api/claude-sessions`,
      { signal }
    );
    const legacy = await json<{ sessions: Omit<ResumableSession, 'tool' | 'origin'>[] }>(r);
    return legacy.sessions.map((session) => ({
      ...session,
      firstUserMessage: normalizeResumePreview(session.firstUserMessage),
      tool: 'claude',
      origin: 'Claude Code'
    }));
  }
  const body = await json<{ sessions: ResumableSession[] }>(r);
  return body.sessions.map((session) => ({
    ...session,
    firstUserMessage: normalizeResumePreview(session.firstUserMessage)
  }));
}

export interface AdoptConversationResult {
  ok: boolean;
  partial?: boolean;
  laneId: string;
  warning?: string;
  missingAnnotations?: Array<'adopt-created' | 'provider-bound' | 'source-linked'>;
  repair?: AdoptRepairRequest;
  adoption?: {
    path: string;
    historyId?: string;
    promptHistoryOnly?: boolean;
    tool: string;
    cwd: string;
    providerUuid: string;
    cmd: string;
    args: string[];
  };
  sourceHistoryId?: string;
  sourceProvider?: 'claude' | 'codex';
  destinationProvider?: 'claude' | 'codex';
  mode?: 'native-import' | 'linked-search';
  importedMessages?: number;
  forkedFromSessionId?: string;
  forkPointIndex?: number;
  forkPointMessageId?: string;
  sourceUntouched?: boolean;
}

export interface AdoptRepairRequest {
  target: string;
  historyId?: string;
  laneId: string;
  sourceSessionId?: string;
}

export async function adoptConversation(
  providerUuid: string,
  sourceSessionId?: string,
  historyId?: string,
  destinationProvider?: 'claude' | 'codex',
  runtimeMode?: 'rich' | 'terminal',
  remoteControl?: boolean
): Promise<AdoptConversationResult> {
  const r = await apiFetch(`${httpBase()}/api/recovery/adopt`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({
      target: providerUuid,
      sourceSessionId,
      historyId,
      destinationProvider,
      runtimeMode,
      remoteControl
    })
  });
  return json<AdoptConversationResult>(r);
}

export async function repairAdoption(request: AdoptRepairRequest): Promise<AdoptConversationResult> {
  const r = await apiFetch(`${httpBase()}/api/recovery/adopt`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({
      target: request.target,
      historyId: request.historyId,
      sourceSessionId: request.sourceSessionId,
      repairLaneId: request.laneId
    })
  });
  return json<AdoptConversationResult>(r);
}

export async function forkConversation(
  sourceSessionId: string,
  destinationProvider: 'claude' | 'codex',
  point?: { index: number; messageId: string }
): Promise<AdoptConversationResult> {
  const r = await apiFetch(`${httpBase()}/api/recovery/fork`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({
      sourceSessionId,
      destinationProvider,
      ...(point ? { sourceMessageIndex: point.index, sourceMessageId: point.messageId } : {})
    })
  });
  return featureJSON<AdoptConversationResult>(r, 'Conversation copies');
}

export async function listDirectories(serverId?: string): Promise<DirectoryCandidate[]> {
  const server = requestedServer(serverId);
  const r = await serverFetch(server, `${httpBaseForServer(server)}/api/directories`);
  const body = await json<{ directories: DirectoryCandidate[] }>(r);
  return body.directories;
}

export interface FsEntry {
  name: string;
  kind: 'dir' | 'file' | 'symlink' | 'other';
  hidden: boolean;
}
export interface FsListing {
  path: string;       // canonical absolute path
  parent: string | null; // null when at filesystem root
  entries: FsEntry[];
}

// Direct filesystem listing — the DirectoryBrowser walks this. No
// curation, no "project-shape" filtering; every child the sessionsd
// process can stat shows up. Default to $HOME when path is omitted.
export async function listFs(path?: string, serverId?: string): Promise<FsListing> {
  const server = requestedServer(serverId);
  const base = httpBaseForServer(server) || window.location.origin;
  const url = new URL(`${base}/api/fs/list`);
  if (path) url.searchParams.set('path', path);
  const r = await serverFetch(server, url);
  return json<FsListing>(r);
}

export async function killSession(id: string, reason = '', serverId?: string): Promise<void> {
  const server = requestedServer(serverId);
  const r = await serverFetch(server, `${httpBaseForServer(server)}/api/sessions/${encodeURIComponent(id)}`, {
    method: 'DELETE',
    headers: {
      'content-type': 'application/json',
      'X-Sessions-Client': 'sessions-desktop'
    },
    body: JSON.stringify({ reason })
  });
  await json<{ ok: boolean }>(r);
}

// Push raw bytes to a session's PTY. Used by GridCell's keystroke
// forwarding — no per-cell WebSocket, just a single HTTP POST per
// keystroke. The 2-second poll on each cell already reflects the
// result back into the reflowed thumbnail.
export async function sendInput(sessionId: string, data: string, serverId?: string): Promise<void> {
  const server = requestedServer(serverId);
  const r = await serverFetch(server, `${httpBaseForServer(server)}/api/sessions/${encodeURIComponent(sessionId)}/input`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ data })
  });
  await json<{ ok: boolean }>(r);
}

// Upload a file to the sessionsd host's uploads dir. Returns the absolute
// path on the server. Matches the macOS Terminal drag-drop convention
// — the InputBar pastes that path as text after a successful upload so
// Claude/Codex can read the file off disk.
export async function uploadFile(sessionId: string, file: File): Promise<{ path: string; size: number }> {
  const r = await apiFetch(`${httpBase()}/api/sessions/${encodeURIComponent(sessionId)}/upload`, {
    method: 'POST',
    headers: {
      'content-type': file.type || 'application/octet-stream',
      'x-sessions-filename': file.name || 'file'
    },
    body: file
  });
  return json<{ path: string; size: number }>(r);
}

export interface PushSubscriptionPayload {
  endpoint: string;
  expirationTime?: number | null;
  keys: {
    p256dh: string;
    auth: string;
  };
}

function pushSubscriptionPayload(subscription: PushSubscription): PushSubscriptionPayload {
  const raw = subscription.toJSON();
  if (
    typeof raw.endpoint !== 'string' ||
    !raw.keys ||
    typeof raw.keys.p256dh !== 'string' ||
    typeof raw.keys.auth !== 'string'
  ) {
    throw new Error('browser returned an invalid push subscription');
  }
  return {
    endpoint: raw.endpoint,
    expirationTime: raw.expirationTime,
    keys: {
      p256dh: raw.keys.p256dh,
      auth: raw.keys.auth
    }
  };
}

export async function getPushVapidPublicKey(): Promise<string> {
  const r = await apiFetch(`${httpBase()}/api/push/vapid`);
  const body = await json<{ publicKey: string }>(r);
  return body.publicKey;
}

export async function subscribePush(subscription: PushSubscription): Promise<void> {
  const r = await apiFetch(`${httpBase()}/api/push/subscribe`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(pushSubscriptionPayload(subscription))
  });
  await json<{ ok: boolean }>(r);
}

export async function unsubscribePush(endpoint: string): Promise<void> {
  const r = await apiFetch(`${httpBase()}/api/push/unsubscribe`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ endpoint })
  });
  await json<{ ok: boolean }>(r);
}

export function wsUrl(sessionId: string, lastSeq?: number, claudeEventsSince?: number): string {
  const s = getActiveServer();
  const params = new URLSearchParams({ sessionId });
  if (lastSeq && lastSeq > 0) params.set('lastSeq', String(lastSeq));
  if (claudeEventsSince && claudeEventsSince > 0) {
    params.set('claudeEventsSince', String(claudeEventsSince));
  }
  // Browsers cannot set custom headers on WebSocket — token goes in URL instead.
  if (s.token) params.set('token', s.token);
  return `${wsBase()}/ws?${params.toString()}`;
}

// Multiplexed WS endpoint: ONE socket per window carrying every attached
// session's traffic as sessionId-tagged frames. useTerminal
// attaches/detaches sessions over it via lib/wsMux.
export function wsMuxUrl(): string {
  const s = getActiveServer();
  // Browsers cannot set WS request headers — pass the auth token as a query
  // param instead (daemon accepts ?token=<hex> per contract #1).
  const params = new URLSearchParams({ mux: '1' });
  if (s.token) params.set('token', s.token);
  return `${wsBase()}/ws?${params.toString()}`;
}
