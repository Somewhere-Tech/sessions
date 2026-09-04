import { useCallback, useEffect, useMemo, useState, type Dispatch, type ReactNode, type SetStateAction } from 'react';
import { fetchLANState, fetchRemoteState, forgetPairedDevice, httpBaseForServer, listPairedDevices, requestLocalNetworkAccess, revokePairingTicket, setLANEnabled, setRemoteAuto, type LANState, type PairedDevice, type RemoteState } from '../api/sessionsd';
import {
  discoverNativeMachines,
  getNativeConnectionSettings,
  isTauri,
  requestNativeMachineAccess,
  runNativeConnectionAction,
  setNativeRuntimePort,
  type NativeConnectionSettings,
  type NativeMachinePeer,
  type NativeTailnetRequest
} from '../lib/tauriBridge';
import { configureNativeLocalPort, serverDisplayName, useServers } from '../lib/servers';
import { claimNativeMachinePairing } from '../lib/hostedBootstrap';
import { tailnetClientID } from '../lib/tailnetClient';
import { useMachineAccessPairing } from '../hooks/useMachineAccessPairing';
import { SomewhereCard } from './SomewhereCard';
import { FleetAccountCard } from './FleetAccountCard';
import { ServerSelector } from './ServerSelector';
import { RelayConnectionCard } from './RelayConnectionCard';

interface PairState {
  url: string;
  link: string;
  fallback: string;
  qr_data_url: string;
  ticket_id: string;
  ticket: string;
  expires_at: string;
  endpoints: Array<{ endpoint: string; transport: string }>;
}

async function updateRemoteAuto(
  enabled: boolean,
  setRemote: Dispatch<SetStateAction<RemoteState | null>>,
  setMessage: Dispatch<SetStateAction<string | null>>,
  setBusy: Dispatch<SetStateAction<string | null>>
): Promise<void> {
  setBusy('remote-auto'); setMessage(null);
  try {
    setRemote(await setRemoteAuto(enabled));
  } catch (reason) {
    const detail = reason instanceof Error ? reason.message : String(reason);
    setMessage(detail);
  } finally {
    setBusy(null);
  }
}

export function ConnectionsView({ clientOnly = false, hostName }: { clientOnly?: boolean; hostName?: string }): JSX.Element {
  const activeServer = useServers((state) => state.servers.find((server) => server.id === state.activeId));
  const machineName = hostName || (activeServer ? serverDisplayName(activeServer, true) : 'this computer');
  const connectedViaTailnet = activeServer?.scheme === 'https' && activeServer.host.toLowerCase().endsWith('.ts.net');
  const activeEndpoint = activeServer ? httpBaseForServer(activeServer) : window.location.origin;
  const [native, setNative] = useState<NativeConnectionSettings | null>(null);
  const [port, setPort] = useState('8787');
  const [lan, setLAN] = useState<LANState | null>(null);
  const [remote, setRemote] = useState<RemoteState | null>(null);
  const [pair, setPair] = useState<PairState | null>(null);
  const [pairedDevices, setPairedDevices] = useState<PairedDevice[]>([]);
  const [pairName, setPairName] = useState('My other device');
  const [incomingPairLink, setIncomingPairLink] = useState('');
  const [incomingPairMessage, setIncomingPairMessage] = useState<string | null>(null);
  const [tailnetPeers, setTailnetPeers] = useState<NativeMachinePeer[] | null>(null);
  const [tailnetRequest, setTailnetRequest] = useState<(NativeTailnetRequest & { transport: NativeMachinePeer['transport'] }) | null>(null);
  const [tailnetMessage, setTailnetMessage] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [message, setMessage] = useState<string | null>(null);
  const refresh = useCallback(async (): Promise<void> => {
    setMessage(null);
    const lanRequest = isTauri() && !clientOnly
      ? runNativeConnectionAction<LANState>('lan', 'status').then((result) => result.data)
      : fetchLANState();
    const [nativeResult, lanResult, devicesResult] = await Promise.all([
      getNativeConnectionSettings().catch(() => null),
      lanRequest.catch(() => null),
      clientOnly ? Promise.resolve([]) : listPairedDevices().catch(() => [])
    ]);
    setNative(nativeResult);
    if (nativeResult) setPort(String(nativeResult.port));
    setLAN(lanResult);
    setPairedDevices(devicesResult);
    if (clientOnly) {
      setRemote(null);
      return;
    }
    try {
      setRemote(await fetchRemoteState());
    } catch {
      setRemote(null);
    }
  }, [clientOnly]);

  useEffect(() => { void refresh(); }, [refresh]);

  useEffect(() => {
    if (clientOnly || !pair) return undefined;
    const poll = window.setInterval(() => {
      void listPairedDevices().then(setPairedDevices).catch(() => { /* Refresh remains available. */ });
    }, 2_000);
    return () => window.clearInterval(poll);
  }, [clientOnly, pair]);

  // Shared approval poll — see hooks/useMachineAccessPairing.ts. The
  // Mac-specific denial/expiry wording that used to live here was one of the
  // three drifted copies; the shared, machine-neutral sentence replaces it.
  const pendingAccess = useMemo(
    () => tailnetRequest ? { transport: tailnetRequest.transport, request: tailnetRequest } : null,
    [tailnetRequest]
  );
  useMachineAccessPairing({
    pending: pendingAccess,
    onAccepted: (server) => {
      setTailnetRequest(null);
      setTailnetMessage(`Connected to ${server.name}. Sessions is switching to it now.`);
    },
    onSettled: (_outcome, text) => {
      setTailnetRequest(null);
      setTailnetMessage(text);
    },
    onError: setTailnetMessage
  });

  const changeLAN = async (enabled: boolean): Promise<void> => {
    if (clientOnly || busy) return;
    setBusy('lan'); setMessage(null);
    try {
      if (isTauri()) {
        const result = await runNativeConnectionAction<LANState & { verified?: boolean }>('lan', enabled ? 'enable' : 'disable');
        setLAN(result.data);
      } else {
        setLAN(await setLANEnabled(enabled));
      }
    } catch (reason) {
      setMessage(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setBusy(null);
    }
  };

  const createPair = async (): Promise<void> => {
    if (clientOnly || busy || !isTauri()) return;
    setBusy('pair'); setMessage(null);
    try {
      const result = await runNativeConnectionAction<PairState>('pair', 'create', pairName);
      setPair(result.data);
    } catch (reason) {
      setMessage(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setBusy(null);
    }
  };

  const revokePair = async (): Promise<void> => {
    if (!pair || busy) return;
    setBusy('revoke-pair'); setMessage(null);
    try {
      await revokePairingTicket(pair.ticket_id);
      setPair(null);
      setMessage('Pairing link revoked.');
    } catch (reason) {
      setMessage(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setBusy(null);
    }
  };

  const forgetDevice = async (device: PairedDevice): Promise<void> => {
    if (busy) return;
    setBusy(`forget:${device.device_id}`); setMessage(null);
    try {
      await forgetPairedDevice(device.device_id);
      setPairedDevices((current) => current.filter((item) => item.device_id !== device.device_id));
      setMessage(`Forgot ${device.name}.`);
    } catch (reason) {
      setMessage(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setBusy(null);
    }
  };

  const addMachine = async (): Promise<void> => {
    if (busy || !isTauri() || !incomingPairLink.trim()) return;
    setBusy('incoming-pair'); setIncomingPairMessage(null);
    try {
      const { claim, server } = await claimNativeMachinePairing(incomingPairLink);
      setIncomingPairLink('');
      setIncomingPairMessage(`Paired with ${server.name} as ${claim.name}. Sessions is switching to it now.`);
    } catch (reason) {
      setIncomingPairMessage(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setBusy(null);
    }
  };

  const discoverTailnet = async (): Promise<void> => {
    if (busy || !isTauri()) return;
    setBusy('discover'); setTailnetMessage(null);
    try {
      const result = await discoverNativeMachines();
      setTailnetPeers(result.peers);
      if (result.peers.length === 0 && result.errors.length > 0) setTailnetMessage(result.errors.join(' · '));
    } catch (reason) {
      setTailnetPeers([]);
      setTailnetMessage(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setBusy(null);
    }
  };

  const requestAccess = async (peer: NativeMachinePeer): Promise<void> => {
    if (busy || tailnetRequest || !isTauri()) return;
    setBusy(`request:${peer.endpoint}`); setTailnetMessage(null);
    try {
      const requested = await requestNativeMachineAccess(peer, tailnetClientID(), '');
      setTailnetRequest({ ...requested, transport: peer.transport });
      setTailnetMessage(`Request sent to ${peer.name}. Accept it in Sessions on that machine.`);
    } catch (reason) {
      setTailnetMessage(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setBusy(null);
    }
  };

  const savePort = async (): Promise<void> => {
    if (clientOnly || busy || !native) return;
    const requested = Number(port);
    if (!Number.isInteger(requested) || requested < 1024 || requested > 65535) {
      setMessage('Choose a port between 1024 and 65535.');
      return;
    }
    if (requested === native.port) return;
    setBusy('port'); setMessage('Moving the background service safely…');
    try {
      const next = await setNativeRuntimePort(requested);
      setNative(next);
      configureNativeLocalPort(next.port);
      setMessage(`Sessions moved to localhost:${next.port}. Reconnecting the app…`);
      window.setTimeout(() => window.location.reload(), 650);
    } catch (reason) {
      setPort(String(native.port));
      setMessage(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setBusy(null);
    }
  };

  return (
    <div className="connections-view">
      <div className="connections-shell">
        <header className="connections-heading">
          <div><span>Private by default</span><h1>Connections</h1><p>{clientOnly ? `This device views sessions hosted by ${machineName}. Direct routes stay preferred.` : 'This computer is the server. Direct routes stay preferred.'}</p></div>
          <button type="button" className="btn btn-ghost" disabled={busy !== null} onClick={() => void refresh()}>Refresh status</button>
        </header>

        {message ? <div className="connections-message" role="status">{message}</div> : null}

        <FleetAccountCard clientOnly={clientOnly} />

        <section className="connection-ladder" aria-label="Connection options">
          <ConnectionCard step="01" title={clientOnly ? machineName : 'This computer'} state={clientOnly ? 'Connected host' : 'Always private'} active>
            <p>{clientOnly ? `Sessions on this device uses the independent daemon on ${machineName}. Closing this viewer never stops its sessions.` : 'Sessions.app talks to the independent loopback daemon. Quitting the app never stops its sessions.'}</p>
            <div className="connection-endpoint">Current connection · {activeEndpoint}</div>
            {native ? <div className="connection-endpoint">Sessions.app configured endpoint · http://localhost:{native.port}</div> : null}
            {!clientOnly && native ? <details className="connection-advanced">
              <summary>Advanced port</summary>
              <p>Changing this restarts only the daemon and rolls back on failure. Windows currently changes ports only while no sessions are live; Mac verifies every live runner is re-adopted.</p>
              <div className="connection-inline">
                <input type="number" min={1024} max={65535} value={port} onChange={(event) => setPort(event.currentTarget.value)} disabled={!native || native.runtime.state !== 'ready' || busy !== null} />
                <button type="button" className="btn" disabled={!native || native.runtime.state !== 'ready' || busy !== null || Number(port) === native.port} onClick={() => void savePort()}>{busy === 'port' ? 'Moving…' : 'Change port'}</button>
              </div>
              {native?.runtime.state === 'development' ? <small>Installed-app only. Development builds use the separately managed dev daemon.</small> : null}
            </details> : <HostConnectionChoice hostName={machineName} />}
          </ConnectionCard>

          <ConnectionCard step="02" title="Same Wi-Fi" state={lan?.enabled ? 'On' : 'Off'} active={lan?.enabled === true}>
            <p>Connect another native Sessions client on a private network you trust. Bonjour discovery starts with LAN access; browser terminal access is intentionally not a product surface.</p><LocalNetworkPermissionPrompt visible={!clientOnly && lan?.permission?.status !== 'granted'} disabled={busy !== null} onMessage={setMessage} onState={setLAN} />
            {lan?.url ? <div className="connection-endpoint">{lan.url}</div> : null}
            {lan?.enabled ? (
              <div className="connection-privacy-note">
                <strong>{lan.bonjour?.advertised ? 'Visible to nearby Sessions apps' : 'Nearby discovery unavailable'}</strong>
                <span>{lan.bonjour?.error ?? 'LAN traffic is authenticated but unencrypted. Prefer Tailscale on shared or public Wi-Fi.'}</span>
              </div>
            ) : null}
            {clientOnly ? <HostConnectionChoice hostName={machineName} /> : null}
            <button type="button" className={`btn${lan?.enabled ? ' btn-ghost' : ''}`} disabled={clientOnly || busy !== null} onClick={() => void changeLAN(!lan?.enabled)}>{busy === 'lan' ? 'Checking…' : lan?.enabled ? 'Turn off LAN' : 'Enable LAN access'}</button>
          </ConnectionCard>

          <ConnectionCard step="03" title="Anywhere" state={clientOnly ? (connectedViaTailnet ? 'Tailscale on' : `Check on ${machineName}`) : remote?.enabled ? 'Tailscale on' : 'Off'} active={clientOnly ? connectedViaTailnet : remote?.enabled === true}>
            <p>Tailscale Serve keeps the connection inside your tailnet with HTTPS terminating on {clientOnly ? machineName : 'this Mac'}.</p>
            {clientOnly ? <HostConnectionChoice hostName={machineName} /> : null}
            {clientOnly && !connectedViaTailnet ? <div className="connection-privacy-note"><strong>Host-only status</strong><span>This viewer can confirm LAN access, but the host does not expose its Tailscale configuration. Open Sessions on {machineName} to inspect it.</span></div> : null}
            {remote?.endpoint ? <div className="connection-endpoint">{remote.endpoint}</div> : null}
            {remote?.tailnetIpEndpoint ? <div className="connection-endpoint">{remote.tailnetIpEndpoint}</div> : null}
            {!clientOnly ? <label className="settings-select-row"><span><strong>Reachable over Tailscale automatically</strong><small>Sessions maintains HTTPS by name and HTTP on the tailnet address.</small></span><input type="checkbox" checked={remote?.auto ?? true} disabled={busy !== null} onChange={(event) => { if (!busy) void updateRemoteAuto(event.currentTarget.checked, setRemote, setMessage, setBusy); }} /></label> : null}
          </ConnectionCard>
        </section>

        {!clientOnly ? <RelayConnectionCard /> : null}

        <section className="pair-device-card">
          <div><span className="connections-section-kicker">Tailscale + nearby · no codes</span><h2>Connect to another Sessions machine</h2><p>Search encrypted Tailscale first and nearby Bonjour as a trusted-network fallback. The other machine must approve this device.</p></div>
          <div className="connection-actions">
            <button type="button" className="btn" disabled={!isTauri() || busy !== null} onClick={() => void discoverTailnet()}>{busy === 'discover' ? 'Looking…' : tailnetPeers === null ? 'Find Sessions Macs' : 'Scan again'}</button>
          </div>
          {tailnetPeers !== null ? (
            tailnetPeers.length > 0 ? (
              <div className="tailnet-peer-list">
                {tailnetPeers.map((peer) => {
                  const waiting = tailnetRequest?.endpoint === peer.endpoint;
                  return (
                    <article key={peer.endpoint} className="tailnet-peer">
                      <div className="tailnet-peer-icon" aria-hidden="true">{peer.os.toLowerCase().includes('mac') ? '⌘' : '▣'}</div>
                      <div><strong>{peer.name}</strong><span>{peer.transport === 'tailnet' ? 'Tailscale · encrypted' : 'Nearby · unencrypted'} · {peer.os || 'Sessions device'} · {peer.endpoint.replace(/^https?:\/\//, '')}</span></div>
                      <button type="button" className={waiting ? 'btn btn-ghost' : 'btn'} disabled={busy !== null || tailnetRequest !== null} onClick={() => void requestAccess(peer)}>
                        {waiting ? 'Waiting for approval…' : busy === `request:${peer.endpoint}` ? 'Sending…' : 'Request access'}
                      </button>
                    </article>
                  );
                })}
              </div>
            ) : <div className="connection-empty">No other Sessions machines answered. Enable Tailscale remote access or trusted-network LAN access on the host, then scan again.</div>
          ) : null}
          {tailnetMessage ? <div className="connections-message" role="status">{tailnetMessage}</div> : null}
        </section>

        {!clientOnly ? (
          <HostPairingCard pairName={pairName} onPairName={setPairName} pair={pair}
            pairedDevices={pairedDevices} busy={busy} incomingLink={incomingPairLink}
            incomingMessage={incomingPairMessage} onIncomingLink={setIncomingPairLink}
            onCreate={() => void createPair()} onRevoke={() => void revokePair()}
            onAdd={() => void addMachine()} onForget={(device) => void forgetDevice(device)}
            onCopy={() => pair && void navigator.clipboard.writeText(pair.link).then(() => setMessage('Pairing link copied.'))} />
        ) : (
          <section className="pair-device-card">
            <div><span className="connections-section-kicker">Saved on this device</span><h2>Paired machines</h2><p>Switch, add, or forget the Sessions hosts this device can view.</p></div>
            <ServerSelector />
            <div className="pair-device-controls">
              <input value={incomingPairLink} onChange={(event) => setIncomingPairLink(event.currentTarget.value)} onKeyDown={(event) => { if (event.key === 'Enter') void addMachine(); }} placeholder="sessions://pair?host=…&t=…" autoComplete="off" spellCheck={false} />
              <button type="button" className="btn" disabled={!isTauri() || busy !== null || !incomingPairLink.trim()} onClick={() => void addMachine()}>{busy === 'incoming-pair' ? 'Adding…' : 'Add with pairing link'}</button>
            </div>
            {incomingPairMessage ? <div className="connections-message" role="status">{incomingPairMessage}</div> : null}
          </section>
        )}

        <SomewhereCard clientOnly={clientOnly} hostName={machineName} />
      </div>
    </div>
  );
}

function HostPairingCard({ pairName, onPairName, pair, pairedDevices, busy, incomingLink, incomingMessage, onIncomingLink, onCreate, onRevoke, onAdd, onForget, onCopy }: {
  pairName: string; onPairName: (name: string) => void; pair: PairState | null;
  pairedDevices: PairedDevice[]; busy: string | null; incomingLink: string;
  incomingMessage: string | null; onIncomingLink: (link: string) => void;
  onCreate: () => void; onRevoke: () => void; onAdd: () => void;
  onForget: (device: PairedDevice) => void; onCopy: () => void;
}): JSX.Element {
  return (
    <section className="pair-device-card">
      <div><span className="connections-section-kicker">One-time consent · ten minutes</span><h2>Pair a device</h2><p>Create a single-use code containing every enabled LAN and Tailscale route. Possession of the code grants one independently revocable device credential.</p></div>
      <div className="pair-device-controls"><input value={pairName} maxLength={80} onChange={(event) => onPairName(event.currentTarget.value)} placeholder="Device name" /><button type="button" className="btn" disabled={!isTauri() || busy !== null} onClick={onCreate}>{busy === 'pair' ? 'Creating…' : 'Show pairing code'}</button></div>
      {pair ? <PairingTicketCard pair={pair} revoking={busy === 'revoke-pair'} onRevoke={onRevoke} onCopy={onCopy} /> : null}
      <div className="paired-device-list"><h3>Paired devices</h3>
        {pairedDevices.length === 0 ? <small>No devices have paired yet.</small> : pairedDevices.map((device) => (
          <div key={device.device_id} className="paired-device-row"><strong>{device.name}</strong><button type="button" className="btn btn-ghost" disabled={busy !== null} onClick={() => onForget(device)}>{busy === `forget:${device.device_id}` ? 'Forgetting…' : 'Forget'}</button></div>
        ))}
      </div>
      <details className="connection-advanced"><summary>Enter a pairing link from another machine</summary>
        <div className="pair-device-controls"><input value={incomingLink} onChange={(event) => onIncomingLink(event.currentTarget.value)} onKeyDown={(event) => { if (event.key === 'Enter') onAdd(); }} placeholder="sessions://pair?host=…&t=…" autoComplete="off" spellCheck={false} /><button type="button" className="btn" disabled={!isTauri() || busy !== null || !incomingLink.trim()} onClick={onAdd}>{busy === 'incoming-pair' ? 'Adding…' : 'Pair this Mac'}</button></div>
        {incomingMessage ? <div className="connections-message" role="status">{incomingMessage}</div> : null}
      </details>
    </section>
  );
}

function PairingTicketCard({ pair, revoking, onRevoke, onCopy }: { pair: PairState; revoking: boolean; onRevoke: () => void; onCopy: () => void }): JSX.Element {
  const expires = new Date(pair.expires_at).getTime();
  const [remaining, setRemaining] = useState(() => Math.max(0, Math.ceil((expires - Date.now()) / 1000)));
  useEffect(() => {
    const tick = (): void => setRemaining(Math.max(0, Math.ceil((expires - Date.now()) / 1000)));
    tick();
    const timer = window.setInterval(tick, 1_000);
    return () => window.clearInterval(timer);
  }, [expires]);
  const countdown = remaining === 0
    ? 'Expired'
    : `${String(Math.floor(remaining / 60)).padStart(2, '0')}:${String(remaining % 60).padStart(2, '0')}`;
  return (
    <div className="pair-result">
      <div><strong>Pairing code ready</strong><span>{countdown}</span></div>
      <img className="pair-qr" src={pair.qr_data_url} alt="One-time Sessions pairing QR code" />
      <code>{pair.link}</code>
      <a href={pair.fallback} target="_blank" rel="noreferrer">Open plain HTTPS or LAN fallback</a>
      <div className="connection-actions">
        <button type="button" className="btn" onClick={onCopy}>Copy link</button>
        <button type="button" className="btn btn-ghost" disabled={revoking} onClick={onRevoke}>{revoking ? 'Revoking…' : 'Revoke'}</button>
      </div>
    </div>
  );
}

interface LocalNetworkPermissionPromptProps {
  visible: boolean;
  disabled: boolean;
  onMessage: Dispatch<SetStateAction<string | null>>;
  onState: Dispatch<SetStateAction<LANState | null>>;
}

function LocalNetworkPermissionPrompt({ visible, disabled, onMessage, onState }: LocalNetworkPermissionPromptProps): JSX.Element | null {
  const [waiting, setWaiting] = useState(false);
  if (!visible) return null;
  const allow = async (): Promise<void> => {
    setWaiting(true); onMessage(null);
    try {
      await requestLocalNetworkAccess();
      onMessage('Sessions can use the local network.');
      onState(await fetchLANState());
    } catch (reason) {
      onMessage(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setWaiting(false);
    }
  };
  return (
    <div className="connection-privacy-note">
      <strong>macOS Local Network permission</strong>
      <span>Ask macOS while this page is open.</span>
      <button type="button" className="btn" disabled={disabled || waiting} onClick={() => void allow()}>{waiting ? 'Waiting for macOS…' : 'Allow local network'}</button>
    </div>
  );
}

function HostConnectionChoice({ hostName }: { hostName: string }): JSX.Element {
  return <div className="settings-host-choice">Chosen on {hostName}</div>;
}

function ConnectionCard({ step, title, state, active = false, children }: { step: string; title: string; state: string; active?: boolean; children: ReactNode }): JSX.Element {
  return (
    <article className={`connection-card${active ? ' is-active' : ''}`}>
      <header><span>{step}</span><h2>{title}</h2><strong>{state}</strong></header>
      <div className="connection-card-body">{children}</div>
    </article>
  );
}
