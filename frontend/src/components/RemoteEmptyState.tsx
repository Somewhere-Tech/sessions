import type { ReactNode } from 'react';
import { ProviderMark, type Provider as ProviderIdentity } from './ProviderBadge';

interface Props {
  historyPending: boolean;
  providerIdentity: ProviderIdentity;
  providerName: string;
  terminalAvailable: boolean;
  // A pending provider control replaces the "send the first request" hint:
  // the session is waiting on a choice, not on a message.
  control: ReactNode;
}

// What the conversation shows before it has any messages: a loading skeleton
// while history is being read, then either the pending control or the
// invitation to send the first request.
export function RemoteEmptyState({ historyPending, providerIdentity, providerName, terminalAvailable, control }: Props) {
  if (historyPending) {
    return (
      <div className="remote-history-loading" role="status" aria-busy="true" aria-label="Loading conversation history">
        <span className="loading-skeleton is-title" />
        <span className="loading-skeleton is-line" />
        <span className="loading-skeleton is-line is-medium" />
        <div>
          <span className="loading-skeleton is-line" />
          <span className="loading-skeleton is-line is-short" />
        </div>
      </div>
    );
  }
  return (
    <div className="remote-empty">
      <ProviderMark provider={providerIdentity} size={48} />
      {control ?? (
        <>
          <span>Ready</span>
          <h2>Start a {providerName} conversation</h2>
          <p className="remote-empty-hint">
            {terminalAvailable
              ? 'Send the first request below. Conversation and Terminal stay connected to the same provider session.'
              : 'Send the first request below. Rich sessions use the provider’s structured conversation interface.'}
          </p>
        </>
      )}
    </div>
  );
}
