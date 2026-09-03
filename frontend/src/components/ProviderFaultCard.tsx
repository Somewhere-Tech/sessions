import { useEffect, useState } from 'react';
import { retryProviderSession, stopProviderRetry } from '../api/sessionsd';
import type { ProviderFailureKind, ProviderRetry } from '../types';

interface Props {
  sessionId: string;
  failureKind: ProviderFailureKind;
  detail?: string;
  retry?: ProviderRetry;
  rich: boolean;
  onOpenTerminal: () => void;
}

function retryCountdown(nextAt: number, now: number): number {
  return Math.max(0, Math.ceil((nextAt - now) / 1000));
}

export function ProviderFaultCard({ sessionId, failureKind, detail, retry, rich, onOpenTerminal }: Props): JSX.Element {
  const [now, setNow] = useState(Date.now());
  const [busy, setBusy] = useState<'retry' | 'stop' | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!retry) return;
    setNow(Date.now());
    const timer = window.setInterval(() => setNow(Date.now()), 1_000);
    return () => window.clearInterval(timer);
  }, [retry]);

  const act = async (action: 'retry' | 'stop'): Promise<void> => {
    if (busy) return;
    setBusy(action);
    setError(null);
    try {
      if (action === 'retry') await retryProviderSession(sessionId);
      else await stopProviderRetry(sessionId);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : 'Sessions could not change this retry.');
    } finally {
      setBusy(null);
    }
  };

  const guidance = failureKind === 'auth'
    ? 'Open the terminal to log in'
    : retry
      ? `Retrying in ${retryCountdown(retry.nextAt, now)}s (attempt ${retry.attempt} of ${retry.max})`
      : rich
        ? 'Retry'
        : 'Send your message again when the provider is back';

  return (
    <div className="provider-control-card is-provider-fault" role="group" aria-label="Provider trouble">
      <span className="provider-control-card-title">Provider trouble</span>
      <p className="provider-control-card-text">{detail || 'The provider did not complete this turn.'}</p>
      <span className="provider-control-card-hint" aria-live="polite">{guidance}</span>
      <div className="provider-control-card-choices" role="toolbar" aria-label="Provider recovery">
        {failureKind === 'auth' ? (
          <button type="button" className="provider-control-card-action is-primary" onClick={onOpenTerminal}>Open Terminal</button>
        ) : rich ? (
          <button type="button" className="provider-control-card-action is-primary" disabled={busy !== null} onClick={() => void act('retry')}>
            {busy === 'retry' ? 'Retrying…' : 'Retry now'}
          </button>
        ) : null}
        {retry ? (
          <button type="button" className="provider-control-card-action" disabled={busy !== null} onClick={() => void act('stop')}>
            {busy === 'stop' ? 'Stopping…' : 'Stop retrying'}
          </button>
        ) : null}
      </div>
      {error ? <span className="provider-control-card-hint is-error" role="alert">{error}</span> : null}
    </div>
  );
}
