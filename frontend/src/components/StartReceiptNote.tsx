import type { SessionInfo } from '../types';
import { startNotice } from '../lib/startReceipt';

// How far a delegated start provably got, as one header line. Loaded lazily:
// it only has something to say while a start is stalled or blocked.
export function StartReceiptNote({ session, onOpenAccounts }: { session: SessionInfo; onOpenAccounts?: () => void }): JSX.Element | null {
  const start = startNotice(session);
  if (!start) return null;
  return (
    <span className={`session-start-note ${start.tone === 'attention' ? 'is-fault' : 'is-progress'}`} role="status" title={session.start?.evidence}>
      {start.text}
      {start.connectAccount && onOpenAccounts ? <button type="button" className="session-start-note-action" onClick={onOpenAccounts}>Connect account</button> : null}
    </span>
  );
}
