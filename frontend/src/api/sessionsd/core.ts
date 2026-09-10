import {
  getActiveServer,
  getServer,
  isLocalServer,
  useServers,
  type ServerConfig
} from '../../lib/servers';
import { isTauri } from '../../lib/tauriBridge';
import { parseServerEndpoint } from '../../lib/serverEndpoint';

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
function isSameOriginDaemon(s: ServerConfig): boolean {
  if (s.relayMachineId) return false;
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

export function httpBaseForServer(s: ServerConfig): string {
  if (isSameOriginDaemon(s)) {
    return window.location.origin;
  }
  // Honour the selected endpoint exactly. Falling back to HTTP keeps older
  // stored configs (which predate the scheme field) compatible.
  const scheme = s.scheme ?? 'http';
  const origin = `${scheme}://${hostForUrl(s.host)}:${s.port}`;
  if (s.basePath) return `${origin}${s.basePath}`;
  return s.relayMachineId
    ? `${origin}/api/fleet/${encodeURIComponent(s.relayMachineId)}`
    : origin;
}

export function httpBase(): string {
  return httpBaseForServer(getActiveServer());
}

export function requestedServer(serverId?: string): ServerConfig {
  return serverId ? getServer(serverId) : getActiveServer();
}

export function wsBase(): string {
  const s = getActiveServer();
  if (isSameOriginDaemon(s)) {
    const scheme = window.location.protocol === 'https:' ? 'wss' : 'ws';
    return `${scheme}://${window.location.host}`;
  }
  // Mirror the http→https / ws→wss mapping so TLS connections work end-to-end.
  const scheme = s.scheme === 'https' ? 'wss' : 'ws';
  const origin = `${scheme}://${hostForUrl(s.host)}:${s.port}`;
  if (s.basePath) return `${origin}${s.basePath}`;
  return s.relayMachineId
    ? `${origin}/api/fleet/${encodeURIComponent(s.relayMachineId)}`
    : origin;
}

// Returns `{ Authorization: 'Bearer <token>' }` when the supplied server has
// a token configured, or an empty object when open (no auth).
function authHeaders(s: ServerConfig): Record<string, string> {
  return s.token ? { Authorization: `Bearer ${s.token}` } : {};
}

const transportSelections = new Map<string, { endpoint: string; expires: number }>();

async function selectTransport(server: ServerConfig, headers: Record<string, string>): Promise<string> {
  const key = server.machineId ?? server.id;
  const cached = transportSelections.get(key);
  if (cached && cached.expires > Date.now()) return cached.endpoint;
  for (const candidate of server.transportCandidates ?? []) {
    try {
      const response = await fetch(`${candidate.endpoint.replace(/\/$/, '')}/api/machine`, {
        headers, redirect: 'error', signal: AbortSignal.timeout(5_000)
      });
      response.body?.cancel();
      if (!response.ok) continue;
      transportSelections.set(key, { endpoint: candidate.endpoint, expires: Date.now() + 30_000 });
      const parsed = parseServerEndpoint(candidate.endpoint);
      void useServers.getState().updateServer(server.id, { ...parsed, transport: candidate.transport });
      return candidate.endpoint;
    } catch { /* try the next route */ }
  }
  throw new Error('No machine transport is reachable.');
}

function requestThroughEndpoint(input: RequestInfo | URL, server: ServerConfig, endpoint: string): RequestInfo | URL {
  const requested = new URL(input instanceof Request ? input.url : input.toString(), window.location.origin);
  const currentBase = new URL(httpBaseForServer(server));
  let path = requested.pathname;
  if (currentBase.pathname !== '/' && path.startsWith(currentBase.pathname)) {
    path = path.slice(currentBase.pathname.length) || '/';
  }
  const rewritten = `${endpoint.replace(/\/$/, '')}${path}${requested.search}`;
  return input instanceof Request ? new Request(rewritten, input) : rewritten;
}

// Shared fetch path for active-server and explicit fleet requests. Injects
// auth when requested and translates 401 into the existing AuthError.
export async function serverFetch(
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
  const endpoint = authenticate && server.machineId && server.transportCandidates?.length
    ? await selectTransport(server, extra)
    : '';
  let res;
  try {
    res = await fetch(endpoint ? requestThroughEndpoint(input, server, endpoint) : input, merged);
  } catch (reason) {
    if (endpoint) transportSelections.delete(server.machineId ?? server.id);
    throw reason;
  }
  if (res.status === 401) {
    if (server.isDefault && isSameOriginDaemon(server)) {
      useServers.getState().markTokenRequired(server.id);
    }
    throw new AuthError();
  }
  return res;
}

export async function apiFetch(input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
  return serverFetch(getActiveServer(), input, init);
}

// Thrown when the daemon answered with a status this caller cannot use. The
// message is the complete body, because a failure body carries more than a
// sentence: a reboot-paused session sends `code`, `sessionId` and the exact
// `action` that recovers it, and dropping any of that would take the recovery
// instruction away from whoever is reading the error. `detail` is the daemon's
// own sentence, offered separately for a surface that wants to show one line.
export class DaemonResponseError extends Error {
  readonly status: number;
  readonly body: string;
  readonly detail: string;
  constructor(status: number, body: string, statusText: string) {
    super(`sessionsd ${status}: ${body || statusText}`);
    this.name = 'DaemonResponseError';
    this.status = status;
    this.body = body;
    this.detail = daemonErrorSentence(body) ?? `${body || statusText}`;
  }
}

export async function json<T>(res: Response): Promise<T> {
  if (!res.ok) {
    const text = await res.text().catch(() => '');
    throw new DaemonResponseError(res.status, text, res.statusText);
  }
  return res.json() as Promise<T>;
}

function daemonErrorSentence(body: string): string | null {
  try {
    const parsed = JSON.parse(body) as { error?: unknown };
    if (typeof parsed?.error === 'string' && parsed.error.trim()) return parsed.error;
  } catch { /* not a daemon JSON error */ }
  return null;
}

// One line for a surface with room for one line. Everything else the daemon
// sent stays on the error for callers that need it.
export function daemonErrorDisplay(error: unknown): string | null {
  if (error instanceof DaemonResponseError) return `sessionsd ${error.status}: ${error.detail}`;
  return error instanceof Error ? error.message : null;
}

export async function featureJSON<T>(res: Response, feature: string): Promise<T> {
  if (res.status === 404) {
    throw new Error(`${feature} is not available on this runtime. Update Sessions or connect to a current sessionsd.`);
  }
  return json<T>(res);
}
