import { create } from 'zustand';
import { randomUUID } from './uuid';
import {
  isTauri,
  loadNativeMachineCredentials,
  saveNativeMachineCredentials,
  syncNativeAgentMachines,
  type NativeMachineCredential
} from './tauriBridge';
import { readWindowScope } from './windowScope';

// A "server" is a sessionsd instance reachable over the network. The user can
// have multiple — their Mac Mini on Tailscale, their local MacBook, a Fly
// machine, etc. — and switch between them. The frontend changes its REST/WS
// base URLs based on whichever server is active. Browser builds persist the
// complete list in localStorage. Windows native builds keep only non-secret
// metadata there and hydrate tokens from the signed-in user's DPAPI vault
// before the first daemon request.

export interface ServerConfig {
  id: string;
  // Stable daemon identity returned only after a successful pairing claim.
  // A machine can have LAN and Tailscale endpoints without becoming two fleet
  // entries; endpoint URLs are access paths, not machine identity.
  machineId?: string;
  // The host-side revocation identity returned with a successful pairing.
  deviceId?: string;
  // The hostname most recently reported by the authenticated daemon. It may
  // change while machineId stays stable (for example after renaming a Mac).
  systemName?: string;
  // A user-assigned Fleet label is separate from the reported hostname.
  customName?: string;
  name: string;
  host: string;
  port: number;
  isDefault: boolean;
  // Optional auth token (contract #1).  When present every HTTP request
  // carries `Authorization: Bearer <token>` and every WS URL gets
  // `?token=<token>` appended.  Absent → no auth (open daemon).
  token?: string;
  // Transport scheme.  Defaults to 'http' so existing stored configs
  // (which have no scheme field) continue to work without migration.
  scheme?: 'http' | 'https';
  // Client-only viewers reach inherited fleet machines through the one host
  // they paired with. These runtime-only fields are never persisted: the host
  // remains the credential owner and refreshes the inherited set.
  relayParentId?: string;
  relayMachineId?: string;
}

function friendlyReportedMachineName(value: string): string {
  const normalized = value
    .replace(/\.local$/i, '')
    .replace(/[-_]+/g, ' ')
    .replace(/\s+/g, ' ')
    .trim();
  if (/^mac\s*mini(?:\s+\d+)?$/i.test(normalized)) return 'Mac mini';
  if (/^macbook\s*pro(?:\s+\d+)?$/i.test(normalized)) return 'MacBook Pro';
  if (/^macbook\s*air(?:\s+\d+)?$/i.test(normalized)) return 'MacBook Air';
  if (/^macbook(?:\s+\d+)?$/i.test(normalized)) return 'MacBook';
  if (/^imac(?:\s+\d+)?$/i.test(normalized)) return 'iMac';
  return normalized || 'Computer';
}

export function serverDisplayName(server: ServerConfig, annotateLocal = false): string {
  const custom = server.customName?.trim();
  const reported = server.systemName?.trim();
  const legacy = server.name?.trim();
  if (custom) return custom;
  if (annotateLocal && server.isDefault && isLocalServer(server)) {
    const source = reported || legacy || '';
    if (/mac/i.test(source)) return 'This Mac';
    if (/windows|desktop|surface|pc\b/i.test(source)) return 'This PC';
    return 'This computer';
  }
  const source = reported || (legacy && legacy !== 'This machine' ? legacy : '') || 'This computer';
  return friendlyReportedMachineName(source);
}

const STORAGE_KEY = 'sessions:servers';
const ACTIVE_KEY = 'sessions:active-server';

function embeddedServer(): ServerConfig | null {
  if (typeof window === 'undefined') return null;
  // Tauri's page origin is its asset protocol, not the daemon. A fresh
  // desktop install still needs to be zero-configuration and talk directly
  // to the loopback daemon managed by `sessions install`.
  if (isTauri()) {
    return {
      id: 'local',
      name: 'This machine',
      host: 'localhost',
      port: 8787,
      isDefault: true,
      scheme: 'http'
    };
  }
  const scheme = window.location.protocol === 'https:' ? 'https' : 'http';
  const port = window.location.port
    ? Number(window.location.port)
    : (scheme === 'https' ? 443 : 80);

  // The normal daemon origin should remain zero-configuration: the same
  // production build served by sessionsd at localhost:8787 goes straight to
  // the session list. A static preview or hosted site uses another port and
  // deliberately starts with no server, so its first paint is the picker.
  if (port !== 8787) return null;
  return {
    id: 'local',
    name: 'This machine',
    host: window.location.hostname,
    port,
    isDefault: true,
    scheme
  };
}

function readServers(): ServerConfig[] {
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) {
      const embedded = embeddedServer();
      return embedded ? [embedded] : [];
    }
    const parsed = JSON.parse(raw) as ServerConfig[];
    if (!Array.isArray(parsed)) return [];
    return parsed;
  } catch {
    return [];
  }
}

let nativeCredentialStoreEnabled = false;
let nativeCredentialStoreBlockedError: Error | null = null;
let nativePersistence: Promise<void> = Promise.resolve();

function withoutTokens(servers: ServerConfig[]): ServerConfig[] {
  return servers.map(({ token: _token, ...server }) => server);
}

function persistentServers(servers: ServerConfig[]): ServerConfig[] {
  return servers.filter((server) => !server.relayMachineId);
}

function writeServerMetadata(servers: ServerConfig[]): boolean {
  try {
    const persistent = persistentServers(servers);
    window.localStorage.setItem(
      STORAGE_KEY,
      JSON.stringify(nativeCredentialStoreEnabled ? withoutTokens(persistent) : persistent)
    );
    return true;
  } catch {
    return false;
  }
}

function machineCredentials(servers: ServerConfig[]): NativeMachineCredential[] {
  return servers.flatMap((server) => !server.relayMachineId && server.token
    ? [{ serverId: server.id, token: server.token }]
    : []);
}

function credentialsMatch(
  expected: NativeMachineCredential[],
  actual: NativeMachineCredential[]
): boolean {
  if (expected.length !== actual.length) return false;
  const byServer = new Map(actual.map((credential) => [credential.serverId, credential.token]));
  return expected.every((credential) => byServer.get(credential.serverId) === credential.token);
}

async function persistServers(servers: ServerConfig[]): Promise<void> {
  if (nativeCredentialStoreBlockedError) {
    throw nativeCredentialStoreBlockedError;
  }
  if (!nativeCredentialStoreEnabled) {
    writeServerMetadata(servers);
    return;
  }
  const credentials = machineCredentials(servers);
  const save = nativePersistence
    .catch(() => { /* a later explicit save may repair an earlier failure */ })
    .then(async () => {
      const previous = await loadNativeMachineCredentials();
      if (!previous.supported) {
        throw new Error('The protected Windows machine credential store is unavailable.');
      }
      const stored = await saveNativeMachineCredentials(credentials);
      if (!stored.supported || !credentialsMatch(credentials, stored.credentials)) {
        throw new Error('Sessions could not verify the protected Windows machine credentials.');
      }
      if (!writeServerMetadata(servers)) {
        try {
          const restored = await saveNativeMachineCredentials(previous.credentials);
          if (
            !restored.supported
            || !credentialsMatch(previous.credentials, restored.credentials)
          ) {
            throw new Error('the previous credential set did not verify');
          }
        } catch {
          nativeCredentialStoreBlockedError = new Error(
            'Sessions could not finish or safely roll back the protected Windows credential update. Reopen the app before changing saved machines.'
          );
          throw nativeCredentialStoreBlockedError;
        }
        throw new Error(
          'Sessions could not update local machine metadata, so it restored the previous protected credentials.'
        );
      }
    });
  nativePersistence = save;
  await save;
}

function readActiveId(): string {
  try {
    return window.localStorage.getItem(ACTIVE_KEY) ?? '';
  } catch {
    return '';
  }
}

function writeActiveId(id: string | null): void {
  try {
    if (id) window.localStorage.setItem(ACTIVE_KEY, id);
    else window.localStorage.removeItem(ACTIVE_KEY);
  } catch { /* ignore */ }
}

export interface ServerSelectionSnapshot {
  servers: ServerConfig[];
  activeId: string | null;
}

export function captureServerSelection(): ServerSelectionSnapshot {
  const state = useServers.getState();
  return {
    servers: state.servers.map((server) => ({ ...server })),
    activeId: state.activeId
  };
}

export async function restoreServerSelection(snapshot: ServerSelectionSnapshot): Promise<void> {
  const servers = snapshot.servers.map((server) => ({ ...server }));
  await persistServers(servers);
  writeActiveId(snapshot.activeId);
  useServers.setState({ servers, activeId: snapshot.activeId });
}

// Pairing creates a live remote credential, so unlike ordinary preference
// updates it must not report success until the credential and active selection
// can be read back from durable browser or native storage.
export async function assertServerPersisted(server: ServerConfig): Promise<void> {
  try {
    await nativePersistence;
    const encoded = window.localStorage.getItem(STORAGE_KEY);
    const parsed = encoded ? JSON.parse(encoded) as ServerConfig[] : null;
    const stored = Array.isArray(parsed)
      ? parsed.find((candidate) => candidate.id === server.id)
      : undefined;
    if (
      !stored
      || stored.machineId !== server.machineId
      || stored.host !== server.host
      || stored.port !== server.port
      || (stored.scheme ?? 'http') !== (server.scheme ?? 'http')
      || window.localStorage.getItem(ACTIVE_KEY) !== server.id
    ) {
      throw new Error('stored server did not match the paired machine');
    }
    if (nativeCredentialStoreEnabled) {
      if (stored.token !== undefined) {
        throw new Error('the browser metadata still contained a plaintext credential');
      }
      const native = await loadNativeMachineCredentials();
      const credential = native.credentials.find((candidate) => candidate.serverId === server.id);
      if (!native.supported || credential?.token !== server.token) {
        throw new Error('the protected credential did not match the paired machine');
      }
    } else if (stored.token !== server.token) {
      throw new Error('the stored credential did not match the paired machine');
    }
  } catch {
    throw new Error('Sessions could not durably save this machine.');
  }
}

interface ServersStore {
  servers: ServerConfig[];
  activeId: string | null;
  // Runtime-only auth state for an embedded same-origin daemon. This is not
  // part of ServerConfig and is deliberately never persisted.
  tokenRequiredServerId: string | null;
  // Runtime-only error from an attempted one-time pairing claim. Keeping it
  // in the server store lets bootstrap surface the failure after React mounts
  // without putting the ticket back into the URL or persistent storage.
  pairingError: string | null;
  // A native credential-store failure is not an ordinary pairing error. It
  // blocks all connection attempts until restart so no request can run with
  // missing, stale, or unverified credentials.
  credentialError: string | null;
  addServer: (s: Omit<ServerConfig, 'id' | 'isDefault'>) => Promise<ServerConfig>;
  removeServer: (id: string) => Promise<void>;
  // Patch fields on an existing server (e.g. save a token entered after a
  // 401, or flip scheme). Windows resolves only after DPAPI persistence.
  updateServer: (
    id: string,
    updates: Partial<Omit<ServerConfig, 'id' | 'isDefault'>>
  ) => Promise<void>;
  markTokenRequired: (id: string) => void;
  setPairingError: (error: string | null) => void;
  setCredentialError: (error: string | null) => void;
  setActive: (id: string | null) => void;
  // Resolve the live config for the active server. Falls back to the
  // default if the saved active id no longer exists (e.g. user removed
  // it on another tab).
  active: () => ServerConfig | null;
}

const initial = (() => {
  const servers = readServers();
  const savedActive = readActiveId();
  const windowScope = readWindowScope();
  const scopedServerId = windowScope?.kind === 'server' ? windowScope.value : '';
  const activeId = servers.some((s) => s.id === scopedServerId)
    ? scopedServerId
    : servers.some((s) => s.id === savedActive)
    ? savedActive
    : (servers.find((s) => s.isDefault) ?? servers[0])?.id ?? null;
  return { servers, activeId };
})();

export const useServers = create<ServersStore>((set, get) => ({
  servers: initial.servers,
  activeId: initial.activeId,
  tokenRequiredServerId: null,
  pairingError: null,
  credentialError: null,

  addServer: async (s) => {
    const next: ServerConfig = {
      ...s,
      id: randomUUID(),
      isDefault: false
    };
    const previous = get().servers;
    const servers = [...previous, next];
    set({ servers });
    try {
      await persistServers(servers);
    } catch (error) {
      if (get().servers === servers) set({ servers: previous });
      throw error;
    }
    return next;
  },

  removeServer: async (id) => {
    const state = get();
    // Keep the daemon-origin entry stable. Hosted entries are removable;
    // removing the final one returns the user to the connection screen.
    const target = state.servers.find((server) => server.id === id);
    if (!target || target.isDefault) return;
    const servers = state.servers.filter((server) => server.id !== id && server.relayParentId !== id);
    const activeId = state.activeId === id
      ? (servers.find((server) => server.isDefault) ?? servers[0])?.id ?? null
      : state.activeId;
    set({ servers, activeId });
    try {
      await persistServers(servers);
      if (activeId !== state.activeId) writeActiveId(activeId);
    } catch (error) {
      const current = get();
      if (current.servers === servers) {
        set({ servers: state.servers, activeId: state.activeId });
      }
      throw error;
    }
  },

  updateServer: async (id, updates) => {
    const state = get();
    const servers = state.servers.map((server) =>
      server.id === id ? { ...server, ...updates } : server
    );
    const tokenRequiredServerId = state.tokenRequiredServerId === id && updates.token
      ? null
      : state.tokenRequiredServerId;
    set({ servers, tokenRequiredServerId });
    try {
      await persistServers(servers);
    } catch (error) {
      const current = get();
      if (current.servers === servers) {
        set({
          servers: state.servers,
          tokenRequiredServerId: state.tokenRequiredServerId
        });
      }
      throw error;
    }
  },

  markTokenRequired: (id) => {
    if (!get().servers.some((s) => s.id === id)) return;
    set({ tokenRequiredServerId: id });
  },

  setPairingError: (error) => set({ pairingError: error }),

  setCredentialError: (error) => set({ credentialError: error }),

  setActive: (id) => {
    if (id === null) {
      writeActiveId(null);
      set({ activeId: null });
      return;
    }
    if (!get().servers.some((s) => s.id === id)) return;
    writeActiveId(id);
    set({ activeId: id });
  },

  active: () => {
    const { servers, activeId } = get();
    return servers.find((s) => s.id === activeId)
      ?? servers.find((s) => s.isDefault)
      ?? servers[0]
      ?? null;
  }
}));

// The installed native shell can move its managed loopback daemon away from
// 8787. Hydrate that persisted native choice before ConnectedApp mounts so the
// first API request, WS, tray, and visible server selector all agree.
export function configureNativeLocalPort(port: number): void {
  if (!Number.isInteger(port) || port < 1024 || port > 65535) return;
  const store = useServers.getState();
  const existing = store.servers.find((server) => server.id === 'local')
    ?? store.servers.find((server) => server.isDefault && isLocalServer(server));
  const local: ServerConfig = existing
    ? { ...existing, host: 'localhost', port, scheme: 'http', isDefault: true }
    : { id: 'local', name: 'This machine', host: 'localhost', port, scheme: 'http', isDefault: true };
  const servers = existing
    ? store.servers.map((server) => server.id === existing.id ? local : server)
    : [local, ...store.servers];
  const activeId = store.activeId ?? local.id;
  writeServerMetadata(servers);
  writeActiveId(activeId);
  useServers.setState({ servers, activeId });
}

// Windows, Android, iOS, and other client-only native builds do not own a
// loopback sessionsd. The shared bundle initially synthesizes the Mac's local
// entry before native lifecycle state is available, so remove only that
// synthetic/default loopback entry once the shell reports client-only mode.
// Remembered remote machines and their credentials are left untouched.
export function configureNativeClientOnly(): void {
  const store = useServers.getState();
  const servers = store.servers.filter((server) =>
    !(server.isDefault && server.id === 'local' && isLocalServer(server))
  );
  const activeId = servers.some((server) => server.id === store.activeId)
    ? store.activeId
    : servers[0]?.id ?? null;
  writeServerMetadata(servers);
  writeActiveId(activeId);
  useServers.setState({ servers, activeId });
}

function currentOriginServer(): ServerConfig {
  const scheme = window.location.protocol === 'https:' ? 'https' : 'http';
  const port = window.location.port
    ? Number(window.location.port)
    : (scheme === 'https' ? 443 : 80);
  return {
    id: 'local',
    name: 'This machine',
    host: window.location.hostname,
    port,
    isDefault: true,
    scheme
  };
}

function matchesCurrentOrigin(server: ServerConfig, current: ServerConfig): boolean {
  return (server.scheme ?? 'http') === current.scheme
    && server.host.toLowerCase() === current.host.toLowerCase()
    && server.port === current.port;
}

// Adopt the daemon serving this page, optionally attaching a freshly claimed
// per-device token. This is shared by the health-probe bootstrap and the
// pairing bootstrap so both paths create the same stable current-origin entry.
export async function adoptCurrentOriginServer(
  token?: string,
  tokenRequired = false
): Promise<ServerConfig> {
  const store = useServers.getState();
  const current = currentOriginServer();
  const existing = store.servers.find((server) => matchesCurrentOrigin(server, current));
  const tokenUpdate = token === undefined ? {} : { token: token.trim() || undefined };
  const adopted: ServerConfig = existing
    ? { ...existing, ...tokenUpdate, isDefault: true }
    : { ...current, ...tokenUpdate };
  const servers = existing
    ? store.servers.map((server) => server.id === existing.id ? adopted : server)
    : [...store.servers, adopted];

  await persistServers(servers);
  writeActiveId(adopted.id);
  useServers.setState({
    servers,
    activeId: adopted.id,
    tokenRequiredServerId: tokenRequired ? adopted.id : null,
    pairingError: null
  });
  return adopted;
}

function hasStoredServerList(): boolean {
  try {
    return window.localStorage.getItem(STORAGE_KEY) !== null;
  } catch {
    // If storage cannot be inspected, preserve the existing picker behavior.
    return true;
  }
}

// A non-8787 page may still be the UI served by sessionsd itself (for example
// its Tailscale HTTPS origin). With no saved configuration, probe that origin
// and adopt it only when the response identifies a daemon. Static hosted
// shells fall through unchanged when /api/health is absent or not sessionsd.
//
// This used to be a bare `fetch` with its own health test — `ok === true ||
// typeof name === 'string'` — which is strictly weaker than the client's
// `validateServerHealth`: it never looked at `compatibility.api`, so a daemon
// whose API range excludes this client was adopted silently, and the user got
// whatever confusing failure came next instead of the "Update Sessions on
// this device or the host" message that every other entry point produces. It
// now goes through the central client, which injects auth, translates 401,
// and runs the range check.
//
// The import is dynamic on purpose: api/sessionsd.ts imports this module for
// its server resolution, so a static import would close a module cycle for
// one startup probe.
export async function bootstrapCurrentOriginServer(): Promise<void> {
  if (typeof window === 'undefined') return;
  if (useServers.getState().servers.length > 0 || hasStoredServerList()) return;

  // The existing 8787 embeddedServer() path remains the fast path and must
  // never wait for a startup probe.
  if (embeddedServer()) return;

  const { AuthError, ServerCompatibilityError, fetchServerHealth } = await import('../api/sessionsd');

  let tokenRequired = false;
  try {
    await fetchServerHealth(currentOriginServer());
  } catch (error) {
    if (error instanceof AuthError) {
      // A daemon that answers 401 has identified itself well enough to adopt;
      // the token prompt is the next step, not a dead end.
      tokenRequired = true;
    } else if (error instanceof ServerCompatibilityError) {
      // Reachable, definitely sessionsd, and unusable by this client. Adopting
      // it would hide that; say so on the connect surface instead.
      useServers.getState().setPairingError(error.message);
      return;
    } else {
      // Unreachable, not a daemon, or an unrecognisable body: a static hosted
      // shell. Fall through silently exactly as before.
      return;
    }
  }

  await adoptCurrentOriginServer(undefined, tokenRequired);
}

// Windows moves legacy plaintext tokens out of WebView localStorage before
// any bootstrap path can issue a daemon request. The plaintext copy is removed
// only after the DPAPI-backed native store has saved and read back every token.
export async function hydrateNativeMachineCredentials(): Promise<void> {
  if (!isTauri()) return;
  const native = await loadNativeMachineCredentials();
  if (!native.supported) return;
  nativeCredentialStoreEnabled = true;

  const state = useServers.getState();
  const protectedByServer = new Map(
    native.credentials.map((credential) => [credential.serverId, credential.token])
  );
  const legacyCredentials = machineCredentials(state.servers);
  const servers = state.servers.map((server) => ({
    ...server,
    token: server.token ?? protectedByServer.get(server.id)
  }));
  const hydratedCredentials = machineCredentials(servers);

  if (
    legacyCredentials.length > 0
    || !credentialsMatch(hydratedCredentials, native.credentials)
  ) {
    await persistServers(servers);
  } else {
    writeServerMetadata(servers);
  }
  useServers.setState({ servers });
}

export function blockNativeMachineCredentialPersistence(detail: string): void {
  nativeCredentialStoreEnabled = false;
  nativeCredentialStoreBlockedError = new Error(detail);
}

// Keep the native UI and the agent-facing CLI on one approved-machine list.
// Only paired machines have a stable identity and device credential; manual
// endpoint drafts remain UI-local until the host approves them.
export async function syncNativeAgentMachineAccess(): Promise<void> {
  if (!isTauri()) return;
  const machines = useServers.getState().servers.flatMap((server) => {
    if (server.isDefault || server.relayMachineId || !server.machineId || !server.token) return [];
    return [{
      machineId: server.machineId,
      name: serverDisplayName(server),
      endpoint: `${server.scheme ?? 'http'}://${server.host}:${server.port}`,
      ...(server.deviceId ? { deviceId: server.deviceId } : {}),
      token: server.token
    }];
  });
  await syncNativeAgentMachines(machines);
}

// Non-reactive accessor for use inside api/sessionsd.ts and similar — those
// functions are called per request, and reading the latest value out of
// the store at call time is exactly what we want.
export function getActiveServer(): ServerConfig {
  const server = useServers.getState().active();
  if (!server) throw new Error('No sessions server is configured.');
  return server;
}

export function getServer(serverId: string): ServerConfig {
	const server = useServers.getState().servers.find((candidate) => candidate.id === serverId);
	if (!server) throw new Error(`Sessions server ${serverId} is no longer configured.`);
	return server;
}

// Local means the configured daemon is on browser loopback. It does not
// imply same-origin: a hosted HTTPS shell must still call the configured
// http://localhost endpoint directly rather than rewriting it to the site.
export function isLocalServer(s: ServerConfig): boolean {
  return s.host === '127.0.0.1' || s.host === 'localhost' || s.host === '::1' || s.host === '[::1]';
}
