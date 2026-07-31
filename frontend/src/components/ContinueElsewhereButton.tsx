import { useState } from 'react';
import { createPortal } from 'react-dom';
import {
  isTauri,
  listNativeMoveMachines,
  moveNativeSession,
  type NativeMovePlan,
  type NativeSavedMachine
} from '../lib/tauriBridge';
import { useSessions } from '../store/sessions';

export function ContinueElsewhereButton({
  sessionId,
  label,
  appearance = 'button',
  onOpen
}: {
  sessionId: string;
  label: string;
  appearance?: 'button' | 'menuitem';
  onOpen?: () => void;
}): JSX.Element | null {
  const refresh = useSessions((state) => state.refresh);
  const source = useSessions((state) => state.sessions.find((session) => session.id === sessionId)) ?? null;
  const [open, setOpen] = useState(false);
  const [machines, setMachines] = useState<NativeSavedMachine[]>([]);
  const [selected, setSelected] = useState('');
  const [plan, setPlan] = useState<NativeMovePlan | null>(null);
  const [allowDirty, setAllowDirty] = useState(false);
  const [runtimeMode, setRuntimeMode] = useState<'rich' | 'terminal'>(
    source?.tool === 'claude-code' ? 'terminal' : 'rich'
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [complete, setComplete] = useState<NativeMovePlan | null>(null);

  if (!isTauri()) return null;

  const show = async (): Promise<void> => {
    setOpen(true);
    setPlan(null);
    setComplete(null);
    setError(null);
    setRuntimeMode(source?.tool === 'claude-code' ? 'terminal' : 'rich');
    setBusy(true);
    try {
      const values = await listNativeMoveMachines();
      setMachines(values);
      setSelected((current) => current || values[0]?.alias || '');
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : 'Could not load saved machines.');
    } finally {
      setBusy(false);
    }
  };

  const review = async (): Promise<void> => {
    if (!selected) return;
    setBusy(true);
    setError(null);
    try {
      setPlan(await moveNativeSession(sessionId, selected, { dryRun: true, allowDirty, runtimeMode }));
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : 'Could not prepare this continuation.');
    } finally {
      setBusy(false);
    }
  };

  const execute = async (): Promise<void> => {
    if (!selected || !plan) return;
    setBusy(true);
    setError(null);
    try {
      const result = await moveNativeSession(sessionId, selected, { dryRun: false, allowDirty, runtimeMode });
      setComplete(result);
      await refresh();
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : 'Could not continue this conversation.');
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <button
        type="button"
        className={appearance === 'button' ? 'btn btn-secondary' : undefined}
        role={appearance === 'menuitem' ? 'menuitem' : undefined}
        onClick={() => {
          onOpen?.();
          void show();
        }}
      >
        Continue on another machine{appearance === 'menuitem' ? '…' : ''}
      </button>
      {open ? createPortal((
        <div className="dialog-backdrop continue-elsewhere-backdrop" onMouseDown={(event) => {
          if (event.target === event.currentTarget && !busy) setOpen(false);
        }}>
          <section className="dialog continue-elsewhere-dialog" role="dialog" aria-modal="true" aria-labelledby="continue-elsewhere-title">
            <header>
              <div>
                <span className="dialog-kicker">Cross-machine continuation</span>
                <h2 id="continue-elsewhere-title">Continue “{label}” elsewhere</h2>
              </div>
              <button type="button" className="dialog-close" aria-label="Close" disabled={busy} onClick={() => setOpen(false)}>×</button>
            </header>
            {complete ? (
              <div className="continue-elsewhere-complete">
                <strong>Conversation continued on {machines.find((machine) => machine.alias === selected)?.name || selected}.</strong>
                <p>The original history remains on this machine. Sessions linked it to the new runtime <code>{complete.target_id?.slice(0, 8)}</code>.</p>
                {complete.warning ? <p className="dialog-warning">{complete.warning}</p> : null}
                <button type="button" className="btn btn-primary" onClick={() => setOpen(false)}>Done</button>
              </div>
            ) : (
              <>
                <p className="continue-elsewhere-explainer">
                  Sessions copies only the provider conversation file and a verified workspace reference. It does not move credentials, attachments, or a live process, and it never deletes the source history.
                </p>
                {machines.length > 0 ? (
                  <label className="continue-elsewhere-machine">
                    <span>Destination</span>
                    <select value={selected} disabled={busy} onChange={(event) => {
                      setSelected(event.currentTarget.value);
                      setPlan(null);
                    }}>
                      {machines.map((machine) => (
                        <option value={machine.alias} key={machine.machine_id}>
                          {machine.name} · {machine.transport}
                        </option>
                      ))}
                    </select>
                  </label>
                ) : !busy ? (
                  <div className="continue-elsewhere-empty">
                    <strong>No saved machines yet.</strong>
                    <p>Pair one with <code>sessions machines connect</code>. The app then uses that private device credential without placing it in a command or the web view.</p>
                  </div>
                ) : null}
                <label className="continue-elsewhere-dirty">
                  <input type="checkbox" checked={allowDirty} disabled={busy} onChange={(event) => {
                    setAllowDirty(event.currentTarget.checked);
                    setPlan(null);
                  }} />
                  <span><strong>Include uncommitted Git work in a temporary checkpoint</strong><small>The original files and branch stay unchanged.</small></span>
                </label>
                {source?.tool === 'claude-code' ? (
                  <div className="continue-elsewhere-runtime">
                    <strong>Conversation + Terminal</strong>
                    <small>Claude continues as one interactive session. The destination machine’s Remote Control setting decides whether it also appears on claude.ai and mobile.</small>
                  </div>
                ) : (
                  <fieldset className="continue-elsewhere-runtime">
                    <legend>Open as</legend>
                    <label>
                      <input type="radio" name="move-runtime" checked={runtimeMode === 'rich'} disabled={busy} onChange={() => { setRuntimeMode('rich'); setPlan(null); }} />
                      <span><strong>Rich</strong><small>Recommended. Clean conversation, plans, tools, and model controls.</small></span>
                    </label>
                    <label>
                      <input type="radio" name="move-runtime" checked={runtimeMode === 'terminal'} disabled={busy} onChange={() => { setRuntimeMode('terminal'); setPlan(null); }} />
                      <span><strong>Terminal</strong><small>The provider’s full terminal interface and setup screens.</small></span>
                    </label>
                  </fieldset>
                )}
                {plan ? (
                  <div className="continue-elsewhere-plan">
                    <strong>Ready to continue</strong>
                    <span>{plan.tool === 'codex' ? 'Codex' : 'Claude'} · {(plan.conversation_bytes / 1024 / 1024).toFixed(1)} MB conversation</span>
                    <span>{plan.tool === 'claude-code'
                      ? 'Conversation + Terminal'
                      : plan.runtime_mode === 'terminal' ? 'Terminal interface' : 'Rich conversation'}</span>
                    <span>{plan.workspace.git ? `Git ${plan.workspace.branch || 'workspace'} at ${(plan.workspace.revision || '').slice(0, 8)}` : 'The same folder must already exist on the destination'}</span>
                    <span>Source history remains here</span>
                  </div>
                ) : null}
                {error ? <div className="dialog-error" role="alert">{error}</div> : null}
                <footer>
                  <button type="button" className="btn btn-ghost" disabled={busy} onClick={() => setOpen(false)}>Cancel</button>
                  {plan ? (
                    <button type="button" className="btn btn-primary" disabled={busy} onClick={() => void execute()}>
                      {busy ? 'Continuing…' : `Continue on ${machines.find((machine) => machine.alias === selected)?.name || selected}`}
                    </button>
                  ) : (
                    <button type="button" className="btn btn-primary" disabled={busy || !selected} onClick={() => void review()}>
                      {busy ? 'Checking…' : 'Review continuation'}
                    </button>
                  )}
                </footer>
              </>
            )}
          </section>
        </div>
      ), document.body) : null}
    </>
  );
}
