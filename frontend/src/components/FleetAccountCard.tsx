import { useCallback, useEffect, useState } from 'react';
import {
  fetchFleetAccount,
  logoutFleetAccount,
  requestFleetMagicLink,
  verifyFleetMagicLink,
  type FleetAccountStatus
} from '../api/sessionsd';
import type { ClientFleetStatus } from '../lib/clientFleetAccount';

export function FleetAccountCard({ clientOnly = false }: { clientOnly?: boolean }): JSX.Element {
  const [account, setAccount] = useState<FleetAccountStatus | null>(null);
  const [email, setEmail] = useState('');
  const [code, setCode] = useState('');
  const [sent, setSent] = useState(false);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<string | null>(null);
  const refresh = useCallback(async (): Promise<void> => {
    if (clientOnly) return;
    try {
      setAccount(await fetchFleetAccount());
    } catch (reason) {
      setMessage(reason instanceof Error ? reason.message : String(reason));
    }
  }, [clientOnly]);

  useEffect(() => { void refresh(); }, [refresh]);

  const submit = async (): Promise<void> => {
    if (busy || !email.trim()) return;
    setBusy(true); setMessage(null);
    try {
      await requestFleetMagicLink(email);
      setSent(true);
      setMessage(`Somewhere sent a single-use sign-in code or link to ${email.trim()}.`);
    } catch (reason) {
      setMessage(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setBusy(false);
    }
  };

  const verify = async (): Promise<void> => {
    if (busy || !code.trim()) return;
    setBusy(true); setMessage(null);
    try {
      setAccount(await verifyFleetMagicLink(code));
      setCode(''); setSent(false); setMessage('This machine is registered with your Somewhere fleet.');
    } catch (reason) {
      setMessage(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setBusy(false);
    }
  };

  const signOut = async (): Promise<void> => {
    if (busy) return;
    setBusy(true); setMessage(null);
    try {
      await logoutFleetAccount();
      setAccount({ signed_in: false });
      setMessage('Signed out. Local and Tailscale access is unchanged.');
    } catch (reason) {
      setMessage(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setBusy(false);
    }
  };

	if (clientOnly) return <ClientFleetAccountCard />;
  return (
    <section className="pair-device-card fleet-account-card">
      <div>
        <span className="connections-section-kicker">Optional fleet directory</span>
        <h2>Somewhere account</h2>
        <p>Sign in to register this machine across your devices. Session content and provider credentials stay on this computer.</p>
      </div>
      {account?.signed_in ? (
        <div className="fleet-account-signed-in">
          <div><strong>{account.user?.display_name || account.user?.email || 'Signed in'}</strong><small>{account.user?.display_name ? account.user.email : 'Somewhere fleet account'}</small></div>
          <button type="button" className="btn btn-ghost" disabled={busy} onClick={() => void signOut()}>{busy ? 'Signing out…' : 'Sign out'}</button>
          {account.last_registration_at ? <small>Machine registered {new Date(account.last_registration_at).toLocaleString()}</small> : null}
          {account.last_registration_error ? <small>Registration pending: {account.last_registration_error}</small> : null}
        </div>
      ) : (
        <div className="fleet-account-form">
          <input type="email" value={email} disabled={busy || sent} placeholder="you@example.com" autoComplete="email" onChange={(event) => setEmail(event.currentTarget.value)} />
          {sent ? <input value={code} disabled={busy} placeholder="Six-digit code or link token" autoComplete="one-time-code" onChange={(event) => setCode(event.currentTarget.value)} onKeyDown={(event) => { if (event.key === 'Enter') void verify(); }} /> : null}
          <button type="button" className="btn" disabled={busy || (sent ? !code.trim() : !email.trim())} onClick={() => void (sent ? verify() : submit())}>{busy ? 'Waiting…' : sent ? 'Verify code' : 'Sign in to Somewhere'}</button>
        </div>
      )}
      {message ? <div className="connections-message" role="status">{message}</div> : null}
      <small>Skip it if you only need this network or your own tailnet.</small>
    </section>
  );
}

function ClientFleetAccountCard(): JSX.Element {
	const [status, setStatus] = useState<ClientFleetStatus>({ signedIn: false });
	const [email, setEmail] = useState('');
	const [code, setCode] = useState('');
	const [sent, setSent] = useState(false);
	const [busy, setBusy] = useState(false);
	const [message, setMessage] = useState<string | null>(null);
	useEffect(() => { void import('../lib/clientFleetAccount').then((client) => setStatus(client.clientFleetStatus())); }, []);
	const submit = async (): Promise<void> => {
		setBusy(true); setMessage(null);
		try {
			const client = await import('../lib/clientFleetAccount');
			if (!sent) {
				await client.requestClientFleetMagicLink(email); setSent(true);
				setMessage(`Somewhere sent a single-use code or link to ${email.trim()}.`);
			} else {
				setStatus(await client.verifyClientFleetMagicLink(code)); setSent(false); setCode('');
				const errors = await client.syncClientAccountFleet();
				setMessage(errors.length > 0 ? errors.join(' · ') : 'Your account fleet is ready.');
			}
		} catch (reason) {
			setMessage(reason instanceof Error ? reason.message : String(reason));
		} finally { setBusy(false); }
	};
	const signOut = async (): Promise<void> => {
		setBusy(true); setMessage(null);
		try {
			const { logoutClientFleetAccount } = await import('../lib/clientFleetAccount');
			await logoutClientFleetAccount(); setStatus({ signedIn: false });
			setMessage('Signed out. Saved pairings stay on this phone.');
		} catch (reason) {
			setMessage(reason instanceof Error ? reason.message : String(reason));
		} finally { setBusy(false); }
	};
	return <section className="pair-device-card fleet-account-card">
		<div><span className="connections-section-kicker">Optional fleet directory</span><h2>Somewhere account</h2><p>Sign this phone in to see and connect to every machine in the same account without pairing each one.</p></div>
		{status.signedIn ? <div className="fleet-account-signed-in"><strong>{status.user?.display_name || status.user?.email || 'Signed in'}</strong><button type="button" className="btn btn-ghost" disabled={busy} onClick={() => void signOut()}>Sign out</button></div> : <div className="fleet-account-form"><input type="email" value={email} disabled={busy || sent} placeholder="you@example.com" autoComplete="email" onChange={(event) => setEmail(event.currentTarget.value)} />{sent ? <input value={code} disabled={busy} placeholder="Six-digit code or link token" autoComplete="one-time-code" onChange={(event) => setCode(event.currentTarget.value)} /> : null}<button type="button" className="btn" disabled={busy || (sent ? !code.trim() : !email.trim())} onClick={() => void submit()}>{busy ? 'Waiting…' : sent ? 'Verify code' : 'Sign in to Somewhere'}</button></div>}
		{message ? <div className="connections-message" role="status">{message}</div> : null}
	</section>;
}
