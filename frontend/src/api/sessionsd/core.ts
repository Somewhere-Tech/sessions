import {
  getActiveServer,
  getServer,
  isLocalServer,
  useServers,
  type ServerConfig
} from '../../lib/servers';
import { isTauri } from '../../lib/tauriBridge';

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
  return `${scheme}://${hostForUrl(s.host)}:${s.port}`;
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
  return `${scheme}://${hostForUrl(s.host)}:${s.port}`;
}

// Returns `{ Authorization: 'Bearer <token>' }` when the supplied server has
// a token configured, or an empty object when open (no auth).
function authHeaders(s: ServerConfig): Record<string, string> {
  return s.token ? { Authorization: `Bearer ${s.token}` } : {};
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
  const res = await fetch(input, merged);
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

export async function json<T>(res: Response): Promise<T> {
  if (!res.ok) {
    const text = await res.text().catch(() => '');
    throw new Error(`sessionsd ${res.status}: ${text || res.statusText}`);
  }
  return res.json() as Promise<T>;
}

export async function featureJSON<T>(res: Response, feature: string): Promise<T> {
  if (res.status === 404) {
    throw new Error(`${feature} is not available on this runtime. Update Sessions or connect to a current sessionsd.`);
  }
  return json<T>(res);
}
