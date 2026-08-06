import { useCallback, useEffect, useRef, useState } from 'react';
import { fetchServerHealth, listServerProfiles, listServerSessions, type AccountProfile, type ServerHealth } from '../api/sessionsd';
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
  const localServer = servers.find((server) => server.isDefault) ?? servers[0];
  const localVersion = localServer ? machineVersions[localServer.id] : undefined;

  const rememberVersion = useCallback((serverId: string, version: string): void => {
    if (!version) return;
    setMachineVersions((current) => current[serverId] === version
      ? current
      : { ...current, [serverId]: version });
  }, []);

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
                      <span className={`fleet-platform-mark is-${peerPlatform}`} aria-hidden><PlatformIcon platform={peerPlatform} /></span>
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
        <CloudFleetCard />
      </div>
    </div>
  );
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

function CloudFleetCard(): JSX.Element {
  return (
    <section className="fleet-server-group fleet-cloud-machine is-placeholder" aria-label="Somewhere cloud workspace coming soon">
      <header className="fleet-machine-header">
        <span className="fleet-platform-mark is-cloud" aria-hidden><PlatformIcon platform="cloud" /></span>
        <div className="fleet-server-identity">
          <div className="fleet-machine-title"><h2>Somewhere VM</h2><span className="fleet-machine-badge">Coming soon</span></div>
          <span className="fleet-machine-status"><span className="fleet-reachability-dot" aria-hidden />Not configured</span>
        </div>
      </header>
      <div className="fleet-machine-meta"><span>Cloud workspace</span><span>Outbound-only worker</span></div>
      <div className="fleet-cloud-machine-body">
        <p>An always-on private computer for your sessions, with provider logins isolated inside its own workspace.</p>
        <div><span>Cloud usage</span><span>Encrypted backup</span><span>Scoped files</span></div>
        <button type="button" className="btn" disabled>Set up · coming soon</button>
      </div>
    </section>
  );
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
        <span className={`fleet-platform-mark is-${platform}`} aria-hidden><PlatformIcon platform={platform} /></span>
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
        <span>{platformText}</span>
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

type Platform = 'macos' | 'windows' | 'linux' | 'cloud' | 'server';

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

function PlatformIcon({ platform }: { platform: Platform }): JSX.Element {
  if (platform === 'macos') {
    return <svg viewBox="0 0 384 512" role="img" aria-label="macOS"><path d="M279.6 258.9c-.2-36.7 16.4-64.4 50-84.8-18.8-26.9-47.2-41.7-84.7-44.6-35.5-2.8-74.3 20.7-88.5 20.7-15 0-49.4-19.7-72.6-19.7C34.4 131.2 0 170.9 0 252.9c0 24.3 4.4 49.4 13.3 75.8 11.9 34.7 54.7 119.8 99.4 118.4 23.4-.6 40-16.6 70.5-16.6 29.6 0 45 16.6 71.1 16.6 45.1-.6 83.8-78.2 95.1-112.9-60.4-28.5-57.3-73.7-69.8-75.3ZM256.4 94.7c27.3-32.4 24.8-61.9 24-72.5-24.1 1.4-52 16.4-67.9 34.9-17.5 19.8-27.8 44.3-25.6 71.9 26.1 2 49.9-11.4 69.5-34.3Z" /></svg>;
  }
  if (platform === 'windows') {
    return <svg viewBox="0 0 24 24" role="img" aria-label="Windows"><path d="m3 4.6 7.5-1v7.8H3V4.6Zm8.6-1.2L21 2v9.4h-9.4v-8ZM3 12.5h7.5v7.8l-7.5-1v-6.8Zm8.6 0H21V22l-9.4-1.4v-8.1Z" /></svg>;
  }
  if (platform === 'linux') {
    return <svg viewBox="0 0 24 24" role="img" aria-label="Linux"><path d="M12 2c-3.1 0-5 2.8-5 6.8 0 1.4-.5 2.8-1.4 4.2-1.3 2-1.3 4.3-.2 5.8.8 1 2 1.1 3.3.4.9.6 2 1 3.3 1s2.4-.4 3.3-1c1.3.7 2.5.6 3.3-.4 1.2-1.5 1.1-3.8-.2-5.8-.9-1.4-1.4-2.8-1.4-4.2C17 4.8 15.1 2 12 2Zm-2 5.2c-.6 0-1-.6-1-1.3s.4-1.3 1-1.3 1 .6 1 1.3-.4 1.3-1 1.3Zm4 0c-.6 0-1-.6-1-1.3s.4-1.3 1-1.3 1 .6 1 1.3-.4 1.3-1 1.3Zm-2 4.2-2.2-1.6L12 8.6l2.2 1.2L12 11.4Z" /></svg>;
  }
  if (platform === 'cloud') {
    return <svg viewBox="0 0 24 24" role="img" aria-label="Cloud"><path d="M7 19h10a5 5 0 0 0 .8-9.9A6.5 6.5 0 0 0 5.4 7.7 5.7 5.7 0 0 0 7 19Z" fill="none" stroke="currentColor" strokeWidth="1.6" /></svg>;
  }
  return <svg viewBox="0 0 24 24" role="img" aria-label="Server"><path d="M4 4h16v6H4V4Zm0 10h16v6H4v-6Z" fill="none" stroke="currentColor" strokeWidth="1.5" /><path d="M7 7h.01M7 17h.01" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" /></svg>;
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
  'needs-you': 1,
  limited: 2,
  working: 3,
  ready: 4,
  'not-started': 5,
  finished: 6,
  ended: 7
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
