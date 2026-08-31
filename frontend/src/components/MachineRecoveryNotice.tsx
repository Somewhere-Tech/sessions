interface MachineRecoveryNoticeProps {
  machine: string;
  local: boolean;
  discovering: boolean;
  recovered: number;
  expected: number;
  paused?: number;
  busy: boolean;
  detail?: string | null;
  localAlternative?: string;
  onRecover: () => void;
  onReview?: () => void;
  onStartLocal?: () => void;
}

export function MachineRecoveryNotice({
  machine,
  local,
  discovering,
  recovered,
  expected,
  paused = 0,
  busy,
  detail,
  localAlternative,
  onRecover,
  onReview,
  onStartLocal
}: MachineRecoveryNoticeProps): JSX.Element {
  const hasProgress = discovering && expected > 0;
  const reviewingPaused = !discovering && !detail && paused > 0;
  const progress = hasProgress ? Math.min(100, Math.max(3, (recovered / expected) * 100)) : 0;
  const title = reviewingPaused
    ? `${paused} ${paused === 1 ? 'session stayed' : 'sessions stayed'} paused after restart`
    : discovering
    ? `Recovering sessions on ${machine}`
    : local
      ? `Reconnecting Sessions on ${machine}`
      : `${machine} is offline`;
  const message = reviewingPaused
    ? `Sessions limited automatic recovery on ${machine} so login could not restart the whole retained fleet. Conversation history is preserved; resume only the work you want.`
    : discovering
    ? hasProgress
      ? `${recovered} of ${expected} saved sessions are back in view. Agents are not being restarted.`
      : 'Sessions is rebuilding this machine’s list. Agent processes keep running separately from this window.'
    : local
      ? 'The background service stopped responding. Sessions is repairing it and will reconnect to surviving agents automatically.'
      : 'Showing this machine’s last-known sessions while Sessions keeps checking the connection.';

  return (
    <section className="machine-recovery-notice" role="status" aria-live="polite">
      <span className={`machine-recovery-mark${busy || discovering ? ' is-active' : ''}`} aria-hidden />
      <div className="machine-recovery-copy">
        <strong>{title}</strong>
        <span>{message}</span>
        {hasProgress ? (
          <span className="machine-recovery-progress" aria-label={`${recovered} of ${expected} sessions recovered`}>
            <span style={{ width: `${progress}%` }} />
          </span>
        ) : null}
        {detail ? <details><summary>Technical details</summary><p>{detail}</p></details> : null}
      </div>
      <div className="machine-recovery-actions">
        <button type="button" className="btn" onClick={reviewingPaused && onReview ? onReview : onRecover} disabled={busy}>
          {reviewingPaused ? 'Review sessions' : busy ? 'Recovering…' : local ? 'Recover sessions' : 'Check again'}
        </button>
        {!local && localAlternative && onStartLocal ? (
          <button type="button" className="btn btn-primary" onClick={onStartLocal}>Start on {localAlternative}</button>
        ) : null}
      </div>
    </section>
  );
}
