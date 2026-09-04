import { useEffect, useState } from 'react';
import { fetchServerHistoryTranscript, forkConversation } from '../api/sessionsd';
import { getActiveServer } from '../lib/servers';
import { preferNextSessionView } from '../lib/sessionViewPreference';
import { formatTokenEstimate, usePaidStartPlan, type PaidStartProvider } from '../hooks/usePaidStartPlan';
import { useSessions } from '../store/sessions';
import type { SessionInfo } from '../types';
import { PaidStartPlan, paidStartProviderName } from './PaidStartPlan';

export interface ForkPoint {
  index: number;
  messageId: string;
}

interface Props {
  session: SessionInfo;
  destinationProvider: PaidStartProvider;
  point?: ForkPoint;
  onClose: () => void;
  onStarted: (sessionId: string) => void;
}

interface ForkSize {
  messages: number;
  tokens: number;
}

function sourceProvider(session: SessionInfo): PaidStartProvider {
  return session.tool === 'codex' ? 'codex' : 'claude';
}

function sizeThroughPoint(
  messages: Awaited<ReturnType<typeof fetchServerHistoryTranscript>>['messages'],
  point?: ForkPoint
): ForkSize {
  const selected = messages.filter((message) => (
    (message.role === 'user' || message.role === 'assistant')
    && message.text.trim() !== ''
    && (!point || message.index <= point.index)
  ));
  if (point && !selected.some((message) => message.index === point.index && message.id === point.messageId)) {
    throw new Error('The selected message changed. Close this review and choose the fork point again.');
  }
  const characters = selected.reduce((total, message) => total + [...message.text].length, 0);
  return { messages: selected.length, tokens: Math.ceil(characters / 4) };
}

function useForkSize(sessionId: string, point?: ForkPoint): {
  size: ForkSize | null;
  loading: boolean;
  error: string | null;
} {
  const [size, setSize] = useState<ForkSize | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError(null);
    void fetchServerHistoryTranscript(getActiveServer(), sessionId, controller.signal)
      .then((transcript) => setSize(sizeThroughPoint(transcript.messages, point)))
      .catch((reason: unknown) => {
        if (!controller.signal.aborted) setError(reason instanceof Error ? reason.message : 'Could not measure this conversation.');
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [point, sessionId]);
  return { size, loading, error };
}

export function ForkConfirmationDialog({
  session,
  destinationProvider,
  point,
  onClose,
  onStarted
}: Props): JSX.Element {
  const refresh = useSessions((state) => state.refresh);
  const provider = sourceProvider(session);
  const plan = usePaidStartPlan({
    sourceProvider: provider,
    preferredDestination: destinationProvider,
    preferredRuntime: 'rich',
    terminalAvailable: false
  });
  const measured = useForkSize(session.id, point);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const start = async (): Promise<void> => {
    if (busy || !plan.ready || !measured.size) return;
    setBusy(true);
    setError(null);
    try {
      const result = await forkConversation(
        session.id,
        plan.destination,
        point,
        plan.model,
        plan.effort,
        'constrained'
      );
      await refresh();
      preferNextSessionView(result.laneId, 'remote');
      onStarted(result.laneId);
      onClose();
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : 'Could not start this copy.');
    } finally {
      setBusy(false);
    }
  };
  const sizeLine = measured.loading
    ? 'Measuring the conversation…'
    : measured.size
      ? `${measured.size.messages} messages, about ${formatTokenEstimate(measured.size.tokens)} tokens`
      : 'Conversation size unavailable';
  return (
    <div className="dialog-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget && !busy) onClose(); }}>
      <section className="dialog dialog-wide paid-start-dialog" role="dialog" aria-modal="true" aria-labelledby="fork-confirmation-title">
        <header className="dialog-header">
          <div><span className="dialog-kicker">Review before starting</span><h2 id="fork-confirmation-title">Fork this conversation</h2></div>
          <button type="button" className="dialog-close" aria-label="Close" disabled={busy} onClick={onClose}>×</button>
        </header>
        <PaidStartPlan
          plan={plan}
          title={session.name?.trim() || session.description?.trim() || 'Conversation copy'}
          sizeLine={sizeLine}
          intro={`${paidStartProviderName(plan.destination)} starts an independent copy${point ? ' through the message you chose' : ''}.`}
          sourceNote={session.exited ? 'The saved original stays unchanged.' : 'The original conversation keeps running.'}
          copyNote="Only your messages and the agent’s replies are copied. Tool output, file changes, attachments, sign-in details, usage totals, and the agent’s behind-the-scenes records stay out."
          disabled={busy}
        />
        {measured.error ? <div className="dialog-error" role="alert">{measured.error}</div> : null}
        {error ? <div className="dialog-error" role="alert">{error}</div> : null}
        <footer className="paid-start-dialog-actions">
          <button type="button" className="btn btn-ghost" disabled={busy} onClick={onClose}>Cancel</button>
          <button type="button" className="btn btn-primary continuation-start" disabled={busy || !plan.ready || !measured.size || Boolean(measured.error)} onClick={() => void start()}>
            {busy ? 'Starting…' : plan.ready ? `Start ${paidStartProviderName(plan.destination)} (${plan.modelName})` : 'Preparing details…'}
          </button>
        </footer>
      </section>
    </div>
  );
}
