import type { ClaudeSessionEvent, CreateSessionRequest, DirectoryCandidate, SessionInfo } from '../../types';
import { getActiveServer, type ServerConfig } from '../../lib/servers';
import { randomUUID } from '../../lib/uuid';
import {
  AuthError,
  apiFetch,
  featureJSON,
  httpBase,
  httpBaseForServer,
  json,
  requestedServer,
  serverFetch,
  wsBase
} from './core';
import { normalizeSessionInfo } from './sessions';
import { normalizeResumePreview } from './search';

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
  const controller = new AbortController();
  // Current runtimes stop the complete installer tree after five minutes.
  // Keep a slightly wider client boundary so an older remote runtime can
  // never leave the native sidebar in an endless busy state.
  const timer = window.setTimeout(() => controller.abort(), 5 * 60_000 + 20_000);
  try {
    const r = await apiFetch(`${httpBase()}/api/providers/${id}/update`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: '{}',
      signal: controller.signal
    });
    return await featureJSON<{ provider: ProviderStatus; output: string }>(r, 'Provider update');
  } catch (error) {
    if (controller.signal.aborted) {
      throw new Error('The update took too long and was stopped. Running sessions were not affected.');
    }
    throw error;
  } finally {
    window.clearTimeout(timer);
  }
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

export interface DailyActivity {
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

export interface DailyDay {
  date: string;
  timezone: string;
  activities: DailyActivity[];
  usage: UsageRow;
}

export async function fetchDaily(date: string, signal?: AbortSignal): Promise<DailyDay> {
  const query = new URLSearchParams({ date });
  const r = await apiFetch(`${httpBase()}/api/daily?${query.toString()}`, { signal });
  return featureJSON<DailyDay>(r, 'Daily');
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
  events: ClaudeSessionEvent[];
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
  external?: boolean;
  origin?: string;
  title?: string;
  historyId?: string;
  promptHistoryOnly?: boolean;
  transcriptRecovery?: boolean;
  runs?: ResumableRun[];
  cwd: string;
  modifiedAt: number;
  firstUserMessage: string;
  sizeBytes: number;
}
export interface ResumableRun {
  sessionId: string;
  name?: string;
  // "user" for a person, "session" for a lane started by another lane.
  creatorKind?: string;
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
  transcriptRecovery?: boolean;
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

interface MessageDeliveryReceipt {
  operation_id: string;
  session_id: string;
  status: 'accepted' | 'not-delivered' | 'unknown' | 'text-delivered';
  delivered: boolean;
  retry: boolean;
  reason?: string;
  duplicate?: boolean;
}

function deliveryError(receipt: MessageDeliveryReceipt): Error {
  if (receipt.status === 'not-delivered' && receipt.retry) {
    return new Error(receipt.reason || 'The session could not accept the message. It is safe to try again.');
  }
  if (receipt.status === 'text-delivered') {
    return new Error('The message text reached the session, but Sessions could not confirm Enter. Check the conversation before trying again.');
  }
  return new Error('Sessions could not confirm whether the message arrived. Check the conversation before trying again.');
}

async function readDeliveryResponse(response: Response): Promise<MessageDeliveryReceipt | { ok: true }> {
  let body: MessageDeliveryReceipt | { ok: true } | { error?: string };
  try {
    body = await response.json() as MessageDeliveryReceipt | { ok: true } | { error?: string };
  } catch {
    throw new Error(`sessionsd ${response.status}: ${response.statusText || 'invalid delivery response'}`);
  }
  // Compatibility with daemons from before durable delivery receipts.
  if ('ok' in body && body.ok === true) return body;
  if ('status' in body) return body;
  const detail = 'error' in body ? body.error : '';
  throw new Error(`sessionsd ${response.status}: ${detail || response.statusText}`);
}

// fromSessionId records another lane as the author of the message, the way
// `sessions send --from` does, so a hand-back reads in the manager's history
// as coming from the lane rather than from the person.
export async function submitMessage(sessionId: string, data: string, serverId?: string, fromSessionId?: string): Promise<void> {
  const server = requestedServer(serverId);
  const operationId = randomUUID();
  let response: Response;
  try {
    response = await serverFetch(server, `${httpBaseForServer(server)}/api/sessions/${encodeURIComponent(sessionId)}/submit`, {
      method: 'POST',
      headers: {
        'content-type': 'application/json',
        ...(fromSessionId ? { 'X-Sessions-Creator-Session': fromSessionId } : {})
      },
      body: JSON.stringify({ data, operation_id: operationId })
    });
  } catch (initialError) {
    // A broken response does not prove a broken send. Ask the daemon for the
    // durable, content-free receipt before allowing a person or agent to
    // retry and accidentally duplicate the message.
    try {
      const receiptResponse = await serverFetch(
        server,
        `${httpBaseForServer(server)}/api/message-deliveries/${encodeURIComponent(operationId)}`
      );
      const recovered = await readDeliveryResponse(receiptResponse);
      if ('ok' in recovered || recovered.status === 'accepted') return;
      throw deliveryError(recovered);
    } catch (receiptError) {
      if (receiptError instanceof AuthError) throw receiptError;
      throw new Error('The connection changed while sending. Sessions could not confirm delivery, so it did not retry. Check the conversation before sending again.', { cause: initialError });
    }
  }
  const receipt = await readDeliveryResponse(response);
  if ('ok' in receipt || receipt.status === 'accepted') return;
  throw deliveryError(receipt);
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

// Identity of the mux endpoint a live stream is bound to. Because the token
// lives in the URL, saving a credential for the already-selected machine
// produces a different socket without changing the active server id. Effects
// that own a mux subscription must key on this, not on the id alone —
// otherwise the terminal stays attached to the old tokenless URL and
// reconnects forever with a credential the daemon rejects. Returns '' while no
// server is configured so it is safe to call during render.
export function muxEndpointKey(): string {
  try { return wsMuxUrl(); }
  catch { return ''; }
}

export interface ProjectView {
  id: string;
  name: string;
  implicit: boolean;
  roots: string[];
  github?: string;
  somewhere?: string;
  pinned?: boolean;
  session_ids: string[];
  live: number;
  needs_input: number;
  updated_at?: number;
}

// The daemon groups sessions by the work they belong to (a folder, a git
// checkout with its worktrees, or a Somewhere project). Older daemons have no
// such route; an empty list means "group by nothing", not an error.
export async function fetchProjects(signal?: AbortSignal): Promise<ProjectView[]> {
  const server = getActiveServer();
  const r = await serverFetch(server, `${httpBaseForServer(server)}/api/projects`, { signal });
  if (r.status === 404 || r.status === 501) return [];
  const body = await json<{ projects: ProjectView[] }>(r);
  return body.projects ?? [];
}

// approveSession answers the permission a Rich lane is waiting on.
// fromSessionId attributes the decision to a lane, the way `sessions approve`
// run inside a manager does; a person deciding passes nothing.
export async function approveSession(
  sessionId: string,
  decision: 'allow' | 'allow-session' | 'deny',
  serverId?: string,
  fromSessionId?: string
): Promise<void> {
  const server = requestedServer(serverId);
  const response = await serverFetch(server, `${httpBaseForServer(server)}/api/sessions/${encodeURIComponent(sessionId)}/approve`, {
    method: 'POST',
    headers: {
      'content-type': 'application/json',
      ...(fromSessionId ? { 'X-Sessions-Creator-Session': fromSessionId } : {})
    },
    body: JSON.stringify({ decision })
  });
  if (!response.ok) {
    let message = `The lane did not take the answer (HTTP ${response.status}).`;
    try {
      const body = await response.json() as { error?: string };
      if (body.error) message = body.error;
    } catch {
      // The status is the message.
    }
    throw new Error(message);
  }
}

async function postProviderRetryAction(sessionId: string, tail: string): Promise<void> {
  const response = await apiFetch(
    `${httpBase()}/api/sessions/${encodeURIComponent(sessionId)}/retry${tail}`,
    { method: 'POST' }
  );
  if (response.ok) return;
  let message = `Sessions could not change this retry (HTTP ${response.status}).`;
  try {
    const body = await response.json() as { error?: string };
    if (body.error?.trim()) message = body.error;
  } catch {
    // The status-based message remains actionable when the body is not JSON.
  }
  throw new Error(message);
}

export async function retryProviderSession(sessionId: string): Promise<void> {
  await postProviderRetryAction(sessionId, '');
}

export async function stopProviderRetry(sessionId: string): Promise<void> {
  await postProviderRetryAction(sessionId, '/stop');
}
