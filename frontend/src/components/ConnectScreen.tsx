import { useEffect, useMemo, useState, type FormEvent } from 'react';
import { fetchServerHealth } from '../api/sessionsd';
import { claimNativeMachinePairing, rememberNativeMachineClaim, rememberServerEndpoint } from '../lib/hostedBootstrap';
import { formatServerEndpoint } from '../lib/serverEndpoint';
import { useServers, type ServerConfig } from '../lib/servers';
import { tailnetClientID } from '../lib/tailnetClient';
import {
  claimNativeMachineAccess,
  discoverNativeMachines,
  requestNativeMachineAccess,
  type NativeMachinePeer,
  type NativeTailnetRequest
} from '../lib/tauriBridge';

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
  const [accessRequest, setAccessRequest] = useState<(NativeTailnetRequest & { transport: NativeMachinePeer['transport'] }) | null>(null);
  const [message, setMessage] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const busy = checkingId !== null || discoveryBusy || accessRequest !== null;
  const connectionDisabled = credentialError !== null || busy;
  const remembered = useMemo(
    () => servers.filter((server) => !server.isDefault),
    [servers]
  );
  const isAndroidClient = typeof navigator !== 'undefined' && /Android/i.test(navigator.userAgent);

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

  const findMachines = async (): Promise<void> => {
    if (!clientOnly || connectionDisabled) return;
    setDiscoveryBusy(true);
    setMessage('Looking for Sessions machines through Tailscale and nearby Bonjour…');
    setError(null);
    try {
      const result = await discoverNativeMachines();
      setDiscoveredPeers(result.peers);
      setMessage(result.peers.length > 0
        ? `Found ${result.peers.length} ${result.peers.length === 1 ? 'machine' : 'machines'}.`
        : result.errors[0] ?? 'No Sessions machines answered. Enable Tailscale remote access or trusted-network LAN access on the host.');
    } catch (reason) {
      setDiscoveredPeers([]);
      setMessage(null);
      setError(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setDiscoveryBusy(false);
    }
  };

  const requestAccess = async (peer: NativeMachinePeer): Promise<void> => {
    if (!clientOnly || connectionDisabled) return;
    setDiscoveryBusy(true);
    setError(null);
    try {
      const request = await requestNativeMachineAccess(peer, tailnetClientID(), '');
      setAccessRequest({ ...request, transport: peer.transport });
      setMessage(`Request sent to ${peer.name}. Accept it in Sessions on that machine.`);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setDiscoveryBusy(false);
    }
  };

  useEffect(() => {
    if (!accessRequest) return;
    let cancelled = false;
    let checking = false;
    const check = async (): Promise<void> => {
      if (checking) return;
      checking = true;
      try {
        const result = await claimNativeMachineAccess(accessRequest.transport, accessRequest);
        if (cancelled || result.status === 'pending') return;
        if (result.status === 'accepted' && result.claim) {
          const server = await rememberNativeMachineClaim(result.claim);
          if (!cancelled) {
            setAccessRequest(null);
            setMessage(`${server.name} approved this device. Connecting…`);
          }
          return;
        }
        if (!cancelled) {
          setAccessRequest(null);
          setMessage(null);
          setError(result.status === 'denied'
            ? 'The other machine denied this request.'
            : 'The request expired. Search again when someone is at the other machine.');
        }
      } catch (reason) {
        if (!cancelled) setError(reason instanceof Error ? reason.message : String(reason));
      } finally {
        checking = false;
      }
    };
    void check();
    const interval = window.setInterval(() => { void check(); }, 2_000);
    return () => {
      cancelled = true;
      window.clearInterval(interval);
    };
  }, [accessRequest]);

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
    <main className="connect-screen" data-testid="connect-screen">
      <section className="connect-panel" aria-labelledby="connect-title">
        <div className="connect-brand">Sessions</div>
        <p className="connect-kicker">
          {clientOnly ? 'this device → your Sessions machines' : 'native window → your daemon'}
        </p>
        <h1 id="connect-title">
          {clientOnly ? 'Find the computers running your sessions.' : 'Open your sessions from here.'}
        </h1>
        <p className="connect-lede">
          {clientOnly
            ? 'Sessions connects directly over your private network. The computer running each agent stays in control, and approves this device before anything opens.'
            : 'This is the complete Sessions app. Pick a daemon and this client talks straight to it — no relay, proxy, hosted terminal data, or analytics.'}
        </p>

        {clientOnly ? (
          <>
          <section className="connect-pair-link" aria-labelledby="pair-link-title">
            <div>
              <span>Fastest setup</span>
              <h2 id="pair-link-title">Connect with a one-time link</h2>
              <p>On the computer running Sessions, enable trusted-network access with <code>sessions lan enable</code>, then ask your agent to run <code>sessions pair</code>. Paste the link here; its ticket is consumed once and this device receives its own revocable credential.</p>
            </div>
            <form onSubmit={(event) => { event.preventDefault(); void claimPairingLink(); }}>
              <input
                type="url"
                inputMode="url"
                autoComplete="off"
                placeholder="Paste the Sessions connection link"
                value={pairingLink}
                onChange={(event) => setPairingLink(event.currentTarget.value)}
              />
              <button type="submit" className="connect-submit" disabled={connectionDisabled || !pairingLink.trim()}>
                {checkingId === 'pairing-link' ? 'Connecting…' : 'Connect this device'}
              </button>
            </form>
          </section>
          <section className="connect-discovery" aria-labelledby="discovery-title">
            <div className="connect-discovery-heading">
              <div>
                <span>Private machine discovery</span>
                <h2 id="discovery-title">Find your machines</h2>
                <p>{isAndroidClient
                  ? 'Automatic Android discovery and Tailscale onboarding are coming next. The one-time link above connects directly on a trusted LAN today.'
                  : 'Sessions checks encrypted Tailscale and nearby Bonjour independently, then shows only verified Sessions runtimes.'}</p>
              </div>
              {!isAndroidClient ? (
                <button type="button" className="connect-submit connect-find" disabled={connectionDisabled} onClick={() => void findMachines()}>
                  {discoveryBusy ? 'Searching…' : discoveredPeers === null ? 'Find machines' : 'Search again'}
                </button>
              ) : <span className="connect-coming-soon">Coming next</span>}
            </div>
            {discoveredPeers !== null && discoveredPeers.length > 0 ? (
              <div className="connect-peer-list">
                {discoveredPeers.map((peer) => {
                  const waiting = accessRequest?.endpoint === peer.endpoint;
                  return (
                    <article key={peer.endpoint} className="connect-peer">
                      <span className="connect-peer-icon" aria-hidden>{peer.os.toLowerCase().includes('windows') ? '⊞' : peer.os.toLowerCase().includes('mac') ? '⌘' : '◇'}</span>
                      <div>
                        <strong>{peer.name}</strong>
                        <small>{peer.transport === 'tailnet' ? 'Tailscale · encrypted' : 'Nearby · unencrypted'} · {peer.endpoint.replace(/^https?:\/\//, '')}</small>
                      </div>
                      <button type="button" className="btn" disabled={connectionDisabled} onClick={() => void requestAccess(peer)}>
                        {waiting ? 'Waiting for approval…' : 'Request access'}
                      </button>
                    </article>
                  );
                })}
              </div>
            ) : null}
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
              <span>{isAndroidClient
                ? 'Enable direct access on the trusted Wi-Fi or Ethernet network shared with your phone.'
                : 'Enable direct HTTPS access over your own Tailscale network.'}</span>
              <code>{isAndroidClient ? 'sessions lan enable && sessions pair' : 'sessions remote enable'}</code>
            </li>
            <li>
              <span>
                {isAndroidClient
                  ? 'Paste the printed one-time link above. Tailscale discovery and request/accept onboarding are next.'
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
