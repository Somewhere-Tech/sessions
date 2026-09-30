import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent } from 'react';
import { fetchServerHealth } from '../api/sessionsd';
import { claimNativeMachinePairing, rememberServerEndpoint } from '../lib/hostedBootstrap';
import { formatServerEndpoint } from '../lib/serverEndpoint';
import { useServers, type ServerConfig } from '../lib/servers';
import {
  isNativeMobileRuntime,
  scanPairingCode
} from '../lib/tauriBridge';
import type { ClientFleetStatus } from '../lib/clientFleetAccount';

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
  const [message, setMessage] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const busy = checkingId !== null;
  const connectionDisabled = credentialError !== null || busy;
  const remembered = useMemo(
    () => servers.filter((server) => !server.isDefault),
    [servers]
  );
  const isMobileClient = isNativeMobileRuntime();

  const connectPairingLink = async (link: string, source: 'pairing-link' | 'pairing-code'): Promise<void> => {
    if (!clientOnly || connectionDisabled || !link.trim()) return;
    setCheckingId(source);
    setMessage('Claiming this one-time connection link…');
    setError(null);
    try {
      const { server } = await claimNativeMachinePairing(link.trim());
      setPairingLink('');
      setMessage(`Paired with ${server.name}. Connecting…`);
      onRetry?.();
    } catch (reason) {
      setMessage(null);
      setError(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setCheckingId(null);
    }
  };

  const scanAndConnect = async (): Promise<void> => {
    if (!clientOnly || connectionDisabled) return;
    setCheckingId('pairing-code'); setMessage('Opening the camera…'); setError(null);
    try {
      const link = await scanPairingCode();
      setCheckingId(null);
      await connectPairingLink(link, 'pairing-code');
    } catch (reason) {
      setMessage(null);
      setError(reason instanceof Error ? reason.message : String(reason));
      setCheckingId(null);
    }
  };

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
            ? 'Scan a host pairing code to connect directly over your private network.'
            : 'This is the complete Sessions app. Pick a daemon and this client talks straight to it — no relay, proxy, hosted terminal data, or analytics.'}
        </p>

        {clientOnly ? (
          <>
		  <ClientFleetSignIn onConnected={() => onRetry?.()} />
          <PairingLinkPanel
            link={pairingLink} busy={connectionDisabled} checkingId={checkingId}
            mobile={isMobileClient} onChange={setPairingLink}
            onScan={() => void scanAndConnect()}
            onConnect={() => void connectPairingLink(pairingLink, 'pairing-link')}
          />
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

function ClientFleetSignIn({ onConnected }: { onConnected: () => void }): JSX.Element {
	const [status, setStatus] = useState<ClientFleetStatus>({ signedIn: false });
	const [email, setEmail] = useState('');
	const [code, setCode] = useState('');
	const [sent, setSent] = useState(false);
	const [busy, setBusy] = useState(false);
	const [message, setMessage] = useState<string | null>(null);
	const sync = async (): Promise<void> => {
		setBusy(true); setMessage('Loading your fleet…');
		try {
			const { syncClientAccountFleet } = await import('../lib/clientFleetAccount');
			const errors = await syncClientAccountFleet();
			setMessage(errors.length > 0 ? `Fleet loaded. ${errors.join(' · ')}` : 'Your fleet is ready without pairing.');
			onConnected();
		} catch (reason) {
			setMessage(reason instanceof Error ? reason.message : String(reason));
		} finally { setBusy(false); }
	};
	useEffect(() => {
		void import('../lib/clientFleetAccount').then((client) => {
			const current = client.clientFleetStatus();
			setStatus(current);
			if (current.signedIn) void sync();
		});
	}, []); // eslint-disable-line react-hooks/exhaustive-deps
	const submit = async (): Promise<void> => {
		setBusy(true); setMessage(null);
		try {
			const client = await import('../lib/clientFleetAccount');
			if (!sent) {
				await client.requestClientFleetMagicLink(email);
				setSent(true); setMessage(`Somewhere sent a single-use code or link to ${email.trim()}.`);
			} else {
				setStatus(await client.verifyClientFleetMagicLink(code));
				setSent(false); setCode('');
				await sync();
			}
		} catch (reason) {
			setMessage(reason instanceof Error ? reason.message : String(reason));
		} finally { setBusy(false); }
	};
	const signOut = async (): Promise<void> => {
		setBusy(true); setMessage(null);
		try {
			const { logoutClientFleetAccount } = await import('../lib/clientFleetAccount');
			await logoutClientFleetAccount();
			setStatus({ signedIn: false }); setMessage('Signed out. Saved pairings stay on this phone.');
		} catch (reason) {
			setMessage(reason instanceof Error ? reason.message : String(reason));
		} finally { setBusy(false); }
	};
	return (
		<section className="connect-account" aria-label="Somewhere fleet sign in">
			<div><strong>{status.signedIn ? status.user?.email ?? 'Signed in to Somewhere' : 'See every machine'}</strong><small>{status.signedIn ? 'Same-account machines connect automatically.' : 'Sign in, or scan/enter an address below.'}</small></div>
			{status.signedIn ? (
				<div className="connect-account-actions"><button type="button" className="btn" disabled={busy} onClick={() => void sync()}>{busy ? 'Loading…' : 'Refresh fleet'}</button><button type="button" className="btn btn-ghost" disabled={busy} onClick={() => void signOut()}>Sign out</button></div>
			) : (
				<div className="connect-account-form">
					<input type="email" value={email} disabled={busy || sent} placeholder="you@example.com" autoComplete="email" onChange={(event) => setEmail(event.currentTarget.value)} />
					{sent ? <input value={code} disabled={busy} placeholder="Six-digit code or link token" autoComplete="one-time-code" onChange={(event) => setCode(event.currentTarget.value)} /> : null}
					<button type="button" className="btn" disabled={busy || (sent ? !code.trim() : !email.trim())} onClick={() => void submit()}>{busy ? 'Waiting…' : sent ? 'Verify' : 'Sign in'}</button>
				</div>
			)}
			{message ? <small className="connect-status" role="status">{message}</small> : null}
		</section>
	);
}

function PairingLinkPanel({ link, busy, checkingId, mobile, onChange, onScan, onConnect }: {
  link: string; busy: boolean; checkingId: string | null; mobile: boolean;
  onChange: (value: string) => void; onScan: () => void; onConnect: () => void;
}): JSX.Element {
  const input = useRef<HTMLInputElement>(null);
  const keepVisible = useCallback((): void => {
    input.current?.scrollIntoView({ block: 'center', inline: 'nearest' });
  }, []);
  useEffect(() => {
    const viewport = window.visualViewport;
    if (!viewport) return undefined;
    const onViewportChange = (): void => {
      if (document.activeElement === input.current) keepVisible();
    };
    viewport.addEventListener('resize', onViewportChange);
    return () => viewport.removeEventListener('resize', onViewportChange);
  }, [keepVisible]);
  return (
    <section className="connect-pair-link" aria-labelledby="pair-link-title">
      <div><span>One-time consent</span><h2 id="pair-link-title">Pair with a code</h2><p>Run <code>sessions pair</code> on the host, then scan or paste.</p></div>
      {mobile ? <button type="button" className="connect-submit connect-scan" disabled={busy} onClick={onScan}>{checkingId === 'pairing-code' ? 'Scanning…' : 'Scan a pairing code'}</button> : null}
      <form onSubmit={(event) => { event.preventDefault(); onConnect(); }}>
        <input ref={input} type="url" inputMode="url" autoComplete="off" placeholder="Paste the Sessions pairing link" value={link} onChange={(event) => onChange(event.currentTarget.value)} onFocus={keepVisible} />
        <button type="submit" className="connect-submit" disabled={busy || !link.trim()}>{checkingId === 'pairing-link' ? 'Connecting…' : 'Connect this device'}</button>
      </form>
    </section>
  );
}
