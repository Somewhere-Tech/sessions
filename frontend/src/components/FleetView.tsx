import { useCallback, useEffect, useRef, useState, type Dispatch, type SetStateAction } from 'react';
import { fetchLANState, fetchServerHealth, listServerProfiles, listServerSessions, type AccountProfile, type ServerHealth } from '../api/sessionsd';
import { formatServerEndpoint } from '../lib/serverEndpoint';
import { serverDisplayName, useServers, type ServerConfig } from '../lib/servers';
import { tailnetClientID } from '../lib/tailnetClient';
import {
  discoverNativeNearbyPeers,
  discoverNativeTailnetPeers,
  isTauri,
  requestNativeNearbyAccess,
  requestNativeTailnetAccess,
  type NativeTailnetRequest
} from '../lib/tauriBridge';
import type { SessionInfo, SessionTool } from '../types';
import { classifySession, type SessionStatusState } from '../lib/sessionStatus';
import { ParserIcon } from './ParserIcon';
import { shortLabel } from './SessionTabs';
import {
  useMachineAccessPairing,
  type PendingMachineAccess as SharedPendingMachineAccess
} from '../hooks/useMachineAccessPairing';
import { MachinePlatformIcon } from './MachineMark';

const POLL_INTERVAL_MS = 3_000;
const POLL_TIMEOUT_MS = 5_000;

const TOOL_ICONS: Record<SessionTool, string> = {
  'claude-code': '🟠',
  'codex': '🟢',
  'terminal': '⬛'
};

type Reachability = 'checking' | 'reachable' | 'unreachable';

interface ServerSnapshot {
  reachability: Reachability;
  health: ServerHealth | null;
  sessions: SessionInfo[];
  profiles: AccountProfile[];
  sessionsError: string | null;
}

const INITIAL_SNAPSHOT: ServerSnapshot = {
  reachability: 'checking',
  health: null,
  sessions: [],
  profiles: [],
  sessionsError: null
};

interface FleetViewProps {
  onOpenSession: (serverId: string, sessionId: string) => void;
  onOpenMachine: (serverId: string) => void;
}

interface DiscoveredPeer {
  endpoint: string;
  hostname: string;
  name: string;
  os: string;
  transport: 'tailnet' | 'nearby';
}

interface PendingMachineAccess extends SharedPendingMachineAccess {
  request: NativeTailnetRequest;
  label: string;
}

// Fleet is deliberately client-side aggregation: each group owns its own
// polling loop and talks straight to its configured sessionsd. A slow or dead
// machine therefore cannot delay updates from any other machine.
export function FleetView({ onOpenSession, onOpenMachine }: FleetViewProps): JSX.Element {
  const servers = useServers((state) => state.servers);
  const [includeExited, setIncludeExited] = useState(false);
  const [machineVersions, setMachineVersions] = useState<Record<string, string>>({});
  const [discoveryOpen, setDiscoveryOpen] = useState(false);
  const [discoveryBusy, setDiscoveryBusy] = useState(false);
  const [discoveredPeers, setDiscoveredPeers] = useState<DiscoveredPeer[] | null>(null);
  const [accessRequest, setAccessRequest] = useState<PendingMachineAccess | null>(null);
  const [discoveryMessage, setDiscoveryMessage] = useState<string | null>(null);
  const localNetworkDenied = useLocalNetworkDenied();
  const localServer = servers.find((server) => server.isDefault) ?? servers[0];
  const localVersion = localServer ? machineVersions[localServer.id] : undefined;

  const rememberVersion = useRememberMachineVersion(setMachineVersions);

  const findMachines = async (): Promise<void> => {
    if (!isTauri() || discoveryBusy || accessRequest) return;
    setDiscoveryOpen(true);
    setDiscoveryBusy(true);
    setDiscoveryMessage(null);
    try {
      const [tailnet, nearby] = await Promise.allSettled([
        discoverNativeTailnetPeers(),
        discoverNativeNearbyPeers()
      ]);
      const peers = mergeDiscoveredPeers([
        ...(tailnet.status === 'fulfilled'
          ? tailnet.value.map((peer): DiscoveredPeer => ({
              ...peer,
              name: peer.hostname,
              transport: 'tailnet'
            }))
          : []),
        ...(nearby.status === 'fulfilled'
          ? nearby.value.map((peer): DiscoveredPeer => ({
              endpoint: peer.endpoint,
              hostname: peer.hostname,
              name: peer.name,
              os: peer.os,
              transport: 'nearby'
            }))
          : [])
      ]);
      setDiscoveredPeers(peers);
      const failures = [tailnet, nearby]
        .filter((result): result is PromiseRejectedResult => result.status === 'rejected')
        .map((result) => result.reason instanceof Error ? result.reason.message : String(result.reason));
      if (failures.length === 2) setDiscoveryMessage(failures.join(' · '));
      else if (failures.length === 1 && peers.length === 0) setDiscoveryMessage(failures[0]);
    } catch (reason) {
      setDiscoveredPeers([]);
      setDiscoveryMessage(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setDiscoveryBusy(false);
    }
  };

  const requestAccess = async (peer: DiscoveredPeer): Promise<void> => {
    if (!isTauri() || discoveryBusy || accessRequest) return;
    setDiscoveryBusy(true);
    setDiscoveryMessage(null);
    try {
      const request = peer.transport === 'nearby'
        ? await requestNativeNearbyAccess(peer.endpoint, tailnetClientID(), '')
        : await requestNativeTailnetAccess(peer.endpoint, tailnetClientID(), '');
      setAccessRequest({ request, transport: peer.transport, label: peer.name });
      setDiscoveryMessage(`Request sent to ${peer.name}. Accept it in Sessions on that machine.`);
    } catch (reason) {
      setDiscoveryMessage(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setDiscoveryBusy(false);
    }
  };

  // Shared approval poll — see hooks/useMachineAccessPairing.ts. Fleet adds
  // the machine without stealing the user's current selection.
  useMachineAccessPairing({
    pending: accessRequest,
    select: false,
    onAccepted: (server) => {
      setAccessRequest(null);
      setDiscoveryMessage(`${server.name} is now in Fleet.`);
    },
    onSettled: (_outcome, text) => {
      setAccessRequest(null);
      setDiscoveryMessage(text);
    },
    onError: setDiscoveryMessage
  });

  return (
    <div className="fleet-view" aria-label="Fleet sessions">
      <div className="fleet-view-heading">
        <div>
          <h1>Fleet</h1>
          <p>Every configured machine stays visible here, including when it is offline.</p>
        </div>
        <div className="fleet-heading-actions">
          <label className="fleet-history-toggle">
            <input type="checkbox" checked={includeExited} onChange={(event) => setIncludeExited(event.target.checked)} />
            Show history
          </label>
          <button
            type="button"
            className="btn fleet-find-machines"
            disabled={!isTauri() || discoveryBusy || accessRequest !== null}
            onClick={() => void findMachines()}
          >
            <span aria-hidden>＋</span>{discoveryBusy ? 'Searching…' : 'Find machines'}
          </button>
        </div>
      </div>
      <FleetPermissionBanner visible={localNetworkDenied} />
      {discoveryOpen ? (
        <section className="fleet-discovery" aria-live="polite">
          <header>
            <div>
              <span>Private device discovery</span>
              <h2>Machines you can connect to</h2>
              <p>Sessions checks both Tailscale and your nearby network, then shows only verified Sessions runtimes.</p>
            </div>
            <div className="fleet-discovery-actions">
              <button type="button" className="btn btn-ghost" disabled={discoveryBusy || accessRequest !== null} onClick={() => void findMachines()}>Search again</button>
              <button type="button" className="fleet-discovery-close" aria-label="Close machine discovery" onClick={() => setDiscoveryOpen(false)}>×</button>
            </div>
          </header>
          <div className="fleet-discovery-scopes">
            <span><strong>Tailnet</strong> Encrypted · recommended</span>
            <span><strong>Nearby</strong> Bonjour · trusted networks only</span>
          </div>
          {discoveredPeers !== null ? (
            discoveredPeers.length > 0 ? (
              <div className="fleet-discovery-results">
                {discoveredPeers.map((peer) => {
                  const configured = servers.some((server) => serverMatchesPeer(server, peer.endpoint));
                  const waiting = accessRequest?.request.endpoint === peer.endpoint;
                  const peerPlatform = platformFromReportedOS(peer.os) ?? 'server';
                  return (
                    <article key={peer.endpoint}>
                      <span className={`fleet-platform-mark is-${peerPlatform}`} aria-hidden><MachinePlatformIcon platform={peerPlatform} /></span>
                      <div>
                        <strong>{peer.name}</strong>
                        <small>{peer.transport === 'tailnet' ? 'Tailscale · encrypted' : 'Nearby · unencrypted'} · {peer.endpoint.replace(/^https?:\/\//, '')}</small>
                      </div>
                      <button type="button" className={configured || waiting ? 'btn btn-ghost' : 'btn'} disabled={configured || discoveryBusy || accessRequest !== null} onClick={() => void requestAccess(peer)}>
                        {configured ? 'Already in Fleet' : waiting ? 'Waiting for approval…' : 'Request access'}
                      </button>
                    </article>
                  );
                })}
              </div>
            ) : !discoveryBusy ? (
              <div className="fleet-discovery-empty">No other Sessions machines answered. Enable Tailscale remote access or trusted-network LAN access on the host, then search again.</div>
            ) : null
          ) : null}
          {discoveryMessage ? <div className="fleet-discovery-message">{discoveryMessage}</div> : null}
        </section>
      ) : null}
      <div className="fleet-section-label"><span>Your machines</span><strong>{servers.length} configured</strong></div>
      <div className="fleet-machine-grid">
        {servers.map((server) => (
          <FleetServerGroup
            key={server.id}
            server={server}
            includeExited={includeExited}
            localVersion={localVersion}
            onVersion={rememberVersion}
            onOpenSession={(sessionId) => onOpenSession(server.id, sessionId)}
            onOpenMachine={() => onOpenMachine(server.id)}
          />
        ))}
      </div>
    </div>
  );
}

function useRememberMachineVersion(setVersions: Dispatch<SetStateAction<Record<string, string>>>) {
  return useCallback((serverId: string, version: string): void => {
    if (!version) return;
    setVersions((current) => current[serverId] === version ? current : { ...current, [serverId]: version });
  }, [setVersions]);
}

function useLocalNetworkDenied(): boolean {
  const [denied, setDenied] = useState(false);
  useEffect(() => {
    const controller = new AbortController();
    void fetchLANState(controller.signal)
      .then((state) => setDenied(state.permission?.status === 'denied'))
      .catch(() => {});
    return () => controller.abort();
  }, []);
  return denied;
}

function FleetPermissionBanner({ visible }: { visible: boolean }): JSX.Element | null {
  if (!visible) return null;
  return <div className="fleet-permission-banner" role="alert">
    macOS has not allowed Sessions to use the local network. System Settings › Privacy &amp; Security › Local Network › turn on Sessions.
  </div>;
}

function mergeDiscoveredPeers(peers: DiscoveredPeer[]): DiscoveredPeer[] {
  const byIdentity = new Map<string, DiscoveredPeer>();
  for (const peer of peers) {
    const identity = (peer.hostname || peer.name).trim().toLowerCase();
    const existing = byIdentity.get(identity);
    if (!existing || (existing.transport === 'nearby' && peer.transport === 'tailnet')) {
      byIdentity.set(identity, peer);
    }
  }
  return [...byIdentity.values()].sort((left, right) => left.name.localeCompare(right.name));
}

function serverMatchesPeer(server: ServerConfig, endpoint: string): boolean {
  try {
    return new URL(endpoint).hostname.toLowerCase() === server.host.replace(/^\[|\]$/g, '').toLowerCase();
  } catch {
    return false;
  }
}

function FleetServerGroup({
  server,
  includeExited,
  localVersion,
  onVersion,
  onOpenSession,
  onOpenMachine
}: {
  server: ServerConfig;
  includeExited: boolean;
  localVersion?: string;
  onVersion: (serverId: string, version: string) => void;
  onOpenSession: (sessionId: string) => void;
  onOpenMachine: () => void;
}): JSX.Element {
  const updateServer = useServers((state) => state.updateServer);
  const localServer = useServers((state) => state.servers.find((candidate) => candidate.isDefault));
  const [snapshot, setSnapshot] = useState<ServerSnapshot>(INITIAL_SNAPSHOT);
  const [renaming, setRenaming] = useState(false);
  const [machineName, setMachineName] = useState(server.customName ?? serverDisplayName(server));
  const [renameError, setRenameError] = useState<string | null>(null);

  // Depending on the resolved string rather than on three raw fields is what
  // makes this effect's dependency list honest: `serverDisplayName` reads
  // customName, systemName, name AND isDefault, so the old list was both
  // incomplete and unable to satisfy the exhaustive-deps rule.
  const resolvedMachineName = server.customName ?? serverDisplayName(server);
  useEffect(() => {
    if (!renaming) setMachineName(resolvedMachineName);
  }, [renaming, resolvedMachineName]);

  // The poll is keyed on the address it actually dials, not on the server
  // object. `server` gets a new identity whenever ANY field changes, so with
  // `[onVersion, server]` renaming a machine tore the poll down, reset the
  // card to INITIAL_SNAPSHOT ("checking", no sessions, no version) and made
  // the user watch a full round-trip come back — for an edit that never
  // touched the endpoint. Only a change to where or how we connect should
  // restart it and blank the card; everything else keeps the live snapshot.
  const serverRef = useRef(server);
  serverRef.current = server;
  const endpointKey = [
    server.id,
    server.scheme ?? 'http',
    server.host,
    server.port,
    server.token ?? ''
  ].join('|');

  useEffect(() => {
    let stopped = false;
    let pollTimer: number | undefined;
    let controller: AbortController | null = null;

    setSnapshot(INITIAL_SNAPSHOT);

    const poll = async (): Promise<void> => {
      controller = new AbortController();
      const timeout = window.setTimeout(() => controller?.abort(), POLL_TIMEOUT_MS);

      // Read the latest config at request time so a rename or metadata edit
      // is picked up by the next tick without restarting the poll.
      const target = serverRef.current;

      try {
        const health = await fetchServerHealth(target, controller.signal);
        if (!stopped) {
          onVersion(target.id, health.version);
          setSnapshot((current) => ({ ...current, health }));
        }
      } catch {
        if (!stopped) {
          setSnapshot((current) => ({
            ...current,
            reachability: 'unreachable',
            sessionsError: null
          }));
        }
        window.clearTimeout(timeout);
        if (!stopped) pollTimer = window.setTimeout(() => { void poll(); }, POLL_INTERVAL_MS);
        return;
      }

      if (!stopped) {
        setSnapshot((current) => ({
          ...current,
          reachability: 'reachable',
          sessionsError: null
        }));
      }

      try {
        const [sessions, profiles] = await Promise.all([
          listServerSessions(target, controller.signal),
          listServerProfiles(target, controller.signal).catch(() => [])
        ]);
        if (!stopped) {
          setSnapshot((current) => ({ ...current, reachability: 'reachable', sessions, profiles, sessionsError: null }));
        }
      } catch (error) {
        if (!stopped) {
          setSnapshot((current) => ({
            ...current,
            reachability: 'reachable',
            sessionsError: error instanceof Error ? error.message : 'session list unavailable'
          }));
        }
      } finally {
        window.clearTimeout(timeout);
        if (!stopped) pollTimer = window.setTimeout(() => { void poll(); }, POLL_INTERVAL_MS);
      }
    };

    void poll();
    return () => {
      stopped = true;
      controller?.abort();
      window.clearTimeout(pollTimer);
    };
  }, [endpointKey, onVersion]);

  const unavailable = snapshot.reachability === 'unreachable';
  const candidateSessions = snapshot.sessions.filter((session) => includeExited || !session.exited);
  const visibleSessions = sortFleetSessions(candidateSessions);
  const activeCount = snapshot.sessions.filter((session) => !session.exited).length;
  const reachabilityLabel = snapshot.reachability === 'reachable'
    ? 'reachable'
    : snapshot.reachability === 'unreachable'
    ? 'unreachable'
    : 'checking';
  const profileSummary = snapshot.profiles.reduce<Record<'claude' | 'codex', string[]>>(
    (summary, profile) => {
      summary[profile.tool].push(profile.name);
      return summary;
    },
    { claude: [], codex: [] }
  );
  const profileLabels = [
    profileSummary.claude.length > 0 ? `Claude: ${profileSummary.claude.join(', ')}` : '',
    profileSummary.codex.length > 0 ? `Codex: ${profileSummary.codex.join(', ')}` : ''
  ].filter(Boolean);
  const platform = platformFor(server, snapshot.health);
  const platformText = platformLabel(platform);
  const fullVersion = snapshot.health?.version;
  const version = shortVersion(fullVersion);
  const versionState = machineVersionState(
    snapshot.health?.version,
    localVersion,
    server.isDefault,
    serverDisplayName(server),
    localServer ? serverDisplayName(localServer) : 'the local computer'
  );
  const cardClasses = [
    'fleet-server-group',
    server.isDefault ? 'is-local' : '',
    unavailable ? 'is-unreachable' : ''
  ].filter(Boolean).join(' ');
  const displayMachineName = serverDisplayName(server, true);
  const saveMachineName = async (): Promise<void> => {
    const name = machineName.trim().replace(/\s+/g, ' ').slice(0, 48);
    if (!name) {
      setRenameError('Add a name for this machine.');
      return;
    }
    setRenameError(null);
    try {
      await updateServer(server.id, { name, customName: name });
      setRenaming(false);
    } catch (reason) {
      setRenameError(reason instanceof Error ? reason.message : 'Could not save this machine name.');
    }
  };

  return (
    <section className={cardClasses}>
      <header className="fleet-machine-header">
        <span className={`fleet-platform-mark is-${platform}`} aria-hidden><MachinePlatformIcon platform={platform} /></span>
        <div className="fleet-server-identity">
          <div className="fleet-machine-title">
            {renaming ? (
              <form className="fleet-machine-rename" onSubmit={(event) => { event.preventDefault(); void saveMachineName(); }}>
                <input autoFocus value={machineName} maxLength={48} aria-label="Machine name" onChange={(event) => setMachineName(event.target.value)} />
                <button type="submit">Save</button>
                <button type="button" onClick={() => { setRenaming(false); setRenameError(null); setMachineName(server.customName ?? serverDisplayName(server)); }}>Cancel</button>
              </form>
            ) : (
              <>
                <h2 title={`${displayMachineName} · ${formatServerEndpoint(server)}`}>{displayMachineName}</h2>
                <button type="button" className="fleet-machine-rename-button" aria-label={`Rename ${displayMachineName}`} title="Name this machine" onClick={() => setRenaming(true)}>
                  <svg viewBox="0 0 24 24" aria-hidden><path d="M4 16.5V20h3.5L18 9.5 14.5 6 4 16.5Zm16.7-9.8a1 1 0 0 0 0-1.4l-2-2a1 1 0 0 0-1.4 0l-1.6 1.6 3.5 3.5 1.5-1.7Z" /></svg>
                </button>
              </>
            )}
          </div>
          {renameError ? <span className="fleet-machine-rename-error">{renameError}</span> : null}
          <span className={`fleet-machine-status is-${snapshot.reachability}`}><span className={`fleet-reachability-dot is-${snapshot.reachability}`} aria-hidden />{reachabilityLabel}</span>
        </div>
        <span className="fleet-machine-count"><strong>{activeCount} live</strong><span>{snapshot.sessions.length} total</span></span>
      </header>
      <div className="fleet-machine-meta" title={`Connected at ${formatServerEndpoint(server)}${fullVersion ? ` · Sessions ${fullVersion}` : ''}`}>
        <span>{platformText}{server.transport ? ` · ${server.transport === 'lan' ? 'LAN' : server.transport === 'tailnet' ? 'Tailscale HTTPS' : 'Tailscale IP'}` : ''}</span>
        {snapshot.health?.system?.arch ? <span>{snapshot.health.system.arch}</span> : null}
        <span className="is-version">{version ? `Sessions ${version}` : 'Version unavailable'}</span>
        {profileLabels.length > 0 ? <span title={profileLabels.join(' · ')}>{snapshot.profiles.length} {snapshot.profiles.length === 1 ? 'account' : 'accounts'}</span> : null}
      </div>
      {versionState ? (
        <div className={`fleet-version-notice is-${versionState.tone}`} title={versionState.fullDetail}>
          <strong>{versionState.title}</strong>
          <span>{versionState.detail}</span>
        </div>
      ) : null}

      <div className="fleet-session-list">
        {visibleSessions.map((session) => (
          <FleetSessionRow
            key={session.id}
            session={session}
            disabled={unavailable || session.exited}
            onOpen={() => onOpenSession(session.id)}
          />
        ))}
        {visibleSessions.length === 0 ? (
          <div className="fleet-session-empty">
            {snapshot.reachability === 'checking'
              ? 'Checking machine…'
              : unavailable
              ? 'Session data unavailable'
              : snapshot.sessionsError
              ? snapshot.sessionsError
              : snapshot.sessions.length > 0
              ? 'No active sessions — enable Show history to see retained work'
              : 'No sessions'}
          </div>
        ) : null}
        {snapshot.sessions.length > 0 && snapshot.sessionsError ? (
          <div className="fleet-session-error">Latest session refresh failed: {snapshot.sessionsError}</div>
        ) : null}
      </div>
      {!unavailable ? (
        <button type="button" className="fleet-open-machine" onClick={onOpenMachine}>
          Open all sessions on {displayMachineName} <span aria-hidden>→</span>
        </button>
      ) : null}
    </section>
  );
}

function machineVersionState(
  machineVersion: string | undefined,
  localVersion: string | undefined,
  isLocal: boolean,
  machineName: string,
  localMachineName: string
): { tone: 'older' | 'newer' | 'different'; title: string; detail: string; fullDetail: string } | null {
  if (isLocal || !machineVersion || !localVersion || machineVersion === localVersion) return null;
  const comparison = compareReleaseVersions(machineVersion, localVersion);
  if (comparison === -1) {
    return {
      tone: 'older',
      title: 'Update available',
      detail: `${shortVersion(machineVersion)} → ${shortVersion(localVersion)} · Compatible`,
      fullDetail: `${machineName} runs ${machineVersion}; ${localMachineName} runs ${localVersion}. Their API ranges are compatible, so it can update when convenient.`
    };
  }
  if (comparison === 1) {
    return {
      tone: 'newer',
      title: 'Newer version',
      detail: `${shortVersion(machineVersion)} here · ${shortVersion(localVersion)} locally`,
      fullDetail: `${machineName} runs ${machineVersion}; ${localMachineName} runs ${localVersion}. Their API ranges are compatible.`
    };
  }
  return {
    tone: 'different',
    title: 'Different build',
    detail: `${shortVersion(machineVersion)} here · Compatible`,
    fullDetail: `${machineName} runs ${machineVersion}; ${localMachineName} runs ${localVersion}. Their API ranges are compatible.`
  };
}

function compareReleaseVersions(left: string, right: string): -1 | 0 | 1 | null {
  const parse = (value: string): [number, number, number] | null => {
    const match = value.trim().match(/^v?(\d+)\.(\d+)\.(\d+)/);
    return match ? [Number(match[1]), Number(match[2]), Number(match[3])] : null;
  };
  const a = parse(left);
  const b = parse(right);
  if (!a || !b) return null;
  for (let index = 0; index < a.length; index += 1) {
    if (a[index] < b[index]) return -1;
    if (a[index] > b[index]) return 1;
  }
  return 0;
}

type Platform = 'macos' | 'windows' | 'linux' | 'server';

function platformFromReportedOS(value: string | undefined): Platform | null {
  const reported = value?.toLowerCase() ?? '';
  if (reported === 'darwin' || reported.includes('mac')) return 'macos';
  if (reported.includes('windows') || reported === 'win32') return 'windows';
  if (reported.includes('linux')) return 'linux';
  return null;
}

function platformFor(server: ServerConfig, health: ServerHealth | null): Platform {
  const reported = platformFromReportedOS(health?.system?.os);
  if (reported) return reported;

  const hint = `${server.name} ${server.host}`.toLowerCase();
  if (server.isDefault && /mac|darwin/.test(navigator.userAgent.toLowerCase())) return 'macos';
  if (/mac|darwin/.test(hint)) return 'macos';
  if (/windows|win\b/.test(hint)) return 'windows';
  if (/linux|ubuntu|debian|fedora/.test(hint)) return 'linux';
  return 'server';
}

function platformLabel(platform: Platform): string {
  return platform === 'macos' ? 'macOS' : platform === 'windows' ? 'Windows' : platform === 'linux' ? 'Linux' : 'Sessions host';
}

function shortVersion(version: string | undefined): string {
  if (!version) return '';
  const match = version.trim().match(/^v?(\d+\.\d+\.\d+)/);
  return match ? match[1] : version;
}

function FleetSessionRow({
  session,
  disabled,
  onOpen
}: {
  session: SessionInfo;
  disabled: boolean;
  onOpen: () => void;
}): JSX.Element {
  const status = classifySession(session);
  const stateLabel = status.label;
  const label = shortLabel(session);
  const context = fleetSessionContext(session);
  const details = [label, session.cwd, session.profile].filter(Boolean).join(' · ');

  return (
    <button
      type="button"
      className="fleet-session-row"
      disabled={disabled}
      onClick={onOpen}
      aria-label={session.exited ? `${label}, retained history, ${stateLabel}` : `Open ${label}, ${stateLabel}`}
      title={details}
    >
      <span className="fleet-session-icon" aria-hidden>
        <ParserIcon icon={TOOL_ICONS[session.tool]} size={20} />
      </span>
      <span className="fleet-session-main">
        <span className="fleet-session-name">{label}</span>
        <span className="fleet-session-context">{context}</span>
      </span>
      <span className={`fleet-session-state ${status.className}`}>
        <span className="fleet-session-state-dot" aria-hidden />
        {stateLabel}
      </span>
    </button>
  );
}

// Fleet orders by urgency rather than by the classifier's precedence: an
// ended runtime is not urgent, but a failed one is. The classification itself
// is not re-derived here — only its display order.
const FLEET_SORT_PRIORITY: Record<SessionStatusState, number> = {
  failed: 0,
  'provider-down': 0,
  'auth-needed': 0,
  'needs-recovery': 1,
  'needs-you': 2,
  reconnecting: 3,
  limited: 4,
  working: 5,
  ready: 6,
  unavailable: 7,
  'not-started': 8,
  finished: 9,
  ended: 10
};

function sortFleetSessions(sessions: SessionInfo[]): SessionInfo[] {
  return [...sessions].sort((left, right) => {
    const stateDifference = FLEET_SORT_PRIORITY[classifySession(left).state]
      - FLEET_SORT_PRIORITY[classifySession(right).state];
    if (stateDifference !== 0) return stateDifference;
    return fleetSessionActivity(right) - fleetSessionActivity(left);
  });
}

function fleetSessionActivity(session: SessionInfo): number {
  return session.exitedAt ?? session.lastDataAt ?? session.createdAt;
}

function fleetSessionContext(session: SessionInfo): string {
  const normalized = (session.worktreePath || session.cwd).replace(/\/+$/, '');
  const workspace = normalized.split('/').filter(Boolean).pop();
  if (workspace) return workspace;
  return session.tool === 'terminal' ? 'Shell session' : session.tool === 'codex' ? 'Codex session' : 'Claude session';
}
