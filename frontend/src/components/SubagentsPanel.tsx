import { useState } from 'react';
import { classifySession } from '../lib/sessionStatus';
import { subagentNeedsReview } from '../lib/workingSet';
import { resolvedSessionLabel } from '../lib/tabLabels';
import type { SessionInfo } from '../types';
import { normalizeProvider, ProviderMark } from './ProviderBadge';

interface Props {
  manager: SessionInfo;
  subagents: SessionInfo[];
  onClose: () => void;
  onOpen: (sessionId: string) => void;
  onMakeMain: (sessionId: string) => Promise<void>;
  onEnd: (sessionId: string) => Promise<void>;
  // Hand a lane's result back to its manager: posts the lane's last line of
  // work into the manager's conversation, attributed to the lane. The lane
  // keeps running; this is a report, not an end.
  onHandBack?: (lane: SessionInfo) => Promise<void>;
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

// lastLine is what a lane most recently said or is waiting for, in one line.
function lastLine(session: SessionInfo): string {
  if (session.idleReason === 'needs-input' && session.idleDetail) return session.idleDetail;
  return session.lastSummary?.trim().split('\n')[0] ?? '';
}

export function SubagentsPanel({ manager, subagents, onClose, onOpen, onMakeMain, onEnd, onHandBack }: Props): JSX.Element {
  const [movingId, setMovingId] = useState<string | null>(null);
  const [handingBackId, setHandingBackId] = useState<string | null>(null);
  const [handedBackId, setHandedBackId] = useState<string | null>(null);
  const [endingId, setEndingId] = useState<string | null>(null);
  const [confirmEndId, setConfirmEndId] = useState<string | null>(null);
  const [reviewInactive, setReviewInactive] = useState(false);
  const [copiedCleanupRequest, setCopiedCleanupRequest] = useState(false);
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
  const inactive = ordered.filter((session) => subagentNeedsReview(session));
  const visible = reviewInactive ? inactive : ordered;

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

  const endSubagent = async (session: SessionInfo): Promise<void> => {
    setEndingId(session.id);
    setError(null);
    try {
      await onEnd(session.id);
      setConfirmEndId(null);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : 'Could not end this subagent.');
    } finally {
      setEndingId(null);
    }
  };

  const handBack = async (session: SessionInfo): Promise<void> => {
    if (!onHandBack) return;
    setHandingBackId(session.id);
    setError(null);
    try {
      await onHandBack(session);
      setHandedBackId(session.id);
      window.setTimeout(() => setHandedBackId((current) => current === session.id ? null : current), 2400);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : 'Could not hand this lane back.');
    } finally {
      setHandingBackId(null);
    }
  };

  const copyCleanupRequest = async (): Promise<void> => {
    const request = 'Clean up your subagents. End only delegated sessions whose work is complete; leave anything active or uncertain running.';
    try {
      await navigator.clipboard.writeText(request);
      setCopiedCleanupRequest(true);
      window.setTimeout(() => setCopiedCleanupRequest(false), 1800);
    } catch {
      setError(`Copy this request to ${resolvedSessionLabel(manager)}: ${request}`);
    }
  };

  return (
    <aside className="subagents-panel" aria-labelledby="subagents-panel-title">
      <header>
        <div>
          <span>Delegated work</span>
          <h2 id="subagents-panel-title">Lanes</h2>
          <p>{ordered.length} total{working ? ` · ${working} working` : ''}{needsYou ? ` · ${needsYou} need you` : ''}</p>
        </div>
        <button type="button" aria-label="Close lanes" onClick={onClose}>×</button>
      </header>
      <div className="subagents-manager-note">Work delegated by <strong>{resolvedSessionLabel(manager)}</strong></div>
      {error ? <div className="subagents-error" role="alert">{error}</div> : null}
      {inactive.length > 0 ? (
        <section className="subagents-review-note">
          <div>
            <strong>{inactive.length} quiet for 24h+</strong>
            <p>Nothing is ended automatically. Review these suggestions or ask the manager to clean up its own subagents.</p>
          </div>
          <div>
            <button type="button" className="btn btn-secondary" onClick={() => setReviewInactive((current) => !current)}>
              {reviewInactive ? 'Show all' : 'Review'}
            </button>
            <button type="button" className="btn btn-ghost" onClick={() => void copyCleanupRequest()}>
              {copiedCleanupRequest ? 'Copied' : 'Copy cleanup request'}
            </button>
          </div>
        </section>
      ) : null}
      <div className="subagents-list">
        {visible.map((session, index) => {
          const status = classifySession(session);
          const provider = normalizeProvider(session.tool);
          return (
            <article className={`subagent-card ${status.className}`} key={session.id}>
              <div className="subagent-card-head">
                <span className={`subagent-status ${status.className}`} aria-hidden />
                <div>
                  <strong>{index + 1}. {resolvedSessionLabel(session)}</strong>
                  <small>{status.label}{relativeTime(activityAt(session)) ? ` · ${relativeTime(activityAt(session))}` : ''}</small>
                </div>
                {provider ? <ProviderMark provider={provider} size={24} /> : <span className="subagent-shell" title="Shell">⌘</span>}
              </div>
              <p>{purpose(session)}</p>
              {lastLine(session) && lastLine(session) !== purpose(session) ? (
                <p className={`subagent-last${status.needsYou ? ' is-attention' : ''}`}>{status.needsYou ? 'Waiting: ' : ''}{lastLine(session)}</p>
              ) : null}
              <div className="subagent-card-actions">
                <button type="button" className="btn btn-secondary" onClick={() => onOpen(session.id)}>Open</button>
                {onHandBack && !session.exited ? (
                  <button type="button" className="btn btn-ghost" disabled={handingBackId !== null} onClick={() => void handBack(session)} title="Post this lane's latest result into the manager's conversation">
                    {handingBackId === session.id ? 'Handing back…' : handedBackId === session.id ? 'Handed back' : 'Hand back'}
                  </button>
                ) : null}
                <button type="button" className="btn btn-ghost" disabled={movingId !== null} onClick={() => void makeMain(session)}>
                  {movingId === session.id ? 'Moving…' : 'Make main session'}
                </button>
                {reviewInactive && subagentNeedsReview(session) ? (
                  confirmEndId === session.id ? (
                    <span className="subagent-end-confirm">
                      <button type="button" className="btn btn-secondary" disabled={endingId !== null} onClick={() => void endSubagent(session)}>
                        {endingId === session.id ? 'Ending…' : 'End now'}
                      </button>
                      <button type="button" className="btn btn-ghost" onClick={() => setConfirmEndId(null)}>Cancel</button>
                    </span>
                  ) : (
                    <button type="button" className="btn btn-ghost" onClick={() => setConfirmEndId(session.id)}>End…</button>
                  )
                ) : null}
              </div>
            </article>
          );
        })}
      </div>
      <footer>User-driven sessions are permanent. Lanes stay searchable and resumable after you end them.</footer>
    </aside>
  );
}
