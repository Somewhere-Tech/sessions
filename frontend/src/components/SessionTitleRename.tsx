import { useState } from 'react';

interface Props {
  label: string;
  onRename: (name: string) => Promise<void>;
}

export function SessionTitleRename({ label, onRename }: Props): JSX.Element {
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState(label);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const cancel = (): void => {
    setEditing(false);
    setValue(label);
    setError(null);
  };

  const save = async (): Promise<void> => {
    const name = value.trim();
    if (!name) {
      setError('Add a name for this session.');
      return;
    }
    setSaving(true);
    setError(null);
    try {
      await onRename(name);
      setEditing(false);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : 'Could not rename this session.');
    } finally {
      setSaving(false);
    }
  };

  if (editing) {
    return (
      <form className="session-title-rename" onSubmit={(event) => { event.preventDefault(); void save(); }}>
        <input
          type="text"
          aria-label="Session name"
          value={value}
          autoFocus
          disabled={saving}
          onChange={(event) => setValue(event.target.value)}
          onKeyDown={(event) => { if (event.key === 'Escape') cancel(); }}
        />
        <button type="submit" disabled={saving}>{saving ? 'Saving…' : 'Save'}</button>
        <button type="button" disabled={saving} onClick={cancel}>Cancel</button>
        {error ? <span className="session-title-rename-error" role="alert">{error}</span> : null}
      </form>
    );
  }

  return (
    <div className="session-title-display">
      <h1 title={label}>{label}</h1>
      <button
        type="button"
        className="session-title-rename-button"
        aria-label="Rename session"
        title="Rename session"
        onClick={() => { setValue(label); setError(null); setEditing(true); }}
      >
        <svg width="14" height="14" viewBox="0 0 18 18" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="1.4"><path d="m11 3 4 4M3 11l9-9 4 4-9 9-5 1z" /></svg>
      </button>
    </div>
  );
}
