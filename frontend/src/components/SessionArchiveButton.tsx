import { useState } from 'react';
import type { SessionInfo } from '../types';
import { resolvedSessionLabel } from '../lib/tabLabels';
import { useSessions } from '../store/sessions';

// Putting a session away, from the session itself.
//
// Archiving already existed — a daemon route, a CLI verb, a row menu item —
// and the person looking at a finished conversation had no way to reach it
// from the thing they were looking at. It is never automatic: nothing here
// runs on a timer, and no session is archived because it went quiet.
//
// A live session cannot be archived, because the daemon only archives durably
// closed records. So the honest control for one says what it will do: end it
// first, and say that the work in progress stops.

interface Props {
  session: SessionInfo;
  /** Called once the daemon has archived it, so the tab can close itself. */
  onArchived?: (id: string) => void;
}

export function SessionArchiveButton({ session, onArchived }: Props): JSX.Element {
  const archive = useSessions((state) => state.archive);
  const endSession = useSessions((state) => state.kill);
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const label = resolvedSessionLabel(session);

  const archiveNow = async (): Promise<void> => {
    if (busy) return;
    setBusy(true);
    setError(null);
    try {
      if (!session.exited) await endSession(session.id, 'Ended from the session header to archive it.');
      const result = await archive([session.id]);
      // The daemon answers per session and can refuse one. Its reason is the
      // only thing that explains why the row is still there, so it is shown
      // rather than summarized as a failure.
      const refused = result.items.find((item) => item.id === session.id && item.status === 'skipped');
      if (refused) {
        setError(refused.reason ?? 'The daemon did not archive this session.');
        return;
      }
      setConfirming(false);
      onArchived?.(session.id);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : 'Sessions could not archive this session.');
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <button
        type="button"
        className="btn btn-ghost session-archive-action"
        disabled={busy}
        title={session.exited
          ? 'Take this conversation out of your list. It stays in History.'
          : 'Stop this session and take it out of your list. It stays in History.'}
        onClick={() => { setError(null); if (session.exited) void archiveNow(); else setConfirming(true); }}
      >
        {busy && session.exited ? 'Archiving…' : session.exited ? 'Archive' : 'End and archive…'}
      </button>
      {error && !confirming ? <span className="session-archive-error" role="alert">{error}</span> : null}
      {confirming ? (
        <div
          className="session-move-sheet session-end-sheet"
          role="presentation"
          onMouseDown={(event) => { if (event.target === event.currentTarget && !busy) setConfirming(false); }}
        >
          <section role="dialog" aria-modal="true" aria-labelledby={`archive-session-title-${session.id}`}>
            <header>
              <div>
                <span>End and archive</span>
                <h2 id={`archive-session-title-${session.id}`}>Put “{label}” away?</h2>
              </div>
              <button type="button" aria-label="Cancel archiving" disabled={busy} onClick={() => setConfirming(false)}>×</button>
            </header>
            <p>
              This session is still running: <strong>work in progress will be interrupted</strong>. Its
              conversation is kept — you can find it in History, marked Archived.
            </p>
            {error ? <div className="session-move-error session-end-error" role="alert">{error}</div> : null}
            <div className="session-end-actions">
              <button type="button" disabled={busy} onClick={() => setConfirming(false)}>Keep working</button>
              <button type="button" className="btn btn-primary" disabled={busy} onClick={() => void archiveNow()}>
                {busy ? 'Ending…' : error ? 'Try again' : 'End and archive'}
              </button>
            </div>
          </section>
        </div>
      ) : null}
    </>
  );
}
