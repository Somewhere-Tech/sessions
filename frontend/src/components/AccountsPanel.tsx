import { useEffect, useState } from 'react';
import { forgetAccount, renameAccount, type AccountProfile } from '../api/sessionsd';
import { useAccountFleet } from '../hooks/useAccountFleet';
import { computersWithout, coverageNotes, groupTitle, rollUpAccounts, type AccountGroup, type AccountPlacement, type MachineAccounts } from '../lib/accountRollup';
import { useServers } from '../lib/servers';
import { AccountPlacementRow, AddAccountForm, AddOnComputer } from './AccountRowParts';
import { loginPending, SigningInCard, useGuidedAccountLogin } from './AccountSignIn';
import { AccountUsageSummary } from './AccountUsageSummary';
import { ProviderMark } from './ProviderBadge';

// Accounts, one row per account. A subscription's allowance belongs to the
// provider account, so the row leads with who it is and how much of it is
// used, then lists the computers it is signed into. Each computer keeps its own
// sign-in: adding the account to another computer runs the provider's own
// sign-in there, and no credential ever moves between computers.
//
// Two computers' homes are one row only when the provider reported the same
// account ID for both. No supported provider reports one yet, so today each
// computer's home is its own row; a matching email is mentioned, not merged.
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

type GuidedLogin = ReturnType<typeof useGuidedAccountLogin>;

/**
 * The guided sign-in, pointed at whichever computer an action belongs to. The
 * sign-in hook is bound to one computer, so an action on another computer
 * switches the target first and runs once the hook has followed.
 */
function useTargetedLogin(homeId: string, onProfiles: (id: string, profiles: AccountProfile[]) => void) {
  const [target, setTarget] = useState(homeId);
  const [queued, setQueued] = useState<{ serverId: string; run: (login: GuidedLogin) => void } | null>(null);
  const login = useGuidedAccountLogin({
    serverId: target || undefined,
    onReload: (profiles) => onProfiles(target, profiles)
  });
  useEffect(() => {
    if (!queued || queued.serverId !== target) return;
    setQueued(null);
    queued.run(login);
  }, [queued, target, login]);
  const on = (serverId: string, run: (login: GuidedLogin) => void): void => {
    if (login.signingInFor) login.finishSignIn();
    if (serverId === target) { run(login); return; }
    setTarget(serverId);
    setQueued({ serverId, run });
  };
  return { login, target, on };
}

export function AccountsPanel({ profiles, machineName, serverId, onReload }: Props): JSX.Element {
  const activeId = useServers((state) => state.activeId);
  const home = serverId ?? activeId ?? '';
  const fleet = useAccountFleet({ homeId: home, homeName: machineName, homeProfiles: profiles, onHomeReload: onReload });
  const { login, target, on } = useTargetedLogin(home, (id, list) => fleet.accept(id, list));
  const [adding, setAdding] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);
  const [editBusy, setEditBusy] = useState(false);
  const signInPending = login.signingInFor !== null && loginPending(login.operation);
  const busy = login.busy || signInPending || editBusy;
  const groups = rollUpAccounts(fleet.machines);
  const nameOf = (id: string): string => fleet.machines.find((machine) => machine.serverId === id)?.machineName ?? machineName;
  const edits = accountEdits(setEditBusy, setNotice, fleet.reloadComputer);

  const startAdding = (): void => {
    if (login.signingInFor) login.finishSignIn();
    login.setMessage(null); setNotice(null);
    setAdding(true);
  };
  const signIn = (placement: AccountPlacement): void => {
    setAdding(false); setNotice(null);
    on(placement.serverId, (current) => void current.startLogin(placement.profile));
  };
  const addOn = (group: AccountGroup, machine: MachineAccounts): void => {
    const first = group.placements[0]!.profile;
    setAdding(false); setNotice(null);
    on(machine.serverId, (current) => void current.addAccount(first.tool, first.name, first.label ?? ''));
  };

  return (
    <section className="settings-page accounts-panel">
      <AccountsIntroduction />
      <div className="settings-card accounts-card">
        <div className="accounts-card-head">
          <h2>Accounts added in Sessions</h2>
          <button type="button" className="btn btn-ghost accounts-refresh" disabled={fleet.refreshing} onClick={() => void fleet.refreshUsage()}>
            {fleet.refreshing ? 'Reading usage…' : 'Refresh usage'}
          </button>
        </div>
        {coverageNotes(fleet.machines).map((note) => <p key={note} className="settings-message" role="status">{note}</p>)}
        {groups.length === 0 ? (
          <p className="accounts-empty">
            {fleet.machines.some((machine) => machine.profiles === null && !machine.profilesError)
              ? 'Reading accounts…' : 'No second account yet.'}
          </p>
        ) : (
          <ul className="accounts-list" aria-label="Accounts">
            {groups.map((group) => (
              <AccountGroupRow
                key={group.key}
                group={group}
                addable={computersWithout(group, fleet.machines)}
                busy={busy}
                onSignIn={signIn}
                onRename={edits.rename}
                onForget={(placement) => void edits.forget(placement, groupTitle(group))}
                onAddOn={(machine) => addOn(group, machine)}
              />
            ))}
          </ul>
        )}
        {!adding && !signInPending ? (
          <button type="button" className="btn btn-secondary accounts-add-button" onClick={startAdding}>Add account</button>
        ) : null}
      </div>

      {adding && !login.signingInFor ? (
        <AddAccountForm
          busy={login.busy}
          computers={fleet.machines.filter((machine) => !machine.profilesError)}
          initialComputer={target}
          onCancel={() => { setAdding(false); login.setMessage(null); }}
          onAdd={(computer, tool, label) => on(computer, (current) => {
            void current.addAccount(tool, '', label).then((added) => { if (added) setAdding(false); });
          })}
        />
      ) : null}

      {login.signingInFor ? (
        <SigningInCard
          account={login.signingInFor}
          operation={login.operation}
          busy={login.busy}
          machineName={target === home ? undefined : nameOf(target)}
          onCode={login.submitCode}
          onCancel={login.cancel}
          onDone={login.finishSignIn}
        />
      ) : null}

      {login.message ? <p className="settings-message" role="status">{login.message}</p> : null}
      {notice ? <p className="settings-message" role="status">{notice}</p> : null}
      <p className="field-help">Removing an account keeps its saved chats and sign-in on that computer.</p>
    </section>
  );
}

/** Remove and rename on the computer that holds the account. */
function accountEdits(
  setBusy: (busy: boolean) => void,
  setNotice: (notice: string | null) => void,
  reloadComputer: (id: string) => Promise<void>
) {
  const forget = async (placement: AccountPlacement, title: string): Promise<void> => {
    setBusy(true); setNotice(null);
    try {
      await forgetAccount(placement.profile.tool, placement.profile.name, placement.serverId || undefined);
      setNotice(`${title} was removed from this list on ${placement.machineName}. Its saved chats and sign-in are unchanged.`);
      await reloadComputer(placement.serverId);
    } catch (error) {
      setNotice(error instanceof Error ? error.message : 'Sessions could not remove that account.');
    } finally {
      setBusy(false);
    }
  };
  // A refused rename is answered in its row, where the person is still typing.
  const rename = async (placement: AccountPlacement, label: string): Promise<string | null> => {
    setBusy(true);
    try {
      await renameAccount(placement.profile.tool, placement.profile.name, label, placement.serverId || undefined);
      await reloadComputer(placement.serverId);
      return null;
    } catch (error) {
      return error instanceof Error ? error.message : 'Sessions could not rename that account.';
    } finally {
      setBusy(false);
    }
  };
  return { forget, rename };
}

function AccountsIntroduction(): JSX.Element {
  return (
    <header className="accounts-intro">
      <h1>Accounts</h1>
      <p>
        See usage beside each account and where it is available. Each computer keeps its own sign-in.
      </p>
    </header>
  );
}

const providerName = (tool: 'claude' | 'codex'): string => tool === 'codex' ? 'ChatGPT' : 'Claude';

function AccountGroupRow(
  { group, addable, busy, onSignIn, onRename, onForget, onAddOn }: {
    group: AccountGroup;
    addable: MachineAccounts[];
    busy: boolean;
    onSignIn: (placement: AccountPlacement) => void;
    onRename: (placement: AccountPlacement, label: string) => Promise<string | null>;
    onForget: (placement: AccountPlacement) => void;
    onAddOn: (machine: MachineAccounts) => void;
  }
): JSX.Element {
  const title = groupTitle(group);
  const nickname = group.placements.some((placement) => placement.profile.label?.trim());
  // The email is secondary only when a nickname leads; otherwise it already is
  // the title, and the provider says what kind of account this is.
  const details = [
    providerName(group.tool),
    nickname && group.identity?.email ? group.identity.email : null,
    group.identity?.plan ? `${group.identity.plan} plan` : null
  ].filter(Boolean).join(' · ');
  return (
    <li className="accounts-row accounts-group">
      <ProviderMark provider={group.tool} size={32} />
      <div className="accounts-identity">
        <strong>{title}</strong>
        <span className="accounts-details">{details}</span>
        <AccountMatchNote group={group} />
      </div>
      <div className="accounts-usage-area"><AccountUsageSummary group={group} label={title} /></div>
      <div className="accounts-computers">
        <span className="accounts-computers-heading">Connected on</span>
        <ul className="accounts-placements" aria-label={`Computers for ${title}`}>
          {group.placements.map((placement) => (
            <AccountPlacementRow
              key={`${placement.serverId}:${placement.profile.tool}:${placement.profile.name}`}
              placement={placement}
              title={title}
              busy={busy}
              onSignIn={onSignIn}
              onRename={onRename}
              onForget={onForget}
            />
          ))}
        </ul>
        {addable.length > 0 ? <AddOnComputer title={title} provider={providerName(group.tool)} computers={addable} busy={busy} onAdd={onAddOn} /> : null}
      </div>
    </li>
  );
}

function AccountMatchNote({ group }: { group: AccountGroup }): JSX.Element | null {
  if (group.matchedByProvider && group.placements.length > 1) {
    return <span className="accounts-activity">One allowance, signed in on {group.placements.length} computers.</span>;
  }
  if (!group.matchedByProvider && group.sameEmailOn.length > 0) {
    return (
      <span className="accounts-activity" title="The provider did not report an account ID, and one email can belong to several workspaces or organizations with separate allowances.">
        The same email is also listed on {group.sameEmailOn.join(', ')}. Sessions cannot confirm it is the same workspace, so it is shown separately.
      </span>
    );
  }
  return null;
}
