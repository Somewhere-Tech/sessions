import { useRef, useState } from 'react';

interface Props {
  working: boolean;
  onContinue: (remoteControl: boolean) => Promise<void>;
}

/** Visible access to the existing, explicit same-conversation handoff. */
export function ClaudeRuntimeControl({ working, onContinue }: Props): JSX.Element {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const inFlight = useRef(false);
  const continueChat = async (remoteControl: boolean): Promise<void> => {
    if (inFlight.current || working) return;
    inFlight.current = true;
    setBusy(true);
    setError(null);
    try {
      await onContinue(remoteControl);
      setOpen(false);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : 'Could not continue this conversation.');
    } finally {
      inFlight.current = false;
      setBusy(false);
    }
  };
  return <div className="claude-runtime-control">
    <button type="button" className="view-toggle-btn" aria-expanded={open}
      onClick={() => { setOpen(!open); }}>Terminal / Remote Control</button>
    {open ? <section className="claude-runtime-panel" aria-label="Continue Claude conversation">
      <strong>Continue in Claude’s terminal</strong>
      <p>This ends the current runtime and resumes the same saved conversation. The next dialog confirms the exact runtime and permissions. Your unsent draft is kept and will not be sent automatically.</p>
      {working ? <p>Claude reports an active turn. Finish it before switching runtimes.</p> : null}
      <div>
        <button type="button" className="btn" disabled={busy || working} onClick={() => void continueChat(false)}>Continue in Terminal</button>
        <button type="button" className="btn" disabled={busy || working} onClick={() => void continueChat(true)}>Use Remote Control</button>
        <button type="button" className="btn btn-ghost" disabled={busy} onClick={() => setOpen(false)}>Cancel</button>
      </div>
      {error ? <p role="alert">{error}</p> : null}
    </section> : null}
  </div>;
}
