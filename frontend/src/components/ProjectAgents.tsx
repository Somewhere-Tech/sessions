import { useState, type ReactNode } from 'react';
import type { AgentProject, Agent } from '../lib/projectAgents';
import { resolvedSessionLabel } from '../lib/tabLabels';
import { classifySession } from '../lib/sessionStatus';
import { serverDisplayName } from '../lib/servers';
import { normalizeProvider, ProviderMark } from './ProviderBadge';
import { updateSessionName } from '../api/sessionsd';
import { useSessions } from '../store/sessions';

export function ProjectAgents({ groups, activeMachineId, renderLocal, onOpen, onAdd }: {
  groups: AgentProject[];
  activeMachineId: string | null;
  renderLocal: (row: Agent) => ReactNode;
  onOpen: (serverId: string, sessionId: string) => void;
  onAdd?: (row: Agent, project: string) => void;
}): JSX.Element {
  return <>{groups.map((group) => <section className="agent-project" key={group.id} aria-label={group.name}>
    <header><h3>{group.name}</h3>{onAdd ? <button type="button" aria-label={`Add agent to ${group.name}`} onClick={() => onAdd(group.rows[0], group.name)}>＋</button> : <span>{group.rows.length}</span>}</header>
    {group.rows.map((row) => <div key={`${row.server.id}:${row.session.id}`}>
      <RemoteAgent row={row} onOpen={onOpen} localRow={row.server.id === activeMachineId ? renderLocal(row) : undefined} />
    </div>)}
  </section>)}</>;
}

function RemoteAgent({ row, onOpen, localRow }: { row: Agent; onOpen: (serverId: string, sessionId: string) => void; localRow?: ReactNode }): JSX.Element {
  const { session, server } = row;
  const [name, setName] = useState<string | null>(null);
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const label = name ?? resolvedSessionLabel(session);
  const provider = normalizeProvider(session.tool);
  const status = classifySession(session);
  const save = async (): Promise<void> => {
    if (busy || !draft.trim()) return;
    setBusy(true); setError(null);
    try {
      setName(await updateSessionName(session.id, draft.trim(), server.id)); setEditing(false);
      if (localRow) void useSessions.getState().refresh().catch(() => undefined);
    }
    catch (reason) { setError(reason instanceof Error ? reason.message : 'Could not rename this agent.'); }
    finally { setBusy(false); }
  };
  return <div className="agent-remote-row">
    {localRow ?? <button type="button" className="session-fleet-row" onClick={() => onOpen(server.id, session.id)} title={`${serverDisplayName(server, true)}${session.profile ? ` · ${session.profile}` : ''}`}>
      <span className="session-fleet-provider">{provider ? <ProviderMark provider={provider} size={18} /> : '⌘'}</span>
      <span className="session-fleet-copy"><strong>{label}</strong><small>{row.unavailable ? 'Computer unavailable · saved view' : status.label}</small></span>
    </button>}
    <button type="button" className="agent-rename" aria-label={`Rename ${label}`} onClick={() => { setDraft(label); setEditing(true); }}>✎</button>
    {editing ? <form className="agent-rename-form" onSubmit={(event) => { event.preventDefault(); void save(); }}>
      <input autoFocus aria-label="Agent name" value={draft} onChange={(event) => setDraft(event.target.value)} disabled={busy} />
      <button type="submit" disabled={busy || !draft.trim()}>Save</button><button type="button" disabled={busy} onClick={() => setEditing(false)}>Cancel</button>
    </form> : null}
    {error ? <p role="alert">{error}</p> : null}
  </div>;
}
