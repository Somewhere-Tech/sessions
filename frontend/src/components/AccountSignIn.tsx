import { useCallback, useEffect, useRef, useState } from 'react';
import { createAccount, fetchProfiles, type AccountProfile } from '../api/sessionsd';
import { accountLoginRequest, type AccountLogin } from '../api/sessionsd/accountLogin';
import { DaemonResponseError } from '../api/sessionsd/core';
import { openExternalURL } from '../lib/tauriBridge';
import { DeviceSignInHelp } from './AddAccountDialog';
import '../styles/accounts.css';

export const loginPending = (operation: AccountLogin | null): boolean => operation?.state === 'opening' || operation?.state === 'waiting';

export function useGuidedAccountLogin({ serverId, onReload }: {
  serverId?: string;
  onOpenSession?: (sessionId: string) => void;
  onReload?: (profiles: AccountProfile[]) => void;
}) {
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<string | null>(null);
  const [signingInFor, setSigningInFor] = useState<AccountProfile | null>(null);
  const [operation, setOperation] = useState<AccountLogin | null>(null);
  const lock = useRef(false);
  const currentHost = useRef(serverId);
  currentHost.current = serverId;
  const reload = useCallback(async (): Promise<void> => {
    try {
      const profiles = await fetchProfiles(undefined, serverId);
      if (currentHost.current === serverId) onReload?.(profiles);
    } catch (error) {
      if (currentHost.current === serverId) setMessage(error instanceof Error ? error.message : 'Could not refresh accounts.');
    }
  }, [serverId, onReload]);
  const refreshRef = useRef(reload);
  refreshRef.current = reload;

  useEffect(() => { setOperation(null); setSigningInFor(null); setMessage(null); }, [serverId]);
  useEffect(() => {
    if (!operation?.id) return;
    let active = true;
    let timer: ReturnType<typeof setTimeout>;
    const poll = async (): Promise<void> => {
      try {
        const next = await accountLoginRequest(serverId, operation.id);
        if (!active) return;
        setMessage(null);
        if (next.state === 'connected') {
          await refreshRef.current();
          if (active) setOperation(next);
          return;
        }
        setOperation(next);
        if (!loginPending(next)) return;
      } catch (error) {
        if (!active) return;
        if (error instanceof DaemonResponseError && error.status < 500) {
          setOperation((current) => current ? { ...current, state: 'failed', message: error.detail, url: undefined, code: undefined } : null);
          return;
        }
        setMessage('Waiting for this computer to reconnect. Your sign-in has not been restarted.');
      }
      if (active) timer = setTimeout(() => void poll(), 1500);
    };
    void poll();
    return () => { active = false; clearTimeout(timer); };
  }, [operation?.id, serverId]);

  const startLogin = async (account: AccountProfile): Promise<boolean> => {
    if (lock.current) return false;
    lock.current = true;
    setBusy(true); setMessage(null);
    try {
      const next = await accountLoginRequest(serverId, '', 'POST', { tool: account.tool, profile: account.name });
      if (currentHost.current !== serverId) return false;
      setSigningInFor(account); setOperation(next);
      return true;
    } catch (error) { setMessage(error instanceof Error ? error.message : 'Could not start sign-in.'); return false; }
    finally { lock.current = false; setBusy(false); }
  };

  const addAccount = async (tool: 'claude' | 'codex', name: string, label: string): Promise<boolean> => {
    if (lock.current) return false;
    lock.current = true; setBusy(true); setMessage(null);
    let account: AccountProfile;
    try { account = await createAccount(tool, name, label, serverId); }
    catch (error) { setMessage(error instanceof Error ? error.message : 'Could not add account.'); return false; }
    finally { lock.current = false; setBusy(false); }
    if (currentHost.current !== serverId) return false;
    const started = await startLogin(account);
    await reload();
    return started;
  };

  const act = async (cancel: boolean, code?: string): Promise<void> => {
    if (!operation) return;
    setBusy(true);
    try {
      const next = await accountLoginRequest(serverId, operation.id, cancel ? 'DELETE' : 'POST', code ? { code } : undefined);
      if (currentHost.current !== serverId) return;
      if (next.state === 'connected') await reload();
      setOperation(next);
    }
    catch (error) { setMessage(error instanceof Error ? error.message : 'Could not update sign-in.'); }
    finally { setBusy(false); }
  };
  return {
    busy, message, signingInFor, operation, setMessage, setBusy, startLogin, addAccount, reload,
    submitCode: (code: string) => act(false, code), cancel: () => act(true),
    finishSignIn: () => { setSigningInFor(null); setOperation(null); }
  };
}

export function AccountLoginState({ account }: { account: AccountProfile }): JSX.Element {
  return <small className={account.identity ? 'accounts-ready' : 'accounts-pending'}
    title={account.identity ? `Identity checked ${new Date(account.identity.checked_at).toLocaleString()}. This does not check remaining usage.` : 'Check this account to confirm who is signed in.'}>
    {account.identity ? `${account.identity.email}${account.identity.plan ? ` · ${account.identity.plan}` : ''}` : 'Identity not checked'}
  </small>;
}

export function SigningInCard({ account, operation, busy, machineName, onCode, onCancel, onDone }: {
  account: AccountProfile;
  operation: AccountLogin | null;
  busy: boolean;
  machineName?: string;
  onCode: (code: string) => Promise<void>;
  onCancel: () => Promise<void>;
  onDone: () => void;
}): JSX.Element {
  const [code, setCode] = useState('');
  const [error, setError] = useState<string | null>(null);
  const provider = account.tool === 'codex' ? 'ChatGPT' : 'Claude';
  const connected = operation?.state === 'connected';
  return <div className="settings-card accounts-signing-in" role="region" aria-label="Sign in to account">
    <h2>{connected ? 'Account connected' : `Sign in to ${provider}`}{machineName ? ` on ${machineName}` : ''}</h2>
    {operation?.state === 'opening' ? <p role="status">Preparing secure sign-in…</p> : null}
    {operation?.state === 'waiting' ? <>
      <p>Sign in in your browser. Check that you choose the account you want to add; your other accounts stay signed in.</p>
      {operation.code ? <p>Enter this code on the provider’s page: <strong className="account-device-code">{operation.code}</strong></p> : null}
      {account.tool === 'codex' ? <DeviceSignInHelp /> : null}
      <button className="btn btn-primary" type="button" onClick={() => {
        if (operation.url) void openExternalURL(operation.url).catch(() => setError('Could not open your browser. Use the sign-in link below.'));
      }}>Continue to {provider}</button>
      {error && operation.url ? <p><a href={operation.url} target="_blank" rel="noreferrer">Open provider sign-in</a></p> : null}
      {account.tool === 'claude' ? <div className="account-confirmation">
        <label>Paste the confirmation code from Claude
          <input type="password" autoComplete="off" value={code} maxLength={2048} onChange={(event) => setCode(event.currentTarget.value)}
            onKeyDown={(event) => { if (event.key === 'Enter') { event.preventDefault(); event.stopPropagation(); if (code.trim() && !busy) void onCode(code.trim()).then(() => setCode('')); } }}
            aria-label="Claude confirmation code" />
        </label>
        <button type="button" className="btn btn-primary" disabled={busy || !code.trim()} onClick={() => void onCode(code.trim()).then(() => setCode(''))}>Connect account</button>
      </div> : <p role="status">Sessions will confirm your account automatically when sign-in finishes.</p>}
    </> : null}
    {connected ? <>
      <p><strong>{operation.identity?.email}</strong>{operation.identity?.plan ? ` · ${operation.identity.plan}` : ''}</p>
      <p>You can now choose this account when starting a chat on this computer.</p>
      <p className="field-help">Wrong account? Add another account and choose a different login in the provider’s browser page.</p>
    </> : null}
    {operation?.message ? <p role="status">{operation.message}</p> : null}
    {error ? <p role="alert">{error}</p> : null}
    <button className="btn btn-ghost" type="button" disabled={busy} onClick={() => { if (loginPending(operation)) void onCancel(); else onDone(); }}>
      {loginPending(operation) ? 'Cancel sign-in' : connected ? 'Done' : 'Close'}
    </button>
  </div>;
}
