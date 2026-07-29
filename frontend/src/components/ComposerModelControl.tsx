import { useEffect, useId, useRef, useState } from 'react';
import { listSessionModelOptions, type SessionModelOption } from '../api/sessionsd';
import type { SessionTool } from '../types';

interface Props {
  sessionId: string;
  provider: SessionTool;
  model?: string;
  effort?: string;
  supported: boolean;
  working: boolean;
  onChange: (model: string, effort: string) => Promise<void>;
}

function compactModel(model?: string): string {
  if (!model) return 'Provider default';
  return model.length > 24 ? `${model.slice(0, 21)}…` : model;
}

export function ComposerModelControl({
  sessionId,
  provider,
  model,
  effort,
  supported,
  working,
  onChange
}: Props): JSX.Element {
  const [open, setOpen] = useState(false);
  const [draftModel, setDraftModel] = useState(model ?? '');
  const [draftEffort, setDraftEffort] = useState(effort ?? '');
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [options, setOptions] = useState<SessionModelOption[]>([]);
  const [optionsLoading, setOptionsLoading] = useState(false);
  const [optionsError, setOptionsError] = useState<string | null>(null);
  const rootRef = useRef<HTMLDivElement>(null);
  const modelListId = useId();

  useEffect(() => {
    if (open) return;
    setDraftModel(model ?? '');
    setDraftEffort(effort ?? '');
  }, [effort, model, open]);

  useEffect(() => {
    if (!open) return;
    const close = (event: PointerEvent): void => {
      if (!rootRef.current?.contains(event.target as Node)) setOpen(false);
    };
    window.addEventListener('pointerdown', close);
    return () => window.removeEventListener('pointerdown', close);
  }, [open]);

  useEffect(() => {
    if (!open || !supported || provider !== 'codex') return;
    let live = true;
    setOptionsLoading(true);
    setOptionsError(null);
    void listSessionModelOptions(sessionId)
      .then((models) => {
        if (live) setOptions(models.filter((option) => !option.hidden));
      })
      .catch((caught) => {
        if (live) setOptionsError((caught as Error).message);
      })
      .finally(() => {
        if (live) setOptionsLoading(false);
      });
    return () => { live = false; };
  }, [open, provider, sessionId, supported]);

  const save = async (): Promise<void> => {
    if (!supported || working || !draftModel.trim()) return;
    setSaving(true);
    setError(null);
    try {
      await onChange(draftModel.trim(), draftEffort);
      setOpen(false);
    } catch (caught) {
      setError((caught as Error).message);
    } finally {
      setSaving(false);
    }
  };

  const providerName = provider === 'codex' ? 'Codex' : 'Claude';
  return (
    <div className="composer-model-control" ref={rootRef}>
      <button
        type="button"
        className="composer-model-trigger"
        onClick={() => { setError(null); setOpen((current) => !current); }}
        aria-expanded={open}
        title={supported
          ? 'Choose the model and effort for the next message'
          : 'Live model changes require a current Rich session'}
      >
        <span>{compactModel(model)}</span>
        {effort ? <span>{effort}</span> : null}
        <span aria-hidden>⌄</span>
      </button>
      {open ? (
        <section className="composer-model-popover" aria-label={`${providerName} model for next message`}>
          <header>
            <strong>Next message</strong>
            <button type="button" aria-label="Close model selector" onClick={() => setOpen(false)}>×</button>
          </header>
          {!supported ? (
            <p>This session uses Terminal compatibility mode or an older runner. Change the model in Terminal, or resume it with the current Rich runtime.</p>
          ) : (
            <>
              <label>
                <span>Model</span>
                <input
                  value={draftModel}
                  onChange={(event) => setDraftModel(event.currentTarget.value)}
                  list={modelListId}
                  placeholder={provider === 'codex' ? 'Exact Codex model ID' : 'Claude model or alias'}
                  maxLength={128}
                  autoFocus
                />
                <datalist id={modelListId}>
                  {provider === 'claude-code' ? (
                    <>
                    <option value="opus" />
                    <option value="sonnet" />
                    <option value="haiku" />
                    </>
                  ) : options.map((option) => (
                    <option key={option.id} value={option.id}>{option.displayName}</option>
                  ))}
                </datalist>
              </label>
              <label>
                <span>Effort</span>
                <select value={draftEffort} onChange={(event) => setDraftEffort(event.currentTarget.value)}>
                  <option value="">Provider default</option>
                  <option value="low">Low</option>
                  <option value="medium">Medium</option>
                  <option value="high">High</option>
                  <option value="xhigh">Extra high</option>
                  {provider === 'claude-code' ? <option value="max">Max</option> : null}
                </select>
              </label>
              <p>
                {working
                  ? `Wait for ${providerName} to finish this turn.`
                  : optionsLoading
                    ? 'Loading available Codex models…'
                    : optionsError
                      ? 'Could not load the live catalog. You can still enter an exact model ID.'
                      : 'Applies to the next message. Existing history is unchanged.'}
              </p>
              {error ? <p className="composer-model-error" role="alert">{error}</p> : null}
              <button
                type="button"
                className="btn btn-primary composer-model-save"
                disabled={saving || working || !draftModel.trim()}
                onClick={() => void save()}
              >
                {saving ? 'Saving…' : 'Use for next message'}
              </button>
            </>
          )}
        </section>
      ) : null}
    </div>
  );
}
