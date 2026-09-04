import {
  adoptCurrentOriginServer,
  assertServerPersisted,
  captureServerSelection,
  restoreServerSelection,
  syncNativeAgentMachineAccess,
  useServers,
  type ServerConfig
} from './servers';
import { revokeServerDevice } from '../api/sessionsd';
import { isLoopbackHost, isPrivateNetworkHost, parseServerEndpoint } from './serverEndpoint';
import { claimNativePairingLink, type NativePairingClaim } from './tauriBridge';

function matchesEndpoint(
  server: ServerConfig,
  endpoint: ReturnType<typeof parseServerEndpoint>
): boolean {
  return (server.scheme ?? 'http') === endpoint.scheme
    && server.host.toLowerCase() === endpoint.host
    && server.port === endpoint.port;
}

function scrubFragment(): void {
  window.history.replaceState(
    window.history.state,
    '',
    `${window.location.pathname}${window.location.search}`
  );
}

function scrubPairingLocation(): void {
  const pathname = /^\/pair\/[^/]+$/.test(window.location.pathname)
    ? '/'
    : window.location.pathname;
  window.history.replaceState(
    window.history.state,
    '',
    `${pathname}${window.location.search}`
  );
}

interface RememberServerOptions {
  name?: string;
  systemName?: string;
  machineId?: string;
  deviceId?: string;
  token?: string | null;
  select?: boolean;
  allowPrivateHTTP?: boolean;
  lanEndpoint?: string;
  tailnetEndpoint?: string;
  tailnetIpEndpoint?: string;
  transport?: 'lan' | 'tailnet' | 'tailnet-ip';
}

function pairingClaimTransport(claim: NativePairingClaim): 'lan' | 'tailnet' | 'tailnet-ip' {
  if (claim.endpoint === claim.tailnetIpEndpoint) return 'tailnet-ip';
  if (claim.endpoint === claim.tailnetEndpoint || claim.endpoint.toLowerCase().startsWith('https://')) return 'tailnet';
  return 'lan';
}

type ConnectionFields = Pick<RememberServerOptions, 'lanEndpoint' | 'tailnetEndpoint' | 'tailnetIpEndpoint' | 'transport'>;

function connectionFields(source: RememberServerOptions): Partial<ConnectionFields> {
  return Object.fromEntries(
    ['lanEndpoint', 'tailnetEndpoint', 'tailnetIpEndpoint', 'transport']
      .map((key) => [key, source[key as keyof RememberServerOptions]])
      .filter((entry) => entry[1] !== undefined)
  );
}

function claimConnectionFields(claim: NativePairingClaim): Partial<ConnectionFields> {
  return connectionFields({ ...claim, transport: pairingClaimTransport(claim) });
}

// Shared first-run/add-server path. Hosted browser connections require TLS,
// with loopback HTTP as the deliberate exception for a daemon on the device
// running the browser. The endpoint is upserted so rescanning a QR link never
// creates duplicates.
export async function rememberServerEndpoint(
  endpointValue: string,
  options: RememberServerOptions = {}
): Promise<ServerConfig> {
  const endpoint = parseServerEndpoint(endpointValue);
  if (
    endpoint.scheme === 'http'
    && !isLoopbackHost(endpoint.host)
    && !(options.allowPrivateHTTP && isPrivateNetworkHost(endpoint.host))
  ) {
    throw new Error('Use HTTPS for remote servers. HTTP is allowed only on localhost.');
  }

  const store = useServers.getState();
  const machineId = options.machineId?.trim();
  const matchingMachine = machineId
    ? store.servers.find((server) => server.machineId === machineId)
    : undefined;
  const matchingEndpoint = store.servers.find((server) => matchesEndpoint(server, endpoint));
  const existing = matchingMachine ?? matchingEndpoint;
  const tokenUpdate = options.token === undefined
    ? {}
    : { token: options.token?.trim() || undefined };
  const name = options.name?.trim();
  const systemName = options.systemName?.trim();
  const connection = connectionFields(options);

  if (existing) {
    await store.updateServer(existing.id, {
      ...endpoint,
      ...(machineId ? { machineId } : {}),
      ...(options.deviceId ? { deviceId: options.deviceId } : {}),
      ...connection,
      ...tokenUpdate,
      ...(name ? { name, customName: name } : {}),
      ...(systemName ? { systemName, ...(!existing.customName ? { name: systemName } : {}) } : {})
    });
    // A pre-identity/manual entry can match the endpoint while another entry
    // already carries this machine ID. Collapse both access paths into the
    // stable machine entry instead of leaving a duplicate in Fleet.
    for (const duplicate of store.servers) {
      if (
        duplicate.id !== existing.id
        && !duplicate.isDefault
        && (
          (machineId && duplicate.machineId === machineId)
          || matchesEndpoint(duplicate, endpoint)
        )
      ) {
        await useServers.getState().removeServer(duplicate.id);
      }
    }
    if (options.select !== false) useServers.getState().setActive(existing.id);
    return useServers.getState().servers.find((server) => server.id === existing.id) ?? existing;
  }

  const created = await store.addServer({
    name: name || systemName || endpoint.host,
    ...(name ? { customName: name } : {}),
    ...(systemName ? { systemName } : {}),
    ...(machineId ? { machineId } : {}),
    ...(options.deviceId ? { deviceId: options.deviceId } : {}),
    ...connection,
    ...endpoint,
    ...tokenUpdate
  });
  if (options.select !== false) store.setActive(created.id);
  return created;
}

export async function claimNativeMachinePairing(
  pairingLink: string
): Promise<{ claim: NativePairingClaim; server: ServerConfig }> {
  const claim = await claimNativePairingLink(pairingLink);
  const previous = captureServerSelection();
  let server: ServerConfig;
  try {
    server = await rememberServerEndpoint(claim.endpoint, {
      systemName: claim.machineName,
      machineId: claim.machineId,
      deviceId: claim.deviceId,
      token: claim.token,
      ...claimConnectionFields(claim),
      allowPrivateHTTP: true
    });
    await assertServerPersisted(server);
    await syncNativeAgentMachineAccess();
  } catch {
    await restoreServerSelection(previous).catch(() => { /* keep the original save failure */ });
    // Avoid leaving an invisible live credential on the source machine when
    // native storage is unavailable. This is best-effort because the original
    // persistence error must remain the actionable message.
    let revoked = false;
    try {
      // Through the central client (api/sessionsd.ts) so this request gets the
      // same auth-header construction and 401 handling as every other call.
      // The claim is not in the server store yet, so the endpoint it names is
      // turned into a config here rather than looked up.
      revoked = await revokeServerDevice(
        { ...parseServerEndpoint(claim.endpoint), id: 'pairing-claim', name: claim.machineName, isDefault: false, token: claim.token },
        claim.deviceId
      );
    } catch { /* source device can also revoke it from Connections/CLI */ }
    throw new Error(
      revoked
        ? 'Sessions could not save this machine, so it revoked the new credential. Free local storage and pair again.'
        : 'Sessions could not save this machine. On the source Mac, revoke this device in Connections or with `sessions devices`, then pair again.'
    );
  }
  return { claim, server };
}

export async function rememberNativeMachineClaim(
  claim: NativePairingClaim,
  options: { select?: boolean } = {}
): Promise<ServerConfig> {
  const previous = captureServerSelection();
  let server: ServerConfig;
  try {
    server = await rememberServerEndpoint(claim.endpoint, {
      systemName: claim.machineName,
      machineId: claim.machineId,
      deviceId: claim.deviceId,
      token: claim.token,
      ...claimConnectionFields(claim),
      allowPrivateHTTP: claim.endpoint.toLowerCase().startsWith('http://')
    });
    await assertServerPersisted(server);
    await syncNativeAgentMachineAccess();
    if (options.select === false) useServers.getState().setActive(previous.activeId);
  } catch {
    await restoreServerSelection(previous).catch(() => { /* keep the original save failure */ });
    // Tailnet credentials remain non-authorizing until their first successful
    // authenticated API use. Do not use this token merely to revoke it: that
    // would acknowledge it in one durable write before deletion in another.
    // Leaving it untouched makes storage failure fail closed at its deadline.
    throw new Error('Sessions could not save this machine. The unacknowledged credential will expire automatically; free local storage and request access again.');
  }
  return server;
}

// Hosted connection links have the form:
//   #endpoint=https%3A%2F%2Fmac.example.com&token=secret
//
// Run once before React mounts. The hash is scrubbed before parsing or
// touching storage so a token never remains visible if validation fails.
export async function bootstrapHostedConnection(): Promise<void> {
  if (typeof window === 'undefined' || !window.location.hash) return;

  const params = new URLSearchParams(window.location.hash.slice(1));
  if (!params.has('endpoint')) return;

  const endpointValue = params.get('endpoint') ?? '';
  const tokenValue = params.get('token');
  scrubFragment();

  try {
    await rememberServerEndpoint(endpointValue, { token: tokenValue });
  } catch (error) {
    const store = useServers.getState();
    store.setPairingError(
      error instanceof Error
        ? error.message
        : 'Sessions could not protect and save this machine credential.'
    );
    store.setActive(null);
  }
}

interface PairClaimResponse {
  device_id: string;
  token: string;
  name: string;
}

function pairingTicket(value: string): string {
  const trimmed = value.trim();
  if (!trimmed) return '';
  if (!trimmed.includes('/') && !trimmed.startsWith('#')) return trimmed;
  try {
    const parsed = new URL(trimmed, window.location.origin);
    if (parsed.protocol === 'sessions:' && parsed.hostname === 'pair') {
      return parsed.searchParams.get('t')?.trim() ?? '';
    }
    const pathMatch = parsed.pathname.match(/^\/pair\/([^/]+)$/);
    if (pathMatch) return decodeURIComponent(pathMatch[1]).trim();
    const fromFragment = new URLSearchParams(parsed.hash.slice(1)).get('pair');
    if (fromFragment) return fromFragment.trim();
    return '';
  } catch {
    // A bare ticket is the normal Settings input; return it unchanged.
  }
  if (trimmed.startsWith('#')) {
    return new URLSearchParams(trimmed.slice(1)).get('pair')?.trim() ?? '';
  }
  return trimmed;
}

function pairClaimError(status: number, body: unknown): Error {
  if (typeof body === 'object' && body !== null) {
    const error = (body as Record<string, unknown>).error;
    if (typeof error === 'string' && error.trim()) return new Error(error.trim());
  }
  return new Error(`Pairing failed (HTTP ${status}). Run sessions pair again.`);
}

// Claim only against the page's own daemon origin. Pairing tickets are never
// sent to a configured cross-origin server or to the hosted app shell.
export async function claimCurrentOriginPairing(
  ticketValue: string,
  name?: string
): Promise<PairClaimResponse> {
  const ticket = pairingTicket(ticketValue);
  if (!ticket) throw new Error('Paste a pairing ticket from `sessions pair`.');

  // Deliberately a bare fetch, not the central client. This is the call that
  // OBTAINS the credential, so there is no token to inject; and a 401 here
  // means "this ticket is not valid", which must stay the instructional
  // `pairClaimError` message below rather than being translated into the
  // AuthError that drives the "enter your daemon token" banner. The API-range
  // check does not apply to a pairing claim either — it is validated against
  // /api/health by adoptCurrentOriginServer's caller path immediately after.
  const response = await fetch(`${window.location.origin}/api/lan/access/claim`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ ticket, ...(name?.trim() ? { name: name.trim() } : {}) })
  });
  let body: unknown = null;
  try {
    body = await response.json();
  } catch {
    // The status-specific fallback below remains instructional.
  }
  if (!response.ok) throw pairClaimError(response.status, body);
  if (typeof body !== 'object' || body === null) {
    throw new Error('Pairing succeeded without a device credential. Run `sessions pair` again.');
  }
  const claimed = body as Partial<PairClaimResponse>;
  if (!claimed.device_id || !claimed.token || !claimed.name) {
    throw new Error('Pairing succeeded with an invalid device credential. Run `sessions pair` again.');
  }

  await adoptCurrentOriginServer(claimed.token);
  return claimed as PairClaimResponse;
}

// Run before every other bootstrap. The fragment is scrubbed before the
// network request so even an expired or malformed ticket never stays visible.
export async function bootstrapPairingConnection(): Promise<boolean> {
  if (typeof window === 'undefined') return false;
  const ticket = pairingTicket(window.location.href);
  if (!ticket) return false;
  scrubPairingLocation();
  try {
    await claimCurrentOriginPairing(ticket);
  } catch (error) {
    const detail = error instanceof Error ? error.message : 'Pairing failed. Run `sessions pair` again.';
    const store = useServers.getState();
    store.setPairingError(detail);
    store.setActive(null);
  }
  return true;
}
