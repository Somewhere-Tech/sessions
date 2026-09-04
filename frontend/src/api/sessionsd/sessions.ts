import type { SessionInfo } from '../../types';
import {
  getActiveServer,
  getServer,
  type ServerConfig
} from '../../lib/servers';
import {
  apiFetch,
  featureJSON,
  httpBase,
  httpBaseForServer,
  json,
  requestedServer,
  serverFetch
} from './core';

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

// The daemon answers with the state it actually stored rather than the one that
// was requested, so a daemon that accepted the call and did something else is
// visible instead of silently assumed.
export async function updatePinned(
  sessionId: string,
  pinned: boolean
): Promise<boolean> {
  const r = await apiFetch(`${httpBase()}/api/sessions/${encodeURIComponent(sessionId)}/pin`, {
    method: 'PUT',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ pinned })
  });
  const body = await featureJSON<{ pinned: boolean }>(r, 'Session pinning');
  return body.pinned;
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
  server: ServerConfig = getActiveServer(),
  signal?: AbortSignal
): Promise<MachineAccessRequest[] | null> {
  let r = await serverFetch(server, `${httpBaseForServer(server)}/api/access/requests`, { signal });
  if (r.status === 404) {
    r = await serverFetch(server, `${httpBaseForServer(server)}/api/tailnet/access/requests`, { signal });
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

export function normalizeSessionInfo(session: SessionInfo): SessionInfo {
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
  restore?: { pending: number; automaticPinnedLimit: number };
}

export const API_PROTOCOL_VERSION = 1;

// A reachable, identified sessionsd whose API range excludes this client.
// Distinguishable from "this origin is not a daemon at all" so a caller that
// silently ignores the latter can still surface the instructional message for
// the former — see bootstrapCurrentOriginServer in lib/servers.ts.
export class ServerCompatibilityError extends Error {
  readonly code = 'incompatible' as const;
  constructor(message: string) {
    super(message);
    this.name = 'ServerCompatibilityError';
  }
}

function validateServerHealth(health: ServerHealth): ServerHealth {
  if (!health.ok || health.name !== 'sessionsd') {
    throw new Error('unexpected health response');
  }
  const range = health.compatibility?.api;
  if (
    range
    && (API_PROTOCOL_VERSION < range.minimumClient || API_PROTOCOL_VERSION > range.maximumClient)
  ) {
    throw new ServerCompatibilityError(
      `This client uses Sessions API ${API_PROTOCOL_VERSION}, but the machine accepts ${range.minimumClient}–${range.maximumClient}. Update Sessions on this device or the host.`
    );
  }
  return health;
}

export async function fetchActiveServerHealth(signal?: AbortSignal): Promise<ServerHealth> {
  const server = getActiveServer();
  const r = await serverFetch(server, `${httpBaseForServer(server)}/api/health`, { signal }, Boolean(server.relayMachineId));
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
  permission?: {
    status: 'granted' | 'denied' | 'not-yet-asked' | 'not-required';
    reason?: 'local-network-permission';
    message?: string;
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
    Boolean(server.relayMachineId)
  );
  return validateServerHealth(await json<ServerHealth>(r));
}

// Revoking a device credential on ANOTHER machine, using the credential
// itself. hostedBootstrap called `fetch` directly for this, which skipped the
// client's auth header construction and its 401 translation; the request is
// otherwise identical.
export async function revokeServerDevice(
  server: ServerConfig,
  deviceId: string
): Promise<boolean> {
  const r = await serverFetch(
    server,
    `${httpBaseForServer(server)}/api/devices/${encodeURIComponent(deviceId)}`,
    { method: 'DELETE' }
  );
  return r.ok;
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
