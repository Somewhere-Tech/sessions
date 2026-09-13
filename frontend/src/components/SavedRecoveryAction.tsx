import type { Agent } from '../lib/projectAgents';
import { canContinueSession, lostSessionNote } from '../lib/sessionStatus';

export function SavedRecoveryAction({ row, onResume }: {
  row: Agent;
  onResume: (row: Agent) => void;
}): JSX.Element {
  const canResume = !row.unavailable && canContinueSession(row.session);
  const reason = lostSessionNote(row.session)
    || (row.session.unreachableReason === 'restart-restore-pending'
      ? 'Paused after this computer restarted.'
      : 'Saved conversation.');
  return <span className="saved-recovery-action">
    <small className="conversation-row-note">{reason}</small>
    {canResume ? <button type="button" className="session-row-continue" onClick={() => onResume(row)}>Resume <span aria-hidden>→</span></button> : null}
    {!canResume ? <small className="conversation-row-note">{row.unavailable ? 'Reconnect this computer to resume.' : 'No resumable provider conversation is available.'}</small> : null}
  </span>;
}
