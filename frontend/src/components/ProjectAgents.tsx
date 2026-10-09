import { lazy, Suspense, useId, useState, type ReactNode } from 'react';
import type { AgentProject, Agent } from '../lib/projectAgents';
import { resolvedSessionLabel } from '../lib/tabLabels';
import { classifySession } from '../lib/sessionStatus';
import { serverDisplayName } from '../lib/servers';
import { normalizeProvider, ProviderMark } from './ProviderBadge';
import { updateSessionName } from '../api/sessionsd';
import { useSessions } from '../store/sessions';
import { projectPreview } from '../lib/projectPreview';

const SavedRecoveryAction = lazy(() => import('./SavedRecoveryAction').then((module) => ({ default: module.SavedRecoveryAction })));

interface ProjectAgentsProps {
  groups: AgentProject[];
  activeMachineId: string | null;
  activeSessionId?: string | null;
  compact?: boolean;
  renderLocal: (row: Agent) => ReactNode;
  onOpen: (serverId: string, sessionId: string) => void;
  onAdd?: (row: Agent, project: string) => void;
  onResume?: (row: Agent) => void;
}

export function ProjectAgents(props: ProjectAgentsProps): JSX.Element {
  return <>{props.groups.map((group) => <AgentGroup key={group.id} group={group} {...props} />)}</>;
}

function AgentGroup({ group, activeMachineId, activeSessionId, compact = true, renderLocal, onOpen, onAdd, onResume }: ProjectAgentsProps & { group: AgentProject }): JSX.Element {
  const [open, setOpen] = useState(true);
  const [showAll, setShowAll] = useState(false);
  const rowsId = useId();
  const preview = compact ? projectPreview(group.rows, (row) => row.session,
    (row) => row.server.id === activeMachineId && row.session.id === activeSessionId) : group.rows;
  const remaining = group.rows.length - preview.length;
  const rows = showAll ? group.rows : preview;
  return <section className="agent-project" aria-label={group.name}>
    <header><button type="button" className="session-tree-group-head agent-project-disclosure" aria-expanded={open} aria-controls={rowsId} onClick={() => setOpen((current) => !current)}>
      <span className={`inbox-chevron${open ? ' is-open' : ''}`} aria-hidden>▸</span>
      <span className="agent-project-name">{group.name}</span><strong className="agent-project-count">{group.rows.length}</strong>
    </button>{onAdd ? <button type="button" aria-label={`Add agent to ${group.name}`} onClick={() => onAdd(group.rows[0], group.name)}>＋</button> : null}</header>
    <div id={rowsId} hidden={!open}>{open ? rows.map((row) => <div key={`${row.server.id}:${row.session.id}`}>
      {row.projectName ? <small className="session-nav-project">{row.projectName}</small> : null}
      <RemoteAgent row={row} onOpen={onOpen} onResume={onResume} localRow={row.server.id === activeMachineId ? renderLocal(row) : undefined} />
    </div>) : null}
    {open && remaining > 0 ? <button type="button" className="inbox-fold project-preview-toggle" aria-expanded={showAll} aria-controls={rowsId} onClick={() => setShowAll((current) => !current)}>
      {showAll ? 'Show fewer agents' : `Show ${remaining} remaining agents`}
    </button> : null}</div>
  </section>;
}

function RemoteAgent({ row, onOpen, onResume, localRow }: { row: Agent; onOpen: (serverId: string, sessionId: string) => void; onResume?: (row: Agent) => void; localRow?: ReactNode }): JSX.Element {
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
    {localRow ?? <><button type="button" className="session-fleet-row" onClick={() => onOpen(server.id, session.id)} title={`${serverDisplayName(server, true)}${session.profile ? ` · ${session.profile}` : ''}`}>
      <span className="session-fleet-provider">{provider ? <ProviderMark provider={provider} size={18} /> : '⌘'}</span>
      <span className="session-fleet-copy"><strong>{label}</strong><small>{row.unavailable ? 'Computer unavailable · saved view' : status.label}</small></span>
    </button>{onResume ? <Suspense fallback={null}><SavedRecoveryAction row={row} onResume={onResume} /></Suspense> : null}</>}
    <button type="button" className="agent-rename" aria-label={`Rename ${label}`} onClick={() => { setDraft(label); setEditing(true); }}>✎</button>
    {editing ? <form className="agent-rename-form" onSubmit={(event) => { event.preventDefault(); void save(); }}>
      <input autoFocus aria-label="Agent name" value={draft} onChange={(event) => setDraft(event.target.value)} disabled={busy} />
      <button type="submit" disabled={busy || !draft.trim()}>Save</button><button type="button" disabled={busy} onClick={() => setEditing(false)}>Cancel</button>
    </form> : null}
    {error ? <p role="alert">{error}</p> : null}
  </div>;
}
