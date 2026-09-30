import type { AccountProfile } from '../api/sessionsd';
import { accountLabel } from '../lib/accountChoice';
import { AccountLoginState, SigningInCard, useGuidedAccountLogin } from './AccountSignIn';

// Which subscriptions each computer can use, on the computer's own card.
//
// A second Claude or ChatGPT plan is bought once and signed into per machine,
// so the fact a person needs is per machine: this Mac has "Work — team plan",
// the mini does not. An account another computer has and this one lacks is an
// offer — "Log in here too" — running the same guided login Settings runs, on
// the machine whose card it appears under.

/** An account some other computer has, named the way its owner named it. */
export interface AccountElsewhere {
  tool: 'claude' | 'codex';
  name: string;
  label: string;
  /** The computers that already have it, for the button's own explanation. */
  machines: string[];
}

/**
 * The accounts other computers have that this one does not, keyed the way an
 * account is identified on a machine: provider and name.
 */
export function accountsMissingOn(
  accounts: Record<string, AccountProfile[]>, serverId: string, machineName: (id: string) => string
): AccountElsewhere[] {
  const here = new Set((accounts[serverId] ?? []).map((account) => `${account.tool}/${account.name}`));
  const missing = new Map<string, AccountElsewhere>();
  for (const [otherId, profiles] of Object.entries(accounts)) {
    if (otherId === serverId) continue;
    for (const account of profiles) {
      const key = `${account.tool}/${account.name}`;
      if (here.has(key)) continue;
      const existing = missing.get(key);
      if (existing) {
        if (!existing.machines.includes(machineName(otherId))) existing.machines.push(machineName(otherId));
        continue;
      }
      missing.set(key, {
        tool: account.tool, name: account.name, label: accountLabel(account), machines: [machineName(otherId)]
      });
    }
  }
  return [...missing.values()].sort((left, right) => left.label.localeCompare(right.label));
}

export function FleetMachineAccounts(
  { serverId, machineName, profiles, elsewhere, onOpenSession, onReload }: {
    serverId: string;
    machineName: string;
    profiles: AccountProfile[];
    elsewhere: AccountElsewhere[];
    onOpenSession: (sessionId: string) => void;
    onReload?: (profiles: AccountProfile[]) => void;
  }
): JSX.Element {
  const login = useGuidedAccountLogin({ serverId, onOpenSession, onReload });
  return (
    <div className="fleet-machine-meta fleet-machine-accounts">
      <strong>Accounts</strong>
      {profiles.length === 0 && elsewhere.length === 0 ? <span>Default account only</span> : null}
      {profiles.map((account) => (
        <span key={`${account.tool}:${account.name}`} className="fleet-account">
          <strong>{accountLabel(account)}</strong>
          {account.tool === 'claude' ? 'Claude' : 'Codex'}
          <AccountLoginState account={account} />
        </span>
      ))}
      {elsewhere.map((account) => (
        <span key={`${account.tool}:${account.name}`} className="fleet-account is-missing">
          <strong>{account.label}</strong>
          {account.tool === 'claude' ? 'Claude' : 'Codex'}
          <button
            type="button"
            className="btn btn-ghost"
            disabled={login.busy}
            title={`${account.machines.join(', ')} ${account.machines.length === 1 ? 'has' : 'have'} this account. Sessions will register it on ${machineName} and open ${account.tool === 'claude' ? 'Claude' : 'Codex'} there to sign in.`}
            onClick={() => void login.addAccount(account.tool, account.name, account.label)}
          >
            Log in here too
          </button>
        </span>
      ))}
      {login.signingInFor ? (
        <SigningInCard
          account={login.signingInFor}
          operation={login.operation}
          busy={login.busy}
          machineName={machineName}
          onCode={login.submitCode}
          onCancel={login.cancel}
          onDone={login.finishSignIn}
        />
      ) : null}
      {login.message ? <span role="status">{login.message}</span> : null}
    </div>
  );
}
