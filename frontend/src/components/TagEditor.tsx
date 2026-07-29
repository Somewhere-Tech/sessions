import { useMemo, useState } from 'react';

interface Props {
  value: Record<string, string>;
  onChange: (value: Record<string, string>) => void;
  disabled?: boolean;
}

const TAG_KEY = /^[a-z0-9][a-z0-9._-]*$/;

export function TagEditor({ value, onChange, disabled = false }: Props): JSX.Element {
  const [key, setKey] = useState('');
  const [tagValue, setTagValue] = useState('');
  const [error, setError] = useState<string | null>(null);
  const entries = useMemo(
    () => Object.entries(value).sort(([left], [right]) => left.localeCompare(right)),
    [value]
  );

  const add = (): void => {
    const normalizedKey = key.trim().toLowerCase();
    const normalizedValue = tagValue.trim();
    if (!TAG_KEY.test(normalizedKey) || normalizedKey.length > 64) {
      setError('Keys start with a letter or number and may use . _ or -');
      return;
    }
    if (!normalizedValue || normalizedValue.length > 256) {
      setError('Values must be between 1 and 256 characters');
      return;
    }
    if (!(normalizedKey in value) && entries.length >= 32) {
      setError('A session can have up to 32 tags');
      return;
    }
    onChange({ ...value, [normalizedKey]: normalizedValue });
    setKey('');
    setTagValue('');
    setError(null);
  };

  return (
    <div className="tag-editor">
      {entries.length > 0 ? (
        <div className="tag-chip-list" aria-label="Session tags">
          {entries.map(([tagKey, currentValue]) => (
            <span className="tag-chip" key={tagKey}>
              <span className="tag-chip-key">{tagKey}</span>
              <span className="tag-chip-value">{currentValue}</span>
              <button
                type="button"
                aria-label={`Remove ${tagKey} tag`}
                disabled={disabled}
                onClick={() => {
                  const next = { ...value };
                  delete next[tagKey];
                  onChange(next);
                }}
              >
                ×
              </button>
            </span>
          ))}
        </div>
      ) : null}
      <div className="tag-editor-row">
        <input
          className="field-input tag-editor-key"
          type="text"
          placeholder="product"
          aria-label="Tag key"
          value={key}
          maxLength={64}
          disabled={disabled}
          onChange={(event) => setKey(event.target.value)}
        />
        <span className="tag-editor-equals">=</span>
        <input
          className="field-input tag-editor-value"
          type="text"
          placeholder="Sessions"
          aria-label="Tag value"
          value={tagValue}
          maxLength={256}
          disabled={disabled}
          onChange={(event) => setTagValue(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter') {
              event.preventDefault();
              add();
            }
          }}
        />
        <button
          type="button"
          className="btn btn-ghost tag-editor-add"
          disabled={disabled || !key.trim() || !tagValue.trim()}
          onClick={add}
        >
          Add tag
        </button>
      </div>
      {error ? <span className="tag-editor-error">{error}</span> : null}
      <span className="field-hint">Use any dimensions you care about, such as product, client, project, or environment.</span>
    </div>
  );
}
