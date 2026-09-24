import { useEffect, useId, useState, type FormEvent } from 'react';
import { fetchProfiles, forgetAccount, renameAccount, type AccountProfile } from '../api/sessionsd';
import { serverDisplayName, useServers } from '../lib/servers';
import { loginPending, SigningInCard, useGuidedAccountLogin } from './AccountSignIn';
import { ProviderMark } from './ProviderBadge';

// Accounts on one computer: a second Claude or ChatGPT subscription, its own
// provider home, its own history. Adding one is a guided login rather than a
// hidden menu item, and removing one is a decision about this list — never
// about somebody's subscription.
//
// "One computer" is a choice, not the machine this window happens to be
// connected to: the switcher here lists the same machines Fleet does, so a
// second subscription can be set up on the computer that needs it.
//
// One flow at a time: the add form and the sign-in card never share the page,
// so a finished sign-in is not left above a new, unrelated form.

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
  const signingIn = login.signingInFor !== null;
  const signInPending = signingIn && loginPending(login.operation);

  const startAdding = (): void => {
    // A finished or failed sign-in card is closed, not stacked above the form.
    if (signingIn) login.finishSignIn();
    login.setMessage(null);
    setStage('naming');
  };
  const signIn = (account: AccountProfile): void => {
    setStage('idle');
    void login.startLogin(account);
  };

  const { forget, rename } = accountEdits(login, requestId);

  return (
    <section className="settings-page accounts-panel">
      <AccountsIntroduction />

      <div className="settings-card accounts-card">
        <div className="accounts-card-head">
          <h2>Accounts added in Sessions</h2>
          <ComputerPicker
            value={targetId ?? home}
            machineName={targetName}
            onChange={(id) => {
              if (signingIn) login.finishSignIn();
              setTargetId(id); setStage('idle'); login.setMessage(null);
            }}
          />
        </div>
        {elsewhere.error ? <p className="settings-message" role="status">{elsewhere.error}</p> : null}
        {elsewhere.loading ? <p role="status">Reading the accounts on {targetName}…</p> : (
          <AccountsList
            key={requestId ?? home}
            profiles={accounts}
            machineName={targetName}
            busy={login.busy || signInPending}
            onSignIn={signIn}
            onRename={rename}
            onForget={(account) => void forget(account)}
          />
        )}
        {stage === 'idle' && !signInPending ? (
          <button type="button" className="btn btn-secondary accounts-add-button" onClick={startAdding}>
            Add account
          </button>
        ) : null}
      </div>

      {stage === 'naming' && !signingIn ? (
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

type GuidedLogin = ReturnType<typeof useGuidedAccountLogin>;

/** Remove and rename, reported through the same busy state as sign-in. */
function accountEdits(login: GuidedLogin, requestId: string | undefined) {
  const forget = async (account: AccountProfile): Promise<void> => {
    login.setBusy(true);
    try {
      await forgetAccount(account.tool, account.name, requestId);
      login.setMessage(`${accountTitle(account)} was removed from this list. Its saved chats and sign-in are unchanged.`);
      await login.reload();
    } catch (error) {
      login.setMessage(error instanceof Error ? error.message : 'Sessions could not remove that account.');
    } finally {
      login.setBusy(false);
    }
  };
  // A refused rename is answered in its row, where the person is still typing.
  const rename = async (account: AccountProfile, label: string): Promise<string | null> => {
    login.setBusy(true);
    try {
      await renameAccount(account.tool, account.name, label, requestId);
      await login.reload();
      return null;
    } catch (error) {
      return error instanceof Error ? error.message : 'Sessions could not rename that account.';
    } finally {
      login.setBusy(false);
    }
  };
  return { forget, rename };
}

function ComputerPicker(
  { value, machineName, onChange }: { value: string; machineName: string; onChange: (id: string) => void }
): JSX.Element {
  const servers = useServers((state) => state.servers);
  if (servers.length <= 1) return <span className="accounts-machine-name">{machineName}</span>;
  return (
    <label className="accounts-machine">
      <span>Computer</span>
      <select aria-label="Computer" value={value} onChange={(event) => onChange(event.currentTarget.value)}>
        {servers.map((server) => (
          <option key={server.id} value={server.id}>{serverDisplayName(server, true)}</option>
        ))}
      </select>
    </label>
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

function AccountsIntroduction(): JSX.Element {
  return (
    <header className="accounts-intro">
      <h1>Accounts</h1>
      <p>
        Choose which account your agents use. Each sign-in stays separate on its computer.
      </p>
    </header>
  );
}

const providerName = (account: AccountProfile): string => account.tool === 'codex' ? 'ChatGPT' : 'Claude';

/** The one name a row leads with: the nickname, then the verified email. */
function accountTitle(account: AccountProfile): string {
  return account.label?.trim() || account.identity?.email || `${providerName(account)} account`;
}

function AccountsList(
  { profiles, machineName, busy, onSignIn, onRename, onForget }: {
    profiles: AccountProfile[];
    machineName: string;
    busy: boolean;
    onSignIn: (account: AccountProfile) => void;
    onRename: (account: AccountProfile, label: string) => Promise<string | null>;
    onForget: (account: AccountProfile) => void;
  }
): JSX.Element {
  if (profiles.length === 0) {
    return <p className="accounts-empty">No second account on {machineName} yet.</p>;
  }
  return (
    <ul className="accounts-list" aria-label={`Accounts on ${machineName}`}>
      {profiles.map((account) => (
        <AccountRow
          key={`${account.tool}:${account.name}`}
          account={account}
          busy={busy}
          onSignIn={onSignIn}
          onRename={onRename}
          onForget={onForget}
        />
      ))}
    </ul>
  );
}

function AccountRow(
  { account, busy, onSignIn, onRename, onForget }: {
    account: AccountProfile;
    busy: boolean;
    onSignIn: (account: AccountProfile) => void;
    onRename: (account: AccountProfile, label: string) => Promise<string | null>;
    onForget: (account: AccountProfile) => void;
  }
): JSX.Element {
  const titleId = useId();
  const [editing, setEditing] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const title = accountTitle(account);
  const nickname = account.label?.trim();
  // The email is secondary only when a nickname leads; otherwise it already is
  // the title, and the provider says what kind of account this is.
  const details = [
    providerName(account),
    nickname && account.identity?.email ? account.identity.email : null,
    account.identity?.plan ? `${account.identity.plan} plan` : null
  ].filter(Boolean).join(' · ');
  const activity = [
    account.sessions.length > 0 ? `${account.sessions.length} active` : null,
    account.last_used > 0 ? `Last used ${new Date(account.last_used).toLocaleDateString()}` : null
  ].filter(Boolean).join(' · ');

  return (
    <li className="accounts-row">
      <ProviderMark provider={account.tool} size={32} />
      <div className="accounts-identity">
        {editing ? (
          <NicknameEditor
            account={account}
            title={title}
            busy={busy}
            onCancel={() => { setEditing(false); setError(null); }}
            onSave={async (label) => {
              const failure = await onRename(account, label);
              setError(failure);
              if (!failure) setEditing(false);
            }}
          />
        ) : <strong id={titleId}>{title}</strong>}
        <span className="accounts-details">{details}</span>
        {activity ? <span className="accounts-activity">{activity}</span> : null}
        {error ? <span className="accounts-row-error" role="alert">{error}</span> : null}
      </div>
      <AccountStatus account={account} />
      {!editing ? (
        <div className="accounts-actions" role="group" aria-labelledby={titleId}>
          <button type="button" className="btn btn-ghost" disabled={busy} aria-describedby={titleId} onClick={() => onSignIn(account)}>
            {account.identity ? 'Check account' : 'Sign in'}
          </button>
          <button type="button" className="btn btn-ghost" disabled={busy} aria-describedby={titleId} onClick={() => { setError(null); setEditing(true); }}>
            Rename
          </button>
          <button type="button" className="btn btn-ghost accounts-remove" disabled={busy} aria-describedby={titleId} onClick={() => onForget(account)}>
            Remove
          </button>
        </div>
      ) : null}
    </li>
  );
}

function NicknameEditor(
  { account, title, busy, onSave, onCancel }: {
    account: AccountProfile;
    title: string;
    busy: boolean;
    onSave: (label: string) => Promise<void>;
    onCancel: () => void;
  }
): JSX.Element {
  const [draft, setDraft] = useState(account.label ?? '');
  const submit = (event: FormEvent): void => {
    event.preventDefault();
    if (!busy) void onSave(draft.trim());
  };
  return (
    <form className="accounts-rename" onSubmit={submit}>
      <input
        value={draft}
        onChange={(event) => setDraft(event.currentTarget.value)}
        onKeyDown={(event) => { if (event.key === 'Escape') { event.preventDefault(); onCancel(); } }}
        maxLength={64}
        placeholder={account.identity?.email ?? 'Work or personal'}
        aria-label={`Nickname for ${title}`}
        autoFocus
      />
      <button type="submit" className="btn btn-primary" disabled={busy}>Save</button>
      <button type="button" className="btn btn-ghost" disabled={busy} onClick={onCancel}>Cancel</button>
    </form>
  );
}

/**
 * What Sessions actually knows about the sign-in. A provider-reported identity
 * is a check that happened; a login file on disk is only a file, so it never
 * reads as ready.
 */
function AccountStatus({ account }: { account: AccountProfile }): JSX.Element {
  if (account.identity) {
    return (
      <span
        className="accounts-status is-verified"
        title={`The provider reported this account on ${new Date(account.identity.checked_at).toLocaleString()}. This does not check remaining usage.`}
      >
        Verified {new Date(account.identity.checked_at).toLocaleDateString()}
      </span>
    );
  }
  return (
    <span
      className="accounts-status"
      title={account.signed_in
        ? 'A provider login file is present, but Sessions has not confirmed who is signed in. Check this account to confirm who is signed in.'
        : 'Check this account to confirm who is signed in.'}
    >
      Identity not checked
    </span>
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
    <form
      className="settings-card accounts-add"
      aria-label="Add an account"
      onSubmit={(event) => { event.preventDefault(); if (!busy) onAdd(tool, '', label.trim()); }}
    >
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
        <button type="submit" className="btn btn-primary" disabled={busy}>
          {busy ? 'Preparing…' : 'Continue'}
        </button>
        <button type="button" className="btn btn-ghost" disabled={busy} onClick={onCancel}>Cancel</button>
      </div>
    </form>
  );
}
