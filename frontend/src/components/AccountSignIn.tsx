import { useState } from 'react';
import { createAccount, createSession, fetchProfiles, type AccountProfile } from '../api/sessionsd';
import { accountLabel } from '../lib/accountChoice';

// Adding an account is one flow, wherever it is started from. Settings runs it
// for the computer being looked at and Fleet runs it for any other computer in
// the list, so "Log in here too" has to do exactly what "Add account" does:
// register the home on that machine, open the provider's own sign-in there, and
// say to check the account in the browser before confirming.

export const ACCOUNT_NAME = /^[a-z0-9-]{1,32}$/;

/** The provider's own sign-in, in that account's home, in a visible session. */
export async function openProviderLogin(account: AccountProfile, serverId?: string): Promise<string> {
  const info = await createSession({
    cmd: account.tool === 'codex' ? 'codex' : 'claude',
    args: account.tool === 'codex' ? ['login'] : [],
    profile: account.name,
    name: `Sign in: ${accountLabel(account)}`,
    description: `Provider login for the ${account.tool} account ${account.name}`
  }, serverId);
  return info.id;
}

export interface GuidedLogin {
  busy: boolean;
  message: string | null;
  /** The account whose sign-in is open, so the steps can be shown beside it. */
  signingInFor: AccountProfile | null;
  setMessage: (value: string | null) => void;
  /** For a caller that runs its own request against the same account list. */
  setBusy: (value: boolean) => void;
  startLogin: (account: AccountProfile) => Promise<boolean>;
  addAccount: (tool: 'claude' | 'codex', name: string, label: string) => Promise<boolean>;
  finishSignIn: () => void;
  reload: () => Promise<void>;
}

/**
 * The add-and-sign-in state machine. `serverId` is the computer this runs on:
 * undefined means the one the app is connected to, and any other id sends every
 * request to that machine instead.
 */
export function useGuidedAccountLogin(
  { serverId, onOpenSession, onReload }: {
    serverId?: string;
    onOpenSession?: (sessionId: string) => void;
    onReload?: (profiles: AccountProfile[]) => void;
  }
): GuidedLogin {
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<string | null>(null);
  const [signingInFor, setSigningInFor] = useState<AccountProfile | null>(null);

  const reload = async (): Promise<void> => {
    try {
      onReload?.(await fetchProfiles(undefined, serverId));
    } catch { /* the list is refreshed by the next visit */ }
  };

  // One login session per click, and a failure that says so. A rejected create
  // used to leave an unhandled promise and a panel stuck on "finish signing in"
  // for a session that was never opened.
  const startLogin = async (account: AccountProfile): Promise<boolean> => {
    if (busy) return false;
    setBusy(true);
    setMessage(null);
    try {
      const sessionId = await openProviderLogin(account, serverId);
      setSigningInFor(account);
      onOpenSession?.(sessionId);
      return true;
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Sessions could not open the provider sign-in.');
      setSigningInFor(null);
      return false;
    } finally {
      setBusy(false);
    }
  };

  const addAccount = async (tool: 'claude' | 'codex', name: string, label: string): Promise<boolean> => {
    if (busy) return false;
    let account: AccountProfile;
    setBusy(true);
    setMessage(null);
    try {
      account = await createAccount(tool, name, label, serverId);
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Sessions could not add that account.');
      return false;
    } finally {
      setBusy(false);
    }
    await startLogin(account);
    await reload();
    return true;
  };

  return {
    busy, message, signingInFor, setMessage, setBusy, startLogin, addAccount, reload,
    finishSignIn: () => setSigningInFor(null)
  };
}

/**
 * What Sessions can actually see about an account: whether the file a provider
 * writes at sign-in is present in its home. Never "signed in", which would
 * claim a working login nothing here has checked.
 */
export function AccountLoginState({ account }: { account: AccountProfile }): JSX.Element {
  return (
    <small
      className={account.signed_in ? 'accounts-ready' : 'accounts-pending'}
      title={account.signed_in
        ? 'This account’s home holds the file the provider writes when it signs in. Sessions does not open it, so it cannot tell you whether that login still works.'
        : 'Nothing in this account’s home looks like a provider login yet. A provider that keeps its credential in the system keychain also reads this way.'}
    >
      {account.signed_in ? 'Login file present' : 'No login file yet'}
    </small>
  );
}

export function SigningInCard(
  { account, busy, machineName, onCheck, onDone }: {
    account: AccountProfile;
    busy: boolean;
    /** Named when the sign-in was opened somewhere other than here. */
    machineName?: string;
    onCheck: () => void;
    onDone: () => void;
  }
): JSX.Element {
  const where = machineName ? ` on ${machineName}` : '';
  return (
    <div className="settings-card accounts-signing-in" role="status">
      <h2>Finish signing in to {accountLabel(account)}</h2>
      <ol>
        <li>{account.tool === 'claude'
          ? `Send /login in the session Sessions just opened${where}.`
          : `Choose “Sign in with ChatGPT” in the session Sessions just opened${where}.`}</li>
        <li>Open the link it prints and <strong>check which account you are signing in as</strong> in the browser.</li>
        <li>Come back here; the account shows its login file once the provider has written one.</li>
      </ol>
      <div className="accounts-add-actions">
        <button type="button" className="btn btn-ghost" disabled={busy} onClick={onCheck}>Check again</button>
        <button type="button" className="btn btn-ghost" onClick={onDone}>Done</button>
      </div>
    </div>
  );
}
