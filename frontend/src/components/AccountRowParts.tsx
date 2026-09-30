import { useId, useState, type FormEvent } from 'react';
import type { AccountPlacement, MachineAccounts } from '../lib/accountRollup';
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
  const activity = [
    ownName && ownName !== title ? `Called “${ownName}” here` : null,
    account.sessions.length > 0 ? `${account.sessions.length} active` : null,
    account.last_used > 0 ? `Last used ${new Date(account.last_used).toLocaleDateString()}` : null
  ].filter(Boolean).join(' · ');
  return (
    <li className="accounts-placement">
      <div className="accounts-placement-head">
        <span id={labelId} className="accounts-placement-name">{placement.machineName}</span>
        <PlacementStatus placement={placement} />
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
            {account.identity && placement.usage?.state !== 'signed_out' ? 'Check account' : 'Sign in'}
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

/**
 * What this computer actually knows about its sign-in. A usage read that
 * succeeded is a provider check that just happened; a saved identity is a
 * check that happened once; a login file on disk is only a file, so it never
 * reads as ready.
 */
function PlacementStatus({ placement }: { placement: AccountPlacement }): JSX.Element {
  const { usage, profile } = placement;
  if (usage?.state === 'available' && !usage.stale && usage.checked_at) {
    return (
      <span className="accounts-status is-verified" title={`The provider answered for this account on ${new Date(usage.checked_at).toLocaleString()}.`}>
        Signed in, checked {relativeTime(usage.checked_at)}
      </span>
    );
  }
  if (usage?.state === 'signed_out') {
    return <span className="accounts-status" title={usage.message}>Signed out</span>;
  }
  if (profile.identity) {
    return (
      <span
        className="accounts-status is-verified"
        title={`The provider reported this account on ${new Date(profile.identity.checked_at).toLocaleString()}. That is not a check of the sign-in now.`}
      >
        Verified {new Date(profile.identity.checked_at).toLocaleDateString()}
      </span>
    );
  }
  return (
    <span
      className="accounts-status"
      title={profile.signed_in
        ? 'A provider login file is present, but Sessions has not confirmed who is signed in. Check this account to confirm who is signed in.'
        : 'Check this account to confirm who is signed in.'}
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

export function AddAccountForm(
  { busy, computers, initialComputer, onAdd, onCancel }: {
    busy: boolean;
    computers: MachineAccounts[];
    initialComputer: string;
    onAdd: (computer: string, tool: 'claude' | 'codex', label: string) => void;
    onCancel: () => void;
  }
): JSX.Element {
  const [tool, setTool] = useState<'claude' | 'codex'>('claude');
  const [label, setLabel] = useState('');
  const [computer, setComputer] = useState(initialComputer);
  const machineName = computers.find((machine) => machine.serverId === computer)?.machineName;
  return (
    <form
      className="settings-card accounts-add"
      aria-label="Add an account"
      onSubmit={(event) => { event.preventDefault(); if (!busy) onAdd(computer, tool, label.trim()); }}
    >
      <h2>Add an account{machineName && computers.length > 1 ? ` on ${machineName}` : ''}</h2>
      {computers.length > 1 ? (
        <label>
          <span>Computer</span>
          <select aria-label="Computer" value={computer} onChange={(event) => setComputer(event.currentTarget.value)}>
            {computers.map((machine) => <option key={machine.serverId} value={machine.serverId}>{machine.machineName}</option>)}
          </select>
        </label>
      ) : null}
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
