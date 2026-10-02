import { useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import {
  isTauri,
  listNativeMoveMachines,
  moveNativeSession,
  type NativeMovePlan,
  type NativeSavedMachine
} from '../lib/tauriBridge';
import { useSessions } from '../store/sessions';
import { killSession } from '../api/sessionsd';
import { isLocalServer, serverDisplayName, syncNativeAgentMachineAccess, useServers, type ServerConfig } from '../lib/servers';

function moveMachineChoices(values: NativeSavedMachine[], source: ServerConfig, local: ServerConfig | null): { sourceMachine: string; destinations: NativeSavedMachine[] } {
  const remote = !isLocalServer(source);
  const endpoint = `${source.scheme ?? 'http'}://${source.host}:${source.port}`.replace(/\/$/, '');
  const matched = remote ? values.find((machine) => source.machineId
    ? machine.machine_id === source.machineId
    : machine.endpoint.replace(/\/$/, '') === endpoint) : undefined;
  if (remote && !matched) throw new Error(`Sessions could not use the saved connection to ${serverDisplayName(source, true)}. Check its connection in Fleet, then try again. Nothing has moved.`);
  const destinations = values.filter((machine) => machine.machine_id !== matched?.machine_id);
  if (remote && local) destinations.unshift({
    alias: '__local__', machine_id: 'local', name: serverDisplayName(local, true),
    endpoint: `http://127.0.0.1:${local.port}`, transport: 'local', device_id: '', connected_at: ''
  });
  return { sourceMachine: matched?.alias ?? '', destinations };
}

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
  const serverId = useSessions((state) => state.serverId);
  const source = useSessions((state) => state.sessions.find((session) => session.id === sessionId)) ?? null;
  const sourceServer = useServers((state) => state.servers.find((server) => server.id === serverId)) ?? null;
  const localServer = useServers((state) => state.servers.find((server) => server.isDefault && isLocalServer(server))) ?? null;
  const [open, setOpen] = useState(false);
  const [machines, setMachines] = useState<NativeSavedMachine[]>([]);
  const [selected, setSelected] = useState('');
  const [sourceMachine, setSourceMachine] = useState('');
  const [plan, setPlan] = useState<NativeMovePlan | null>(null);
  const [allowDirty, setAllowDirty] = useState(false);
  const [runtimeMode, setRuntimeMode] = useState<'rich' | 'terminal'>(
    source?.tool === 'claude-code' ? 'terminal' : 'rich'
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [complete, setComplete] = useState<NativeMovePlan | null>(null);
  const sourceServerId = useRef(serverId);
  const sourceAtOpen = useRef(source);
  const sourceLabelAtOpen = useRef('');

  if (!isTauri() || !sourceServer) return null;
  const sourceLabel = open ? sourceLabelAtOpen.current : serverDisplayName(sourceServer, true);
  const moveSource = open ? sourceAtOpen.current : source;

  const show = async (): Promise<void> => {
    sourceServerId.current = serverId;
    sourceAtOpen.current = source;
    sourceLabelAtOpen.current = serverDisplayName(sourceServer, true);
    setOpen(true);
    setPlan(null);
    setComplete(null);
    setError(null);
    setMachines([]);
    setSelected('');
    setSourceMachine('');
    setRuntimeMode(source?.tool === 'claude-code' ? 'terminal' : 'rich');
    setBusy(true);
    try {
      await syncNativeAgentMachineAccess();
      const values = await listNativeMoveMachines();
      const { sourceMachine: matchedSource, destinations } = moveMachineChoices(values, sourceServer, localServer);
      setSourceMachine(matchedSource);
      setMachines(destinations);
      setSelected((current) => destinations.some((machine) => machine.alias === current) ? current : destinations[0]?.alias || '');
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
      if (moveSource && !moveSource.exited) {
        await killSession(sessionId, 'Moved to another computer through Sessions', sourceServerId.current ?? undefined);
      }
      setPlan(await moveNativeSession(sessionId, selected, { dryRun: true, allowDirty, runtimeMode, sourceMachine }));
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
      const result = await moveNativeSession(sessionId, selected, { dryRun: false, allowDirty, runtimeMode, sourceMachine });
      setComplete(result);
      await refresh(sourceServerId.current ?? undefined);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : 'Could not resume this conversation.');
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
        Move to another computer{appearance === 'menuitem' ? '…' : ''}
      </button>
      {open ? createPortal((
        <div className="dialog-backdrop continue-elsewhere-backdrop" onMouseDown={(event) => {
          if (event.target === event.currentTarget && !busy) setOpen(false);
        }}>
          <section className="dialog continue-elsewhere-dialog" role="dialog" aria-modal="true" aria-labelledby="continue-elsewhere-title">
            <header>
              <div>
                <span className="dialog-kicker">Move conversation</span>
                <h2 id="continue-elsewhere-title">Move “{label}”</h2>
              </div>
              <button type="button" className="dialog-close" aria-label="Close" disabled={busy} onClick={() => setOpen(false)}>×</button>
            </header>
            {complete ? (
              <div className="continue-elsewhere-complete">
                <strong>Conversation moved to {machines.find((machine) => machine.alias === selected)?.name || selected}.</strong>
                <p>The original history remains on {sourceLabel}. The new conversation is <code>{complete.target_id?.slice(0, 8)}</code>.</p>
                {complete.warning ? <p className="dialog-warning">{complete.warning}</p> : null}
                <button type="button" className="btn btn-primary" onClick={() => setOpen(false)}>Done</button>
              </div>
            ) : (
              <>
                <p className="continue-elsewhere-explainer">
                  {moveSource && !moveSource.exited
                    ? `Moving stops this session on ${sourceLabel}, then starts its conversation on the computer you choose. The source history stays there so nothing is deleted.`
                    : `Sessions starts this conversation on the computer you choose. The source history stays on ${sourceLabel} so nothing is deleted.`}
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
                ) : !busy && !error ? (
                  <div className="continue-elsewhere-empty">
                    <strong>No saved machines yet.</strong>
                    <p>Pair another computer from Fleet. The app uses protected device credentials without placing them in a command or the web view.</p>
                  </div>
                ) : null}
                <label className="continue-elsewhere-dirty">
                  <input type="checkbox" checked={allowDirty} disabled={busy} onChange={(event) => {
                    setAllowDirty(event.currentTarget.checked);
                    setPlan(null);
                  }} />
                  <span><strong>Include uncommitted Git work in a temporary checkpoint</strong><small>The original files and branch stay unchanged.</small></span>
                </label>
                {moveSource?.tool === 'claude-code' ? (
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
                    <strong>Ready to move</strong>
                    <span>{plan.tool === 'codex' ? 'Codex' : 'Claude'} · {(plan.conversation_bytes / 1024 / 1024).toFixed(1)} MB conversation</span>
                    <span>{plan.tool === 'claude-code'
                      ? 'Conversation + Terminal'
                      : plan.runtime_mode === 'terminal' ? 'Terminal interface' : 'Rich conversation'}</span>
                    <span>{plan.workspace.git ? `Git ${plan.workspace.branch || 'workspace'} at ${(plan.workspace.revision || '').slice(0, 8)}` : 'The same folder must already exist on the destination'}</span>
                    <span>Source history remains on {sourceLabel}</span>
                  </div>
                ) : null}
                {error ? <div className="dialog-error" role="alert">{error}</div> : null}
                <footer>
                  <button type="button" className="btn btn-ghost" disabled={busy} onClick={() => setOpen(false)}>Cancel</button>
                  {plan ? (
                    <button type="button" className="btn btn-primary" disabled={busy} onClick={() => void execute()}>
                      {busy ? 'Moving…' : `Move to ${machines.find((machine) => machine.alias === selected)?.name || selected}`}
                    </button>
                  ) : (
                    <button type="button" className="btn btn-primary" disabled={busy || !selected} onClick={() => void review()}>
                      {busy ? 'Checking…' : moveSource && !moveSource.exited ? 'End here and review move' : 'Review move'}
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
