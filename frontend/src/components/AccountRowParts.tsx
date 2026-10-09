import { useId, useState, type FormEvent } from 'react';
import { placementCheck, type AccountPlacement, type MachineAccounts, type PlacementCheck } from '../lib/accountRollup';
import { relativeTime } from './AccountUsageSummary';

// The per-computer parts of an account row: where it is signed in, what that
// computer last confirmed, and the actions that belong to that computer's copy.

export function AccountPlacementRow(
  { placement, title, busy, onSignIn, onRename, onForget }: {
    placement: AccountPlacement;
    title: string;
    busy: boolean;
    onSignIn: (placement: AccountPlacement) => void;
    onRename: (placement: AccountPlacement, label: string) => Promise<string | null>;
    onForget: (placement: AccountPlacement) => void;
  }
): JSX.Element {
  const labelId = useId();
  const [editing, setEditing] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const account = placement.profile;
  const ownName = account.label?.trim();
  const check = placementCheck(placement);
  const previous = 'previous' in check ? check.previous : undefined;
  const activity = [
    ownName && ownName !== title ? `Called “${ownName}” here` : null,
    previous ? `Last verified as ${previous.email} on ${new Date(previous.checked_at).toLocaleDateString()}` : null,
    account.sessions.length > 0 ? `${account.sessions.length} active` : null,
    account.last_used > 0 ? `Last used ${new Date(account.last_used).toLocaleDateString()}` : null
  ].filter(Boolean).join(' · ');
  return (
    <li className="accounts-placement">
      <div className="accounts-placement-head">
        <span id={labelId} className="accounts-placement-name">{placement.machineName}</span>
        <PlacementStatus check={check} signedIn={account.signed_in} />
      </div>
      {editing ? (
        <NicknameEditor
          label={account.label ?? ''}
          placeholder={account.identity?.email ?? 'Work or personal'}
          title={title}
          busy={busy}
          onCancel={() => { setEditing(false); setError(null); }}
          onSave={async (label) => {
            const failure = await onRename(placement, label);
            setError(failure);
            if (!failure) setEditing(false);
          }}
        />
      ) : null}
      {activity ? <span className="accounts-activity">{activity}</span> : null}
      {error ? <span className="accounts-row-error" role="alert">{error}</span> : null}
      {!editing ? (
        <div className="accounts-actions" role="group" aria-labelledby={labelId}>
          <button type="button" className="btn btn-ghost" disabled={busy} onClick={() => onSignIn(placement)}>
            {check.kind === 'signed_in' || check.kind === 'verified' || check.kind === 'failed' ? 'Check account' : 'Sign in'}
          </button>
          <button type="button" className="btn btn-ghost" disabled={busy} onClick={() => { setError(null); setEditing(true); }}>
            Rename
          </button>
          <button type="button" className="btn btn-ghost accounts-remove" disabled={busy} onClick={() => onForget(placement)}>
            Remove
          </button>
        </div>
      ) : null}
    </li>
  );
}

const on = (at?: number): string => (at ? ` on ${new Date(at).toLocaleString()}` : '');
const checked = (at?: number): string => (at ? `, checked ${relativeTime(at)}` : '');

/** What this computer actually knows about its sign-in; see `placementCheck`. */
function PlacementStatus({ check, signedIn }: { check: PlacementCheck; signedIn: boolean }): JSX.Element {
  const history = 'previous' in check && check.previous
    ? ` Earlier it was verified as ${check.previous.email}; that is history, not the account signed in now.`
    : '';
  switch (check.kind) {
    case 'signed_in':
      return (
        <span className="accounts-status is-verified" title={`The provider answered for this account on ${new Date(check.at).toLocaleString()}.`}>
          Signed in, checked {relativeTime(check.at)}
        </span>
      );
    case 'verified':
      return (
        <span
          className="accounts-status is-verified"
          title={`The provider reported this account on ${new Date(check.at).toLocaleString()}. That is not a check of the sign-in now.`}
        >
          Verified {new Date(check.at).toLocaleDateString()}
        </span>
      );
    case 'signed_out':
      return (
        <span className="accounts-status" title={`${check.message ?? `The provider reported no sign-in here${on(check.at)}.`}${history}`}>
          Signed out{checked(check.at)}
        </span>
      );
    case 'not_subscription':
      return (
        <span className="accounts-status" title={`The provider reported a sign-in here${on(check.at)} that is not a subscription, such as an API key. Sign in with a subscription account.${history}`}>
          Not a subscription{checked(check.at)}
        </span>
      );
    case 'failed':
      return (
        <span className="accounts-status" title={`The check${on(check.at)} could not read who is signed in. That does not mean signed out; check again.${history}`}>
          Check failed{check.at ? ` ${relativeTime(check.at)}` : ''}
        </span>
      );
  }
  return (
    <span
      className="accounts-status"
      title={(signedIn
        ? 'A provider login file is present, but Sessions has not confirmed who is signed in. Check this account to confirm who is signed in.'
        : 'Check this account to confirm who is signed in.') + history}
    >
      Identity not checked
    </span>
  );
}

function NicknameEditor(
  { label, placeholder, title, busy, onSave, onCancel }: {
    label: string;
    placeholder: string;
    title: string;
    busy: boolean;
    onSave: (label: string) => Promise<void>;
    onCancel: () => void;
  }
): JSX.Element {
  const [draft, setDraft] = useState(label);
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
        placeholder={placeholder}
        aria-label={`Nickname for ${title}`}
        autoFocus
      />
      <button type="submit" className="btn btn-primary" disabled={busy}>Save</button>
      <button type="button" className="btn btn-ghost" disabled={busy} onClick={onCancel}>Cancel</button>
    </form>
  );
}

/** Sign this account in on a computer that does not have it yet. */
export function AddOnComputer(
  { title, provider, computers, busy, onAdd }: {
    title: string;
    provider: string;
    computers: MachineAccounts[];
    busy: boolean;
    onAdd: (machine: MachineAccounts) => void;
  }
): JSX.Element {
  return (
    <div className="accounts-add-computer">
      {computers.map((machine) => (
        <button
          key={machine.serverId}
          type="button"
          className="btn btn-ghost"
          disabled={busy}
          title={`Adds ${title} to ${machine.machineName} and opens ${provider}’s own sign-in there. Sign in with the same account; credentials are never copied between computers.`}
          onClick={() => onAdd(machine)}
        >
          Add on {machine.machineName}
        </button>
      ))}
    </div>
  );
}
