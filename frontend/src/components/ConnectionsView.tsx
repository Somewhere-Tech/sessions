import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react';
import { fetchLANState, setLANEnabled, type LANState } from '../api/sessionsd';
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
import { configureNativeLocalPort } from '../lib/servers';
import { claimNativeMachinePairing } from '../lib/hostedBootstrap';
import { tailnetClientID } from '../lib/tailnetClient';
import { useMachineAccessPairing } from '../hooks/useMachineAccessPairing';
import { SomewhereCard } from './SomewhereCard';

interface RemoteState {
  enabled: boolean;
  verified: boolean;
  endpoint?: string;
  target?: string | null;
  connectUrl?: string;
  verificationError?: string;
}

interface PairState {
  url: string;
  endpoint: string;
  ticket: string;
  expires_at: string;
}

export function ConnectionsView(): JSX.Element {
  const [native, setNative] = useState<NativeConnectionSettings | null>(null);
  const [port, setPort] = useState('8787');
  const [lan, setLAN] = useState<LANState | null>(null);
  const [remote, setRemote] = useState<RemoteState | null>(null);
  const [remoteError, setRemoteError] = useState<string | null>(null);
  const [pair, setPair] = useState<PairState | null>(null);
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
    const lanRequest = isTauri()
      ? runNativeConnectionAction<LANState>('lan', 'status').then((result) => result.data)
      : fetchLANState();
    const [nativeResult, lanResult] = await Promise.all([
      getNativeConnectionSettings().catch(() => null),
      lanRequest.catch(() => null)
    ]);
    setNative(nativeResult);
    if (nativeResult) setPort(String(nativeResult.port));
    setLAN(lanResult);
    if (!isTauri()) return;
    try {
      const status = await runNativeConnectionAction<RemoteState>('remote', 'status');
      setRemote(status.data);
      setRemoteError(null);
    } catch (reason) {
      setRemote(null);
      setRemoteError(reason instanceof Error ? reason.message : String(reason));
    }
  }, []);

  useEffect(() => { void refresh(); }, [refresh]);

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
    if (busy) return;
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

  const changeRemote = async (enabled: boolean): Promise<void> => {
    if (busy || !isTauri()) return;
    setBusy('remote'); setMessage(null); setPair(null);
    try {
      const result = await runNativeConnectionAction<RemoteState>('remote', enabled ? 'enable' : 'disable');
      setRemote(result.data);
      setRemoteError(null);
      if (result.detail) setMessage(result.detail);
    } catch (reason) {
      const detail = reason instanceof Error ? reason.message : String(reason);
      setRemoteError(detail); setMessage(detail);
    } finally {
      setBusy(null);
    }
  };

  const createPair = async (): Promise<void> => {
    if (busy || !isTauri()) return;
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
    if (busy || !native) return;
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
          <div><span>Private by default</span><h1>Connections</h1><p>This computer is the server. Sessions never relays terminal traffic.</p></div>
          <button type="button" className="btn btn-ghost" disabled={busy !== null} onClick={() => void refresh()}>Refresh status</button>
        </header>

        {message ? <div className="connections-message" role="status">{message}</div> : null}

        <section className="connection-ladder" aria-label="Connection options">
          <ConnectionCard step="01" title="This computer" state="Always private" active>
            <p>Sessions.app talks to the independent loopback daemon. Quitting the app never stops its sessions.</p>
            <div className="connection-endpoint">http://localhost:{native?.port ?? port}</div>
            <details className="connection-advanced">
              <summary>Advanced port</summary>
              <p>Changing this restarts only the daemon and rolls back on failure. Windows currently changes ports only while no sessions are live; Mac verifies every live runner is re-adopted.</p>
              <div className="connection-inline">
                <input type="number" min={1024} max={65535} value={port} onChange={(event) => setPort(event.currentTarget.value)} disabled={!native || native.runtime.state !== 'ready' || busy !== null} />
                <button type="button" className="btn" disabled={!native || native.runtime.state !== 'ready' || busy !== null || Number(port) === native.port} onClick={() => void savePort()}>{busy === 'port' ? 'Moving…' : 'Change port'}</button>
              </div>
              {native?.runtime.state === 'development' ? <small>Installed-app only. Development builds use the separately managed dev daemon.</small> : null}
            </details>
          </ConnectionCard>

          <ConnectionCard step="02" title="Same Wi-Fi" state={lan?.enabled ? 'On' : 'Off'} active={lan?.enabled === true}>
            <p>Connect another native Sessions client on a private network you trust. Bonjour discovery starts with LAN access; browser terminal access is intentionally not a product surface.</p>
            {lan?.url ? <div className="connection-endpoint">{lan.url}</div> : null}
            {lan?.enabled ? (
              <div className="connection-privacy-note">
                <strong>{lan.bonjour?.advertised ? 'Visible to nearby Sessions apps' : 'Nearby discovery unavailable'}</strong>
                <span>{lan.bonjour?.error ?? 'LAN traffic is authenticated but unencrypted. Prefer Tailscale on shared or public Wi-Fi.'}</span>
              </div>
            ) : null}
            <button type="button" className={`btn${lan?.enabled ? ' btn-ghost' : ''}`} disabled={busy !== null} onClick={() => void changeLAN(!lan?.enabled)}>{busy === 'lan' ? 'Checking…' : lan?.enabled ? 'Turn off LAN' : 'Enable LAN access'}</button>
          </ConnectionCard>

          <ConnectionCard step="03" title="Anywhere" state={remote?.enabled ? 'Tailscale HTTPS on' : 'Off'} active={remote?.enabled === true}>
            <p>Tailscale Serve keeps the connection inside your tailnet with HTTPS terminating on this Mac. Sessions operates no relay.</p>
            {remote?.endpoint ? <div className="connection-endpoint">{remote.endpoint}</div> : null}
            {remote?.enabled ? <div className="connection-privacy-note"><strong>Ready for requests</strong><span>Other Sessions apps in this tailnet can discover this Mac. You still approve every new device.</span></div> : null}
            {remote?.verificationError ? <div className="connection-privacy-note"><strong>Configured on this Mac</strong><span>{remote.verificationError}</span></div> : null}
            {remoteError ? <div className="connection-error">{remoteError}</div> : null}
            {!remote?.enabled ? <div className="connection-privacy-note"><strong>Before enabling</strong><span>Tailscale issues a public certificate, so the machine’s <code>.ts.net</code> name appears in Certificate Transparency logs.</span></div> : null}
            <div className="connection-actions">
              <button type="button" className={`btn${remote?.enabled ? ' btn-ghost' : ''}`} disabled={!isTauri() || busy !== null} onClick={() => void changeRemote(!remote?.enabled)}>{busy === 'remote' ? 'Verifying…' : remote?.enabled ? 'Disable remote access' : 'Enable Tailscale HTTPS'}</button>
              {remoteError?.toLowerCase().includes('not installed') ? <a className="btn btn-ghost" href="https://tailscale.com/download" target="_blank" rel="noreferrer">Get Tailscale</a> : null}
            </div>
          </ConnectionCard>
        </section>

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

        <section className="pair-device-card">
          <div><span className="connections-section-kicker">Local fallback · five minutes</span><h2>Pair without Tailscale</h2><p>For a device on the same Wi-Fi, create a single-use link. The other device gets its own revocable credential; your master daemon token is never shared.</p></div>
          <div className="pair-device-controls">
            <input value={pairName} maxLength={80} onChange={(event) => setPairName(event.currentTarget.value)} placeholder="Device name" />
            <button type="button" className="btn" disabled={!isTauri() || busy !== null || !lan?.enabled} onClick={() => void createPair()}>{busy === 'pair' ? 'Creating…' : 'Create LAN pairing link'}</button>
          </div>
          {!lan?.enabled ? <small>Enable Same Wi-Fi above before creating a LAN pairing link.</small> : null}
          {pair ? (
            <div className="pair-result">
              <div><strong>LAN pairing link ready</strong><span>Expires {new Date(pair.expires_at).toLocaleTimeString()}</span></div>
              <code>{pair.url}</code>
              <div className="connection-actions">
                <button type="button" className="btn" onClick={() => void navigator.clipboard.writeText(pair.url).then(() => setMessage('Pairing link copied.'))}>Copy link</button>
              </div>
            </div>
          ) : null}
          <details className="connection-advanced">
            <summary>Enter a LAN pairing link from another Mac</summary>
            <div className="pair-device-controls">
              <input value={incomingPairLink} onChange={(event) => setIncomingPairLink(event.currentTarget.value)} onKeyDown={(event) => { if (event.key === 'Enter') void addMachine(); }} placeholder="http://192.168.…/#pair=…" autoComplete="off" spellCheck={false} />
              <button type="button" className="btn" disabled={!isTauri() || busy !== null || !incomingPairLink.trim()} onClick={() => void addMachine()}>{busy === 'incoming-pair' ? 'Adding…' : 'Pair over LAN'}</button>
            </div>
            {incomingPairMessage ? <div className="connections-message" role="status">{incomingPairMessage}</div> : null}
          </details>
        </section>

        <SomewhereCard />
      </div>
    </div>
  );
}

function ConnectionCard({ step, title, state, active = false, children }: { step: string; title: string; state: string; active?: boolean; children: ReactNode }): JSX.Element {
  return (
    <article className={`connection-card${active ? ' is-active' : ''}`}>
      <header><span>{step}</span><h2>{title}</h2><strong>{state}</strong></header>
      <div className="connection-card-body">{children}</div>
    </article>
  );
}
