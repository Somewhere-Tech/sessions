import { useState } from 'react';
import { createAccount, createSession, fetchProfiles, forgetAccount, type AccountProfile } from '../api/sessionsd';
import { accountLabel } from '../lib/accountChoice';

// Accounts on one computer: a second Claude or ChatGPT subscription, its own
// provider home, its own history. Adding one is a guided login rather than a
// hidden menu item, and removing one is a decision about this list — never
// about somebody's subscription.

const ACCOUNT_NAME = /^[a-z0-9-]{1,32}$/;

interface Props {
  profiles: AccountProfile[];
  machineName: string;
  serverId?: string;
  /** Opening the login session is how the person sees the provider's own flow. */
  onOpenSession?: (sessionId: string) => void;
  onReload?: (profiles: AccountProfile[]) => void;
}

type Stage = 'idle' | 'naming' | 'signing-in';

/** The provider's own sign-in, in that account's home, in a visible session. */
async function openProviderLogin(account: AccountProfile, serverId?: string): Promise<string> {
  const info = await createSession({
    cmd: account.tool === 'codex' ? 'codex' : 'claude',
    args: account.tool === 'codex' ? ['login'] : [],
    profile: account.name,
    name: `Sign in: ${accountLabel(account)}`,
    description: `Provider login for the ${account.tool} account ${account.name}`
  }, serverId);
  return info.id;
}

export function AccountsPanel({ profiles, machineName, serverId, onOpenSession, onReload }: Props): JSX.Element {
  const [stage, setStage] = useState<Stage>('idle');
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<string | null>(null);
  const [signInFor, setSignInFor] = useState<AccountProfile | null>(null);

  const reload = async (): Promise<void> => {
    try {
      onReload?.(await fetchProfiles(undefined, serverId));
    } catch { /* the list is refreshed by the next visit */ }
  };

  // One login session per click, and a failure that says so. A rejected create
  // used to leave an unhandled promise and a panel stuck on "finish signing in"
  // for a session that was never opened.
  const startLogin = async (account: AccountProfile): Promise<void> => {
    if (busy) return;
    setBusy(true);
    setMessage(null);
    try {
      const sessionId = await openProviderLogin(account, serverId);
      setSignInFor(account);
      setStage('signing-in');
      onOpenSession?.(sessionId);
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Sessions could not open the provider sign-in.');
      setSignInFor(null);
      setStage('idle');
    } finally {
      setBusy(false);
    }
  };

  const addAccount = async (tool: 'claude' | 'codex', name: string, label: string): Promise<void> => {
    if (busy) return;
    let account: AccountProfile;
    setBusy(true);
    setMessage(null);
    try {
      account = await createAccount(tool, name, label, serverId);
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Sessions could not add that account.');
      setStage('naming');
      return;
    } finally {
      setBusy(false);
    }
    await startLogin(account);
    await reload();
  };

  const forget = async (account: AccountProfile): Promise<void> => {
    setBusy(true);
    try {
      const answer = await forgetAccount(account.tool, account.name, serverId);
      setMessage(`${account.tool}/${account.name} is no longer listed. Its provider home was left at ${answer.home} for you to review.`);
      await reload();
    } catch (error) {
      setMessage(error instanceof Error ? error.message : 'Sessions could not remove that account.');
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="settings-page accounts-panel">
      <AccountsIntroduction machineName={machineName} />

      <div className="settings-card">
        <h2>On this computer</h2>
        <AccountsList
          profiles={profiles}
          busy={busy}
          onSignIn={(account) => void startLogin(account)}
          onForget={(account) => void forget(account)}
        />
        {stage === 'idle' ? (
          <button type="button" className="btn btn-primary" onClick={() => { setStage('naming'); setMessage(null); }}>
            Add account
          </button>
        ) : null}
      </div>

      {stage === 'naming' ? (
        <AddAccountForm
          busy={busy}
          onCancel={() => { setStage('idle'); setMessage(null); }}
          onAdd={(tool, name, label) => {
            if (!ACCOUNT_NAME.test(name)) {
              setMessage('Use 1–32 lowercase letters, digits, or hyphens.');
              return;
            }
            void addAccount(tool, name, label);
          }}
        />
      ) : null}

      {stage === 'signing-in' && signInFor ? (
        <SigningInCard
          account={signInFor}
          busy={busy}
          onCheck={() => void reload()}
          onDone={() => { setStage('idle'); setSignInFor(null); }}
        />
      ) : null}

      {message ? <p className="settings-message" role="status">{message}</p> : null}
      <AccountsFootnote />
    </section>
  );
}

function AccountsFootnote(): JSX.Element {
  return (
    <p className="field-help">
      Removing an account only takes it off this list. Its provider home — the login and the history —
      is left in place for you to review or delete yourself.
    </p>
  );
}

function AccountsIntroduction({ machineName }: { machineName: string }): JSX.Element {
  return (
    <>
      <span className="settings-kicker">Subscriptions on {machineName}</span>
      <h1>Accounts</h1>
      <p>
        Each account is a separate Claude or ChatGPT login with its own history. Sessions never reads a
        credential: an account has the name you give it, and Sessions reports only whether the file a
        provider writes when it signs in is present in that account&rsquo;s home — not whether that login
        still works, and not which account it belongs to.
      </p>
    </>
  );
}

function AccountsList(
  { profiles, busy, onSignIn, onForget }: {
    profiles: AccountProfile[];
    busy: boolean;
    onSignIn: (account: AccountProfile) => void;
    onForget: (account: AccountProfile) => void;
  }
): JSX.Element {
  return (
    <div className="settings-profile-list accounts-list">
      {profiles.map((account) => (
        <div key={`${account.tool}:${account.name}`} className="accounts-row">
          <span className={`profile-provider is-${account.tool}`}>{account.tool === 'claude' ? 'Claude' : 'Codex'}</span>
          <strong>{accountLabel(account)}</strong>
          <small className="accounts-name">{account.name}</small>
          <small
            className={account.signed_in ? 'accounts-ready' : 'accounts-pending'}
            title={account.signed_in
              ? 'This account\u2019s home holds the file the provider writes when it signs in. Sessions does not open it, so it cannot tell you whether that login still works.'
              : 'Nothing in this account\u2019s home looks like a provider login yet. A provider that keeps its credential in the system keychain also reads this way.'}
          >
            {account.signed_in ? 'Login file present' : 'No login file yet'}
          </small>
          <small>{account.sessions.length} active session{account.sessions.length === 1 ? '' : 's'}</small>
          <small>{account.last_used > 0 ? `Last used ${new Date(account.last_used).toLocaleDateString()}` : 'Never used'}</small>
          {account.signed_in ? null : (
            <button type="button" className="btn btn-ghost" disabled={busy} onClick={() => onSignIn(account)}>Sign in</button>
          )}
          <button type="button" className="btn btn-ghost" disabled={busy} onClick={() => onForget(account)}>Remove</button>
        </div>
      ))}
      {profiles.length === 0 ? <p>No second account on this computer yet.</p> : null}
    </div>
  );
}

function AddAccountForm(
  { busy, onAdd, onCancel }: {
    busy: boolean;
    onAdd: (tool: 'claude' | 'codex', name: string, label: string) => void;
    onCancel: () => void;
  }
): JSX.Element {
  const [tool, setTool] = useState<'claude' | 'codex'>('claude');
  const [name, setName] = useState('');
  const [label, setLabel] = useState('');
  return (
    <div className="settings-card accounts-add">
      <h2>Add an account</h2>
      <label>
        <span>Provider</span>
        <select value={tool} onChange={(event) => setTool(event.currentTarget.value as 'claude' | 'codex')} aria-label="Provider">
          <option value="claude">Claude</option>
          <option value="codex">Codex</option>
        </select>
      </label>
      <label>
        <span>Name on this computer</span>
        <input
          value={name}
          onChange={(event) => setName(event.currentTarget.value.toLowerCase())}
          placeholder="work or personal"
          maxLength={32}
          aria-label="Account name"
        />
      </label>
      <label>
        <span>Label</span>
        <input
          value={label}
          onChange={(event) => setLabel(event.currentTarget.value)}
          placeholder="Work — team plan"
          maxLength={64}
          aria-label="Account label"
        />
      </label>
      <p className="field-help">
        Sessions opens the provider&rsquo;s own sign-in in this account&rsquo;s home.
        Subscription logins only — there is no API-key path here.
      </p>
      <div className="accounts-add-actions">
        <button type="button" className="btn btn-primary" disabled={busy} onClick={() => onAdd(tool, name.trim().toLowerCase(), label.trim())}>
          {busy ? 'Adding…' : 'Add and sign in'}
        </button>
        <button type="button" className="btn btn-ghost" disabled={busy} onClick={onCancel}>Cancel</button>
      </div>
    </div>
  );
}

function SigningInCard(
  { account, busy, onCheck, onDone }: {
    account: AccountProfile;
    busy: boolean;
    onCheck: () => void;
    onDone: () => void;
  }
): JSX.Element {
  return (
    <div className="settings-card accounts-signing-in" role="status">
      <h2>Finish signing in to {accountLabel(account)}</h2>
      <ol>
        <li>{account.tool === 'claude' ? 'Send /login in the session Sessions just opened.' : 'Choose “Sign in with ChatGPT” in the session Sessions just opened.'}</li>
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
