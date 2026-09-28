import { useEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { restartConversation, type RestartResult } from '../api/sessionsd/restart';
import { getActiveServer } from '../lib/servers';
import { draftStorageKey, flushDraft, readDraft, saveDraft } from '../lib/draftStore';
import { sessionMode } from '../lib/sessionMode';
import { resolvedSessionLabel } from '../lib/tabLabels';
import type { RestartConversationProps } from './RestartConversation';
import '../styles/restart-conversation.css';

function preserveRestartDraft(machineId: string, source: string, destination: string): void {
  flushDraft(draftStorageKey(machineId, source));
  const draft = readDraft(draftStorageKey(machineId, source));
  if (draft.warning) throw new Error(draft.warning);
  if (!draft.text) return;
  const saved = saveDraft(draftStorageKey(machineId, destination), draft.text);
  if (!saved.saved) throw new Error(`${saved.warning} Your draft remains on the original tab. The replacement is already open; copy your draft before opening it.`);
}

/** A runtime id is the confirmation boundary; never follow a retired row's successor. */
export function RestartConversationDialog({ session, onOpen, serverId, initialRemoteControl, initialRuntimeMode, onClose }: RestartConversationProps & { onClose: () => void }): JSX.Element | null {
  const [permissions, setPermissions] = useState<'constrained' | 'full'>(session.permissions === 'full' ? 'full' : 'constrained');
  const [remoteControl, setRemoteControl] = useState(initialRemoteControl ?? session.args.some((arg) => arg === '--remote-control' || arg.startsWith('--remote-control=')));
  const [runtimeMode, setRuntimeMode] = useState<'rich' | 'terminal'>(initialRuntimeMode ?? sessionMode(session));
  const [submitted, setSubmitted] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<RestartResult | null>(null);
  const locked = useRef(false);
  useEffect(() => {
    const onKey = (event: KeyboardEvent): void => { if (event.key === 'Escape' && !locked.current) onClose(); };
    document.addEventListener('keydown', onKey);
    document.querySelector<HTMLSelectElement>('[aria-label="Restart permissions"]')?.focus();
    return () => document.removeEventListener('keydown', onKey);
  }, [onClose]);
  const machineId = serverId ?? getActiveServer().id;
  const reopen = (id: string): void => {
    preserveRestartDraft(machineId, session.id, id);
    onOpen(id); onClose();
  };
  const restart = async (): Promise<void> => {
    if (locked.current) return;
    locked.current = true; setSubmitted(true); setBusy(true); setError(null);
    try {
      flushDraft(draftStorageKey(machineId, session.id));
      const draft = readDraft(draftStorageKey(machineId, session.id));
      if (draft.warning) throw new Error(draft.warning);
      const next = await restartConversation(session.id, permissions, remoteControl, machineId, remoteControl ? 'terminal' : runtimeMode);
      setResult(next);
      if (next.error) throw new Error(next.error);
      if (!next.laneId) throw new Error('No replacement runtime was reported. Retry the same choices to recover the recorded restart.');
      if (next.partial) {
        setError(next.adoption?.warning ?? 'The replacement is running, but its history link needs repair. Retry these same choices to finish its record.');
        return;
      }
      reopen(next.laneId);
    } catch (reason) { setError(reason instanceof Error ? reason.message : 'Restart failed. Retry the same choices to check the recorded operation.'); }
    finally { locked.current = false; setBusy(false); }
  };
  return createPortal(<div className="dialog-backdrop" onClick={(event) => event.stopPropagation()}>
      <section className="dialog restart-conversation-dialog" role="dialog" aria-modal="true" aria-labelledby={`restart-title-${session.id}`}>
        <h2 id={`restart-title-${session.id}`}>Restart “{resolvedSessionLabel(session)}”</h2>
        <p>This ends only runtime <code>{session.id}</code>{session.pid ? ` (PID ${session.pid})` : ''} and reopens the same {session.tool === 'codex' ? 'Codex' : 'Claude'} conversation.</p>
        <p>The {session.profile || 'default'} account, {session.model || 'current'} model, saved history and your unsent draft are kept. Running commands and unsaved process state are interrupted. Your draft is never sent automatically.</p>
        {session.working ? <p role="status">This agent is working. Restart interrupts its current turn.</p> : null}
        <label>Permissions <select aria-label="Restart permissions" value={permissions} disabled={busy || submitted} onChange={(event) => setPermissions(event.target.value as 'constrained' | 'full')}>
          <option value="constrained">Ask for approval</option><option value="full">Full access (YOLO)</option>
        </select></label>
        {permissions === 'full' ? <p>Full access lets this conversation run commands without approval. This changes only the replacement runtime.</p> : null}
        <label>Runtime <select aria-label="Restart runtime" value={remoteControl ? 'terminal' : runtimeMode} disabled={busy || submitted || remoteControl} onChange={(event) => setRuntimeMode(event.target.value as 'rich' | 'terminal')}><option value="rich">Rich</option><option value="terminal">Terminal</option></select></label>
        {session.tool === 'claude-code' ? <label><input type="checkbox" checked={remoteControl} disabled={busy || submitted} onChange={(event) => setRemoteControl(event.target.checked)} /> Remote Control (Claude Terminal; requires consent in Settings)</label> : null}
        {error ? <p role="alert">{error}</p> : null}
        {result?.sourceEnded ? <p role="status">Original runtime ended.{result.laneId ? ` Replacement: ${result.laneId}.` : ' Replacement creation is incomplete.'}</p> : null}
        <div className="dialog-actions">
          <button type="button" className="btn" disabled={busy} onClick={onClose}>{result ? 'Close' : 'Cancel'}</button>
          {result?.laneId ? <button type="button" className="btn" disabled={busy} onClick={() => { try { reopen(result.laneId!); } catch (reason) { setError(String(reason)); } }}>Open replacement</button> : null}
          <button type="button" className="btn btn-primary" disabled={busy} onClick={() => void restart()}>{busy ? 'Restarting…' : submitted ? 'Retry same restart' : 'End this runtime and reopen'}</button>
        </div>
      </section>
    </div>, document.body);
}
