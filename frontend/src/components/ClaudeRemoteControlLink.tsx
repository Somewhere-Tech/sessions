import { useState } from 'react';
import { previewRestart, type RestartPreview } from '../api/sessionsd/restart';

/** Read saved provider facts on demand; never label a recorded link Connected. */
export function ClaudeRemoteControlLink({ sessionId, serverId }: { sessionId: string; serverId: string }): JSX.Element {
  const [preview, setPreview] = useState<RestartPreview | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const inspect = async (): Promise<void> => {
    setBusy(true); setError(null);
    try { setPreview(await previewRestart(sessionId, serverId)); }
    catch (reason) { setError(`Could not read this computer's Claude connection: ${String(reason)}`); }
    finally { setBusy(false); }
  };
  return <div className="claude-runtime-control">
    <button type="button" className="view-toggle-btn" disabled={busy} onClick={() => void inspect()}>{busy ? 'Checking Claude link…' : 'Claude app connection'}</button>
    {preview ? <section className="claude-runtime-panel" aria-label="Recorded Claude connection">
      {preview.remoteUrl ? <a href={preview.remoteUrl}>Open recorded Claude link ↗</a> : <p>No Remote Control link was found in recent history. Check Claude’s Terminal view for its connection status.</p>}
      {preview.savedLoginEmail ? <p>Saved sign-in: {preview.savedLoginEmail}</p> : null}
      <p>This is saved information, not proof that Claude is connected. Another login may use a different link.</p>
      {preview.warning ? <p>{preview.warning}</p> : null}
    </section> : null}
    {error ? <p role="alert">{error}</p> : null}
  </div>;
}
