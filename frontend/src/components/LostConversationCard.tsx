import { useState } from 'react';

interface Props {
  providerName: 'Claude' | 'Codex';
  onResume?: () => void | Promise<void>;
  onClose: () => Promise<void>;
}

export function LostConversationCard({ providerName, onResume, onClose }: Props): JSX.Element {
  const [closing, setClosing] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const close = async (): Promise<void> => {
    if (closing) return;
    setClosing(true);
    setError(null);
    try {
      await onClose();
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : 'Sessions could not close this lost record.');
      setClosing(false);
    }
  };

  return (
    <div className="provider-control-card is-lost-conversation" role="group" aria-label="Lost conversation recovery">
      <span className="provider-control-card-title">Conversation saved</span>
      <p className="provider-control-card-text">The runner is gone. Your {providerName} conversation is saved.</p>
      <div className="provider-control-card-choices" role="toolbar" aria-label="Lost conversation actions">
        <button type="button" className="provider-control-card-action is-primary" disabled={!onResume || closing} onClick={() => void onResume?.()}>Resume conversation…</button>
        <button type="button" className="provider-control-card-action" disabled={closing} onClick={() => void close()}>{closing ? 'Closing…' : 'Close'}</button>
      </div>
      {error ? <span className="provider-control-card-hint is-error" role="alert">{error}</span> : null}
    </div>
  );
}
