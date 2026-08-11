import { useState } from 'react';
import { classifySession } from '../lib/sessionStatus';
import { sessionLabel } from '../lib/tabLabels';
import type { SessionInfo } from '../types';
import { normalizeProvider, ProviderMark } from './ProviderBadge';

interface Props {
  manager: SessionInfo;
  subagents: SessionInfo[];
  onClose: () => void;
  onOpen: (sessionId: string) => void;
  onMakeMain: (sessionId: string) => Promise<void>;
}

function relativeTime(value: number): string {
  if (!value) return '';
  const seconds = Math.max(0, Math.floor((Date.now() - value) / 1000));
  if (seconds < 60) return 'now';
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h`;
  return `${Math.floor(hours / 24)}d`;
}

function purpose(session: SessionInfo): string {
  const description = session.description?.trim().split('\n')[0];
  if (description) return description;
  const summary = session.lastSummary?.trim().split('\n')[0];
  if (summary) return summary;
  const workspace = session.cwd.split('/').filter(Boolean).pop();
  return workspace ? `Working in ${workspace}` : 'No purpose recorded yet.';
}

function activityAt(session: SessionInfo): number {
  return Math.max(session.lastDataAt || 0, session.idleSince || 0, session.createdAt || 0);
}

export function SubagentsPanel({ manager, subagents, onClose, onOpen, onMakeMain }: Props): JSX.Element {
  const [movingId, setMovingId] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const ordered = [...subagents].sort((left, right) => {
    const leftStatus = classifySession(left);
    const rightStatus = classifySession(right);
    const rank = (status: ReturnType<typeof classifySession>): number => (
      status.needsYou ? 0 : status.state === 'failed' ? 1 : status.state === 'working' ? 2 : status.finished ? 4 : 3
    );
    return rank(leftStatus) - rank(rightStatus) || activityAt(right) - activityAt(left);
  });
  const working = ordered.filter((session) => !session.exited && session.working).length;
  const needsYou = ordered.filter((session) => classifySession(session).needsYou).length;

  const makeMain = async (session: SessionInfo): Promise<void> => {
    setMovingId(session.id);
    setError(null);
    try {
      await onMakeMain(session.id);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : 'Could not make this a main session.');
    } finally {
      setMovingId(null);
    }
  };

  return (
    <aside className="subagents-panel" aria-labelledby="subagents-panel-title">
      <header>
        <div>
          <span>Delegated work</span>
          <h2 id="subagents-panel-title">Subagents</h2>
          <p>{ordered.length} total{working ? ` · ${working} working` : ''}{needsYou ? ` · ${needsYou} need you` : ''}</p>
        </div>
        <button type="button" aria-label="Close subagents" onClick={onClose}>×</button>
      </header>
      <div className="subagents-manager-note">Work delegated by <strong>{sessionLabel(manager)}</strong></div>
      {error ? <div className="subagents-error" role="alert">{error}</div> : null}
      <div className="subagents-list">
        {ordered.map((session, index) => {
          const status = classifySession(session);
          const provider = normalizeProvider(session.tool);
          return (
            <article className={`subagent-card ${status.className}`} key={session.id}>
              <div className="subagent-card-head">
                <span className={`subagent-status ${status.className}`} aria-hidden />
                <div>
                  <strong>{index + 1}. {sessionLabel(session)}</strong>
                  <small>{status.label}{relativeTime(activityAt(session)) ? ` · ${relativeTime(activityAt(session))}` : ''}</small>
                </div>
                {provider ? <ProviderMark provider={provider} size={24} /> : <span className="subagent-shell" title="Shell">⌘</span>}
              </div>
              <p>{purpose(session)}</p>
              <div className="subagent-card-actions">
                <button type="button" className="btn btn-secondary" onClick={() => onOpen(session.id)}>Open</button>
                <button type="button" className="btn btn-ghost" disabled={movingId !== null} onClick={() => void makeMain(session)}>
                  {movingId === session.id ? 'Moving…' : 'Make main session'}
                </button>
              </div>
            </article>
          );
        })}
      </div>
      <footer>Subagents stay searchable and keep their creator history even when hidden here.</footer>
    </aside>
  );
}
