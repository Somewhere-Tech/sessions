import { useEffect, useState } from 'react';
import { fetchProfiles, forgetAccount, type AccountProfile } from '../api/sessionsd';
import { accountLabel } from '../lib/accountChoice';
import { serverDisplayName, useServers } from '../lib/servers';
import { AccountLoginState, SigningInCard, useGuidedAccountLogin } from './AccountSignIn';

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
  /** Kept for callers shared with other settings panels; sign-in opens no chat. */
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
            void login.addAccount(tool, name, label).then((added) => { if (added) setStage('idle'); });
          }}
        />
      ) : null}

      {login.signingInFor ? (
        <SigningInCard
          account={login.signingInFor}
          operation={login.operation}
          busy={login.busy}
          machineName={viewingHome ? undefined : targetName}
          onCode={login.submitCode}
          onCancel={login.cancel}
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
      Removing an account keeps its saved chats and sign-in on this computer.
    </p>
  );
}

function AccountsIntroduction({ machineName }: { machineName: string }): JSX.Element {
  return (
    <>
      <span className="settings-kicker">Subscriptions on {machineName}</span>
      <h1>Accounts</h1>
      <p>
        Add your Claude and ChatGPT accounts, then choose one when starting a chat.
        Sign-in happens with the provider. Each account stays separate on this computer.
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
          <div className="accounts-identity">
          <span className={`profile-provider is-${account.tool}`}>{account.tool === 'claude' ? 'Claude' : 'Codex'}</span>
          <strong>{accountLabel(account)}</strong>
          <AccountLoginState account={account} />
          </div>
          <div className="accounts-meta">
          <small>{account.sessions.length} active session{account.sessions.length === 1 ? '' : 's'}</small>
          <small>{account.last_used > 0 ? `Last used ${new Date(account.last_used).toLocaleDateString()}` : 'Never used'}</small>
          </div>
          <div className="accounts-actions">
          <button type="button" className="btn btn-ghost" disabled={busy} onClick={() => onSignIn(account)}>
            {account.identity || account.signed_in ? 'Check account' : 'Sign in'}
          </button>
          <button type="button" className="btn btn-ghost" disabled={busy} onClick={() => onForget(account)}>Remove</button>
          </div>
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
  const [label, setLabel] = useState('');
  return (
    <div className="settings-card accounts-add">
      <h2>Add an account{machineName ? ` on ${machineName}` : ''}</h2>
      <label>
        <span>Provider</span>
        <select value={tool} onChange={(event) => setTool(event.currentTarget.value as 'claude' | 'codex')} aria-label="Provider">
          <option value="claude">Claude</option>
          <option value="codex">ChatGPT / Codex</option>
        </select>
      </label>
      <label>
        <span>Nickname (optional)</span>
        <input
          value={label}
          onChange={(event) => setLabel(event.currentTarget.value)}
          placeholder="Work or personal"
          maxLength={64}
          aria-label="Account label"
        />
      </label>
      <p className="field-help">
        We&rsquo;ll show which account connected after you sign in. No API key needed.
      </p>
      <div className="accounts-add-actions">
        <button type="button" className="btn btn-primary" disabled={busy} onClick={() => onAdd(tool, '', label.trim())}>
          {busy ? 'Preparing…' : 'Continue'}
        </button>
        <button type="button" className="btn btn-ghost" disabled={busy} onClick={onCancel}>Cancel</button>
      </div>
    </div>
  );
}
