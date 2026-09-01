import { useState } from 'react';
import type { SessionInfo } from '../types';
import { getActiveServer, serverDisplayName } from '../lib/servers';
import { resolvedSessionLabel } from '../lib/tabLabels';
import { canContinueSession, classifySession, endedAtLabel, endedSummary, type SessionStatusState } from '../lib/sessionStatus';
import { sessionModeName } from '../lib/sessionMode';
import { ProviderBadge, normalizeProvider } from './ProviderBadge';
import { useSessions } from '../store/sessions';
import { PIN_UNAVAILABLE_WHEN_ENDED } from '../lib/workingSet';

// Long-form expansions of the classifier's one-word state. Same ordering,
// same meaning — this view has room for a sentence, not a different answer.
const STATE_DETAIL: Record<SessionStatusState, string> = {
  reconnecting: 'Sessions is reconnecting to this agent; its work keeps running',
  unavailable: 'Sessions cannot currently reach this agent; its saved conversation remains available',
  'needs-recovery': 'This agent stayed paused after restart; resume it when you are ready',
  failed: 'The runtime stopped unexpectedly; saved history is still available',
  ended: 'The runtime is no longer running',
  'needs-you': 'Waiting for your response',
  working: 'Agent is working',
  limited: 'The agent is alive; one optional capability is unavailable',
  finished: 'The provider run finished; the runtime is still live',
  'not-started': 'The runtime has not produced output yet',
  ready: 'Ready'
};

interface Props {
  session: SessionInfo;
  allSessions: SessionInfo[];
  onEnd: (id: string, reason?: string) => Promise<void>;
  onResume?: (session: SessionInfo) => void | Promise<void>;
}

export function SessionDetails({ session, allSessions, onEnd, onResume }: Props): JSX.Element {
  const activeServer = getActiveServer();
  const [confirming, setConfirming] = useState(false);
  const [endReason, setEndReason] = useState('');
  // `onEnd` rejects when the daemon refuses. Swallowing it left the panel
  // claiming nothing had happened; an unresolved end must stay visible.
  const [ending, setEnding] = useState(false);
  const [endError, setEndError] = useState<string | null>(null);
  // The pin is daemon state, so the panel reports what the daemon stored and
  // says so when the call fails. Flipping the checkbox optimistically would
  // show a protection the session does not actually have.
  const updatePinned = useSessions((state) => state.updatePinned);
  const [pinning, setPinning] = useState(false);
  const [pinError, setPinError] = useState<string | null>(null);
  const displayParentID = session.displayParentSessionId !== undefined
    ? session.displayParentSessionId
    : session.parentSessionId;
  const parent = displayParentID ? allSessions.find((item) => item.id === displayParentID) : null;
  const children = allSessions.filter((item) => (
    item.displayParentSessionId !== undefined ? item.displayParentSessionId : item.parentSessionId
  ) === session.id);
  const value = (content: string | number | null | undefined): string => content === null || content === undefined || content === '' ? '—' : String(content);
  const provider = normalizeProvider(session.tool);
  const end = session.exited ? endedSummary(session, allSessions) : null;
  // Details is the one surface allowed to expand on the classifier's word,
  // because it is a full record view — but the word itself still comes from
  // the classifier, so it can never contradict the row the user clicked.
  const status = classifySession(session);
  const degraded = status.degraded;
  const currentStatus = end ? end.label : status.label;
  const togglePin = async (pinned: boolean): Promise<void> => {
    if (pinning) return;
    setPinning(true);
    setPinError(null);
    try {
      await updatePinned(session.id, pinned);
    } catch (reason) {
      setPinError(reason instanceof Error ? reason.message : 'Sessions could not change the pin.');
    } finally {
      setPinning(false);
    }
  };
  const endNow = async (): Promise<void> => {
    if (ending) return;
    setEnding(true);
    setEndError(null);
    try {
      await onEnd(session.id, endReason.trim());
      setConfirming(false);
    } catch (reason) {
      setEndError(reason instanceof Error ? reason.message : 'Sessions could not end this session.');
    } finally {
      setEnding(false);
    }
  };

  return (
    <div className="session-details-view">
      <section className="details-grid">
        <DetailsCard title="Session">
          <Row label="Status" value={currentStatus} />
          <Row label="How this session runs" value={sessionModeName(session)} />
          {degraded ? <Row label="Unavailable" value={(session.idleDetail || 'A provider integration did not start completely.').replace(/^⚠\s*/, '')} /> : null}
          <div className="details-provider-row"><span>Agent</span>{provider ? <ProviderBadge provider={provider} /> : <strong>Shell</strong>}</div>
          <Row label="Account" value={session.profile || 'Default account'} />
          <Row label="Model" value={session.model || 'Provider default'} />
          <Row label="Effort" value={session.effort || 'Provider default'} />
          {/*
            * The pin is offered here and in the navigator's row menu, through
            * the same store action, and on an ended session both surfaces say
            * the same thing rather than one hiding the control and the other
            * disabling it. Hiding it here used to make the mark look absent on
            * exactly the records where a user most wants to know what happened
            * to it — including a session that was pinned before it ended.
            */}
          <label className="settings-toggle details-pin-toggle">
            <span>
              <strong>Pinned</strong>
              <small>
                {session.exited
                  ? PIN_UNAVAILABLE_WHEN_ENDED
                  : 'Marks this as a user-driven workbench, keeps it at the top of every list, and excludes it from delegated-work cleanup suggestions.'}
              </small>
            </span>
            <input
              type="checkbox"
              checked={session.pinned === true}
              disabled={pinning || session.exited}
              aria-label="Pin this session"
              onChange={(event) => void togglePin(event.currentTarget.checked)}
            />
          </label>
          {pinError ? <p className="session-control-error" role="alert">{pinError}</p> : null}
        </DetailsCard>
        <DetailsCard title="Workspace">
          <Row label="Folder" value={session.cwd}/>
          <Row label="Branch" value={value(session.branch)}/>
          <Row label="Worktree" value={session.worktreePath ? 'Isolated worktree' : 'Original workspace'}/>
          {session.sourceRepo ? <Row label="Repository" value={session.sourceRepo}/> : null}
        </DetailsCard>
        <DetailsCard title="Runtime">
          <Row label="Machine" value={serverDisplayName(activeServer, true)}/>
          <Row label="Started" value={new Date(session.createdAt).toLocaleString()}/>
          {end ? (
            <>
              <Row label="Ended" value={endedAtLabel(session)}/>
              <Row label="How it ended" value={end.label}/>
              <Row label="Reason" value={session.endReason?.trim() || 'No reason supplied'}/>
              {session.endOperationId ? <Row label="Batch action" value="Yes · ended with other sessions"/> : null}
            </>
          ) : <Row label="Current state" value={STATE_DETAIL[status.state]}/>}
          <Row label="Recovery" value={session.provenanceStatus === 'lost' ? 'Runner connection lost' : 'History is tracked'}/>
        </DetailsCard>
        <DetailsCard title="Relationships">
          <Row label="Parent" value={parent ? resolvedSessionLabel(parent) : 'Top-level session'}/>
          <Row label="Children" value={`${children.filter((child) => !child.exited).length} running · ${children.filter((child) => child.exited).length} ended`}/>
          <Row label="Created by" value={session.creatorKind === 'session' ? 'Another session' : session.creatorKind === 'external' ? 'An outside agent or CLI' : 'You'}/>
          {session.resumedFrom ? <Row label="Continued from" value={`Session ${session.resumedFrom.slice(0, 8)}`} /> : null}
          {session.reopenedAs ? <Row label="Continued as" value={`Session ${session.reopenedAs.slice(0, 8)}`} /> : null}
          {session.movedFromSessionId ? <Row label="Continued from machine" value={`${session.movedFromEndpoint || 'Another machine'} · ${session.movedFromSessionId.slice(0, 8)}`} /> : null}
          {session.movedToSessionId ? <Row label="Continued on machine" value={`${session.movedToEndpoint || 'Another machine'} · ${session.movedToSessionId.slice(0, 8)}`} /> : null}
          {session.continuedFromHistoryId ? (
            <>
              <Row label="Original agent" value={session.continuedFromProvider === 'codex' ? 'Codex' : 'Claude'} />
              <Row label="Imported context" value={
                session.continuationMode === 'native-import'
                  ? `${session.importedMessageCount ?? 0} authored messages · native Codex history`
                  : `${session.importedMessageCount ?? 0} authored messages · linked local history`
              } />
            </>
          ) : null}
        </DetailsCard>
        <DetailsCard title="Usage"><p className="details-note">Turn-level usage appears with the conversation. The Usage page has totals by provider, project, and tag.</p></DetailsCard>
      </section>
      <details className="details-technical">
        <summary>Technical details</summary>
        <div className="details-grid">
          <DetailsCard title="Runtime identifiers"><Row label="Session ID" value={session.id}/><Row label="System name" value={activeServer.systemName || activeServer.host}/><Row label="Process ID" value={session.pid || 'Not running'}/><Row label="Runner" value={session.runnerVersion || 'Bundled runtime'}/><Row label="Terminal size" value={`${session.cols} × ${session.rows}`}/></DetailsCard>
          <DetailsCard title="Launch configuration"><Row label="Command" value={[session.cmd, ...session.args].join(' ')}/><Row label="Config folder" value={value(session.configDir)}/><Row label="Idle action" value={value(session.onIdle)}/><Row label="Ledger" value={session.provenanceStatus || 'Verified'}/></DetailsCard>
        </div>
      </details>
      <section className="session-control">
        <div>
          <span>Session control</span>
          <h2>{session.exited ? `${end?.label ?? 'Ended'} · ${endedAtLabel(session)}` : 'End session'}</h2>
          <p>{session.exited ? 'The conversation is kept as read-only history.' : 'Stops the agent now. The conversation is kept and you can resume it later.'}</p>
        </div>
        {session.exited ? (
          canContinueSession(session) ? <button type="button" className="btn btn-primary" onClick={() => onResume?.(session)}>Resume</button> : null
        ) : confirming ? (
          <div className="session-control-confirm">
            <p><strong>End this session?</strong> It will stop running. Its conversation is kept, and you can resume it later from Recently ended.</p>
            <input value={endReason} maxLength={280} onChange={(event) => setEndReason(event.currentTarget.value)} placeholder="Why are you ending it? (optional)" aria-label="Reason for ending session" />
            {endError ? <p className="session-control-error" role="alert">{endError}</p> : null}
            <div className="session-control-actions">
              <button type="button" className="btn btn-ghost" disabled={ending} onClick={() => { setConfirming(false); setEndError(null); }}>Cancel</button>
              <button type="button" className="btn btn-secondary" disabled={ending} onClick={() => void endNow()}>
                {ending ? 'Ending…' : endError ? 'Try again' : 'End session'}
              </button>
            </div>
          </div>
        ) : <button type="button" className="btn btn-secondary" onClick={() => setConfirming(true)}>End session</button>}
      </section>
    </div>
  );
}

function DetailsCard({ title, children }: { title: string; children: React.ReactNode }): JSX.Element { return <article className="details-card"><h2>{title}</h2><div>{children}</div></article>; }
function Row({ label, value }: { label: string; value: string | number }): JSX.Element { return <div className="details-row"><span>{label}</span><strong title={String(value)}>{value}</strong></div>; }
