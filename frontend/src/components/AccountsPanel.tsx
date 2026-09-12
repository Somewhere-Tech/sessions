import { useEffect, useState } from 'react';
import { fetchProfiles, forgetAccount, type AccountProfile } from '../api/sessionsd';
import { accountLabel } from '../lib/accountChoice';
import { serverDisplayName, useServers } from '../lib/servers';
import { ACCOUNT_NAME, AccountLoginState, SigningInCard, useGuidedAccountLogin } from './AccountSignIn';

// Accounts on one computer: a second Claude or ChatGPT subscription, its own
// provider home, its own history. Adding one is a guided login rather than a
// hidden menu item, and removing one is a decision about this list — never
// about somebody's subscription.
//
// "One computer" is a choice, not the machine this window happens to be
// connected to: the switcher here lists the same machines Fleet does, so a
// second subscription can be set up on the computer that needs it.

interface Props {
  profiles: AccountProfile[];
  machineName: string;
  serverId?: string;
  /** Opening the login session is how the person sees the provider's own flow. */
  onOpenSession?: (sessionId: string) => void;
  onReload?: (profiles: AccountProfile[]) => void;
}

type Stage = 'idle' | 'naming';

export function AccountsPanel({ profiles, machineName, serverId, onOpenSession, onReload }: Props): JSX.Element {
  const servers = useServers((state) => state.servers);
  const activeId = useServers((state) => state.activeId);
  // The machine whose accounts the caller already has in hand. Null target
  // means that one, whichever it turns out to be once the store has settled.
  const home = serverId ?? activeId ?? '';
  const [targetId, setTargetId] = useState<string | null>(null);
  const [stage, setStage] = useState<Stage>('idle');
  const viewingHome = targetId === null || targetId === home;
  const elsewhere = useMachineAccounts(targetId ?? '', viewingHome);
  const requestId = viewingHome ? serverId : targetId ?? undefined;
  const target = servers.find((server) => server.id === (targetId ?? home));
  const targetName = viewingHome ? machineName : target ? serverDisplayName(target, true) : 'that computer';
  const login = useGuidedAccountLogin({
    serverId: requestId,
    onOpenSession,
    onReload: viewingHome ? onReload : elsewhere.replace
  });
  const accounts = viewingHome ? profiles : elsewhere.profiles;

  const forget = async (account: AccountProfile): Promise<void> => {
    login.setBusy(true);
    try {
      const answer = await forgetAccount(account.tool, account.name, requestId);
      login.setMessage(`${account.tool}/${account.name} is no longer listed. Its provider home was left at ${answer.home} for you to review.`);
      await login.reload();
    } catch (error) {
      login.setMessage(error instanceof Error ? error.message : 'Sessions could not remove that account.');
    } finally {
      login.setBusy(false);
    }
  };

  return (
    <section className="settings-page accounts-panel">
      <AccountsIntroduction machineName={targetName} />

      <div className="settings-card">
        <h2>{viewingHome ? 'On this computer' : `On ${targetName}`}</h2>
        {servers.length > 1 ? (
          <label className="accounts-machine">
            <span>Computer</span>
            <select
              aria-label="Computer"
              value={targetId ?? home}
              onChange={(event) => { setTargetId(event.currentTarget.value); setStage('idle'); login.setMessage(null); }}
            >
              {servers.map((server) => (
                <option key={server.id} value={server.id}>{serverDisplayName(server, true)}</option>
              ))}
            </select>
          </label>
        ) : null}
        {elsewhere.error ? <p className="settings-message" role="status">{elsewhere.error}</p> : null}
        {elsewhere.loading ? <p role="status">Reading the accounts on {targetName}…</p> : (
          <AccountsList
            profiles={accounts}
            busy={login.busy}
            onSignIn={(account) => void login.startLogin(account)}
            onForget={(account) => void forget(account)}
          />
        )}
        {stage === 'idle' ? (
          <button type="button" className="btn btn-primary" onClick={() => { setStage('naming'); login.setMessage(null); }}>
            Add account
          </button>
        ) : null}
      </div>

      {stage === 'naming' ? (
        <AddAccountForm
          busy={login.busy}
          machineName={viewingHome ? undefined : targetName}
          onCancel={() => { setStage('idle'); login.setMessage(null); }}
          onAdd={(tool, name, label) => {
            if (!ACCOUNT_NAME.test(name)) {
              login.setMessage('Use 1–32 lowercase letters, digits, or hyphens.');
              return;
            }
            void login.addAccount(tool, name, label).then((added) => { if (added) setStage('idle'); });
          }}
        />
      ) : null}

      {login.signingInFor ? (
        <SigningInCard
          account={login.signingInFor}
          busy={login.busy}
          machineName={viewingHome ? undefined : targetName}
          onCheck={() => void login.reload()}
          onDone={login.finishSignIn}
        />
      ) : null}

      {login.message ? <p className="settings-message" role="status">{login.message}</p> : null}
      <AccountsFootnote />
    </section>
  );
}

/** The accounts on another computer, read the same way Fleet reads them. */
function useMachineAccounts(targetId: string, isHome: boolean): {
  profiles: AccountProfile[];
  loading: boolean;
  error: string | null;
  replace: (profiles: AccountProfile[]) => void;
} {
  const [profiles, setProfiles] = useState<AccountProfile[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    if (isHome) {
      setProfiles(null);
      setError(null);
      return;
    }
    const controller = new AbortController();
    setProfiles(null);
    setError(null);
    void fetchProfiles(controller.signal, targetId)
      .then((list) => { if (!controller.signal.aborted) setProfiles(list); })
      .catch((reason) => {
        if (controller.signal.aborted) return;
        setError(reason instanceof Error ? reason.message : 'That computer did not answer.');
        setProfiles([]);
      });
    return () => controller.abort();
  }, [targetId, isHome]);
  return {
    profiles: profiles ?? [],
    loading: !isHome && profiles === null && error === null,
    error,
    replace: setProfiles
  };
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
          <AccountLoginState account={account} />
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
  { busy, machineName, onAdd, onCancel }: {
    busy: boolean;
    machineName?: string;
    onAdd: (tool: 'claude' | 'codex', name: string, label: string) => void;
    onCancel: () => void;
  }
): JSX.Element {
  const [tool, setTool] = useState<'claude' | 'codex'>('claude');
  const [name, setName] = useState('');
  const [label, setLabel] = useState('');
  return (
    <div className="settings-card accounts-add">
      <h2>Add an account{machineName ? ` on ${machineName}` : ''}</h2>
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
