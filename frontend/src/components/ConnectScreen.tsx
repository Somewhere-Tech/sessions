import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent } from 'react';
import { fetchServerHealth } from '../api/sessionsd';
import { claimNativeMachinePairing, rememberServerEndpoint } from '../lib/hostedBootstrap';
import { formatServerEndpoint } from '../lib/serverEndpoint';
import { useServers, type ServerConfig } from '../lib/servers';
import { tailnetClientID } from '../lib/tailnetClient';
import {
  discoverNativeMachines,
  isNativeMobileRuntime,
  requestNativeMachineAccess,
  type NativeMachinePeer,
  type NativeTailnetRequest
} from '../lib/tauriBridge';
import { useMachineAccessPairing } from '../hooks/useMachineAccessPairing';

const LOCAL_ENDPOINT = 'http://localhost:8787';
const HEALTH_TIMEOUT_MS = 8_000;

interface ConnectScreenProps {
  clientOnly?: boolean;
  onRetry?: () => void;
}

export function ConnectScreen({
  clientOnly = false,
  onRetry
}: ConnectScreenProps = {}): JSX.Element {
  const servers = useServers((state) => state.servers);
  const setActive = useServers((state) => state.setActive);
  const removeServer = useServers((state) => state.removeServer);
  const pairingError = useServers((state) => state.pairingError);
  const credentialError = useServers((state) => state.credentialError);
  const setPairingError = useServers((state) => state.setPairingError);
  const [name, setName] = useState('');
  const [endpoint, setEndpoint] = useState('');
  const [token, setToken] = useState('');
  const [pairingLink, setPairingLink] = useState('');
  const [checkingId, setCheckingId] = useState<string | null>(null);
  const [discoveryBusy, setDiscoveryBusy] = useState(false);
  const [discoveredPeers, setDiscoveredPeers] = useState<NativeMachinePeer[] | null>(null);
  const [accessRequest, setAccessRequest] = useState<(NativeTailnetRequest & { transport: NativeMachinePeer['transport']; machineName: string }) | null>(null);
  const [message, setMessage] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const busy = checkingId !== null || discoveryBusy || accessRequest !== null;
  const connectionDisabled = credentialError !== null || busy;
  const remembered = useMemo(
    () => servers.filter((server) => !server.isDefault),
    [servers]
  );
  const isMobileClient = isNativeMobileRuntime();
  const discoveryStarted = useRef(false);
  const pairingLinkInput = useRef<HTMLInputElement>(null);

  const keepPairingLinkVisible = useCallback((): void => {
    pairingLinkInput.current?.scrollIntoView({ block: 'center', inline: 'nearest' });
  }, []);

  useEffect(() => {
    const viewport = window.visualViewport;
    if (!viewport) return undefined;
    const onViewportChange = (): void => {
      if (document.activeElement === pairingLinkInput.current) keepPairingLinkVisible();
    };
    viewport.addEventListener('resize', onViewportChange);
    return () => {
      viewport.removeEventListener('resize', onViewportChange);
    };
  }, [keepPairingLinkVisible]);

  const claimPairingLink = async (): Promise<void> => {
    if (!clientOnly || connectionDisabled || !pairingLink.trim()) return;
    setCheckingId('pairing-link');
    setMessage('Claiming this one-time connection link…');
    setError(null);
    try {
      const { server } = await claimNativeMachinePairing(pairingLink.trim());
      setPairingLink('');
      setMessage(`${server.name} approved this device. Connecting…`);
      onRetry?.();
    } catch (reason) {
      setMessage(null);
      setError(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setCheckingId(null);
    }
  };

  const findMachines = useCallback(async (): Promise<void> => {
    if (!clientOnly || connectionDisabled) return;
    setDiscoveryBusy(true);
    setMessage(isMobileClient
      ? 'Looking for Sessions machines nearby…'
      : 'Looking for Sessions machines through Tailscale and nearby Bonjour…');
    setError(null);
    try {
      const result = await discoverNativeMachines();
      setDiscoveredPeers(result.peers);
      setMessage(result.peers.length > 0
        ? `Found ${result.peers.length} ${result.peers.length === 1 ? 'machine' : 'machines'}.`
        : result.errors[0] ?? (isMobileClient
          ? 'No nearby Sessions machines answered. Enable trusted-network LAN access on the host, then search again.'
          : 'No Sessions machines answered. Enable Tailscale remote access or trusted-network LAN access on the host.'));
    } catch (reason) {
      setDiscoveredPeers([]);
      setMessage(null);
      setError(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setDiscoveryBusy(false);
    }
  }, [clientOnly, connectionDisabled, isMobileClient]);

  useEffect(() => {
    if (!clientOnly || !isMobileClient || discoveryStarted.current) return;
    discoveryStarted.current = true;
    void findMachines();
  }, [clientOnly, findMachines, isMobileClient]);

  const requestAccess = async (peer: NativeMachinePeer): Promise<void> => {
    if (!clientOnly || connectionDisabled) return;
    setDiscoveryBusy(true);
    setError(null);
    try {
      const request = await requestNativeMachineAccess(peer, tailnetClientID(), 'This phone');
      setAccessRequest({ ...request, transport: peer.transport, machineName: peer.name });
      setMessage(`Waiting for ${peer.name} to approve this phone.`);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setDiscoveryBusy(false);
    }
  };

  // One shared implementation of the approval poll (hooks/useMachineAccessPairing.ts).
  // This was one of three hand-rolled copies whose wording had already drifted.
  const pendingAccess = useMemo(
    () => accessRequest ? { transport: accessRequest.transport, request: accessRequest } : null,
    [accessRequest]
  );
  useMachineAccessPairing({
    pending: pendingAccess,
    onAccepted: (server) => {
      setAccessRequest(null);
      setMessage(`${server.name} approved this device. Connecting…`);
      onRetry?.();
    },
    onSettled: (_outcome, text) => {
      setAccessRequest(null);
      setMessage(null);
      setError(text);
    },
    onError: setError
  });

  const probe = async (server: ServerConfig): Promise<void> => {
    if (credentialError) {
      setError(credentialError);
      return;
    }
    setPairingError(null);
    setCheckingId(server.id);
    setMessage(`Checking ${formatServerEndpoint(server)}…`);
    setError(null);
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), HEALTH_TIMEOUT_MS);
    try {
      await fetchServerHealth(server, controller.signal);
      setActive(server.id);
      onRetry?.();
    } catch (probeError) {
      const detail = probeError instanceof Error && probeError.name !== 'AbortError'
        ? probeError.message
        : 'The endpoint did not answer within 8 seconds.';
      setMessage(null);
      setError(`${detail} Check Tailscale, then run sessions doctor on the daemon host.`);
    } finally {
      window.clearTimeout(timeout);
      setCheckingId(null);
    }
  };

  const addAndProbe = async (
    endpointValue: string,
    options: { name?: string; token?: string } = {}
  ): Promise<void> => {
    if (credentialError) {
      setError(credentialError);
      return;
    }
    setPairingError(null);
    setError(null);
    let server: ServerConfig;
    try {
      server = await rememberServerEndpoint(endpointValue, {
        name: options.name,
        token: options.token,
        select: false
      });
    } catch (validationError) {
      setError(validationError instanceof Error ? validationError.message : 'Enter a valid endpoint.');
      return;
    }
    await probe(server);
  };

  const submit = (event: FormEvent<HTMLFormElement>): void => {
    event.preventDefault();
    if (connectionDisabled) return;
    void addAndProbe(endpoint, { name, token });
  };

  return (
    <main className={`connect-screen${clientOnly ? ' connect-screen-client' : ''}`} data-testid="connect-screen">
      <section className="connect-panel" aria-labelledby="connect-title">
        <div className="connect-brand">Sessions</div>
        <p className="connect-kicker">
          {clientOnly ? 'this device → your Sessions machines' : 'native window → your daemon'}
        </p>
        <h1 id="connect-title">
          {clientOnly ? 'Find your Sessions machines.' : 'Open your sessions from here.'}
        </h1>
        <p className="connect-lede">
          {clientOnly
            ? 'Sessions connects directly over your private network. The computer running each agent stays in control, and approves this device before anything opens.'
            : 'This is the complete Sessions app. Pick a daemon and this client talks straight to it — no relay, proxy, hosted terminal data, or analytics.'}
        </p>

        {clientOnly ? (
          <>
          <section className="connect-discovery" aria-labelledby="discovery-title" aria-live="polite">
            <div className="connect-discovery-heading">
              <div>
                <span>Nearby on your network</span>
                <h2 id="discovery-title">Your Sessions machines</h2>
                <p>Choose a computer, then approve this phone there.</p>
              </div>
              <button type="button" className="connect-submit connect-find" disabled={connectionDisabled} onClick={() => void findMachines()}>
                {discoveryBusy ? 'Searching…' : discoveredPeers === null ? 'Find machines' : 'Search again'}
              </button>
            </div>
            {accessRequest ? (
              <p className="connect-waiting">Waiting for {accessRequest.machineName} to approve this phone.</p>
            ) : null}
            {discoveredPeers !== null && discoveredPeers.length > 0 ? (
              <div className="connect-peer-list">
                {discoveredPeers.map((peer) => {
                  const waiting = accessRequest?.endpoint === peer.endpoint;
                  return (
                    <article key={`${peer.transport}:${peer.endpoint}`} className="connect-peer">
                      <span className="connect-peer-icon" aria-hidden>
                        {peer.os ? (peer.os.toLowerCase().includes('windows') ? '⊞' : '⌘') : '⌁'}
                      </span>
                      <div>
                        <strong>{peer.name}</strong>
                        <small>{peer.hostname}</small>
                      </div>
                      <button type="button" className="btn" disabled={connectionDisabled} onClick={() => void requestAccess(peer)}>
                        {waiting ? 'Waiting…' : 'Connect'}
                      </button>
                    </article>
                  );
                })}
              </div>
            ) : null}
          </section>
          <section className="connect-pair-link" aria-labelledby="pair-link-title">
            <div>
              <span>One-time-link fallback</span>
              <h2 id="pair-link-title">Paste a connection link</h2>
              <p>If this computer does not appear, run <code>sessions pair</code> there and paste its one-time link.</p>
            </div>
            <form onSubmit={(event) => { event.preventDefault(); void claimPairingLink(); }}>
              <input
                ref={pairingLinkInput}
                type="url"
                inputMode="url"
                autoComplete="off"
                placeholder="Paste the Sessions connection link"
                value={pairingLink}
                onChange={(event) => setPairingLink(event.currentTarget.value)}
                onFocus={keepPairingLinkVisible}
              />
              <button type="submit" className="connect-submit" disabled={connectionDisabled || !pairingLink.trim()}>
                {checkingId === 'pairing-link' ? 'Connecting…' : 'Connect this device'}
              </button>
            </form>
          </section>
          </>
        ) : null}

        {remembered.length > 0 ? (
          <section className="connect-remembered" aria-labelledby="remembered-title">
            <h2 id="remembered-title">Remembered servers</h2>
            <div className="connect-server-list">
              {remembered.map((server) => (
                <div className="connect-server-row" key={server.id}>
                  <button
                    type="button"
                    className="connect-server-pick"
                    disabled={connectionDisabled}
                    onClick={() => void probe(server)}
                  >
                    <span className="connect-server-name">{server.name}</span>
                    <span className="connect-server-endpoint">{formatServerEndpoint(server)}</span>
                    <span aria-hidden>{checkingId === server.id ? 'checking…' : 'connect →'}</span>
                  </button>
                  <button
                    type="button"
                    className="connect-server-remove"
                    disabled={connectionDisabled}
                    aria-label={`Forget ${server.name}`}
                    onClick={() => {
                      void removeServer(server.id).catch((reason) => {
                        setError(reason instanceof Error ? reason.message : String(reason));
                      });
                    }}
                  >
                    ×
                  </button>
                </div>
              ))}
            </div>
          </section>
        ) : null}

        <div className="connect-actions">
          {!clientOnly ? (
            <button
              type="button"
              className="connect-local-button"
              disabled={connectionDisabled}
              onClick={() => void addAndProbe(LOCAL_ENDPOINT, { name: 'This machine' })}
            >
              <span className="connect-local-icon" aria-hidden>⌁</span>
              <span>
                <strong>Connect on this device</strong>
                <small>{LOCAL_ENDPOINT}</small>
              </span>
              <span aria-hidden>→</span>
            </button>
          ) : null}

          <div className="connect-divider">
            <span>{clientOnly ? 'or enter connection details' : 'or add a server'}</span>
          </div>

          <form className="connect-form" onSubmit={submit}>
            <label>
              <span>Name <small>optional</small></span>
              <input
                type="text"
                autoComplete="off"
                placeholder="Studio Mac"
                value={name}
                onChange={(event) => setName(event.currentTarget.value)}
              />
            </label>
            <label>
              <span>Endpoint</span>
              <input
                type="url"
                inputMode="url"
                autoComplete="url"
                placeholder="https://mac.example.ts.net"
                required
                value={endpoint}
                onChange={(event) => setEndpoint(event.currentTarget.value)}
              />
            </label>
            <label>
              <span>Token <small>from the connect link</small></span>
              <input
                type="password"
                autoComplete="off"
                placeholder="Optional on localhost"
                value={token}
                onChange={(event) => setToken(event.currentTarget.value)}
              />
            </label>
            <button type="submit" className="connect-submit" disabled={connectionDisabled || !endpoint.trim()}>
              {credentialError ? 'Credentials unavailable' : busy ? 'Checking daemon…' : 'Connect to Sessions'}
            </button>
          </form>
        </div>

        {message ? <p className="connect-status" role="status">{message}</p> : null}
        {credentialError || error || pairingError ? (
          <p className="connect-error" role="alert">
            {credentialError ?? error ?? pairingError}
          </p>
        ) : null}

        <section className="connect-setup" aria-labelledby="setup-title">
          <h2 id="setup-title">First time?</h2>
          <ol>
            <li>
              <span>Install and start Sessions on the Mac that owns your sessions.</span>
              <code>brew install somewhere-tech/tap/sessions &amp;&amp; sessions install</code>
            </li>
            <li>
              <span>{isMobileClient
                ? 'Enable direct access on the trusted Wi-Fi or Ethernet network shared with your phone.'
                : 'Enable direct HTTPS access over your own Tailscale network.'}</span>
              <code>{isMobileClient ? 'sessions lan enable' : 'sessions remote enable'}</code>
            </li>
            <li>
              <span>
                {isMobileClient
                  ? 'Choose a nearby Sessions machine above and approve this phone there, or paste a one-time link.'
                  : clientOnly
                    ? 'Paste the one-time link above. On desktop clients, you can also discover and request access from the app.'
                  : 'Scan the printed QR code, or paste its endpoint and token above.'}
              </span>
            </li>
          </ol>
        </section>

        <p className="connect-privacy">
          Endpoint and revocable device token stay on this device. Native Windows
          protects saved tokens for the signed-in user; URL-fragment connect links
          are scrubbed before the app starts.
        </p>
      </section>
    </main>
  );
}
