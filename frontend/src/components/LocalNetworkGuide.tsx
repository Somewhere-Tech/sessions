import { useCallback, useEffect, useRef, useState } from 'react';
import { fetchLANState, requestLocalNetworkAccess, type LANState } from '../api/sessionsd';
import { connectionSettingsTarget } from '../lib/connectionSettingsTarget';
import { isLocalServer, serverDisplayName, useServers } from '../lib/servers';
import { isNativeMobileRuntime, isTauri, openLocalNetworkSettings } from '../lib/tauriBridge';

/** Permission belongs to the host, not necessarily the device displaying this guide. */
export function LocalNetworkGuide({ onState }: { onState?: (state: LANState) => void }): JSX.Element | null {
  useServers((state) => state.activeId);
  const target = connectionSettingsTarget();
  const local = isTauri() && !isNativeMobileRuntime() && isLocalServer(target);
  const name = local ? 'this Mac' : serverDisplayName(target, true);
  const [state, setState] = useState<LANState | null>(null);
  const [message, setMessage] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const hadIssue = useRef(false);
  const checking = useRef(false);
  const onStateRef = useRef(onState);
  onStateRef.current = onState;
  const refresh = useCallback(async (signal: AbortSignal): Promise<void> => {
    const next = await fetchLANState(signal);
    if (signal.aborted) return;
    if (next.permission?.status === 'denied' || next.permission?.status === 'not-yet-asked') hadIssue.current = true;
    setState(next);
    onStateRef.current?.(next);
  }, []);
  useEffect(() => {
    const controller = new AbortController();
    setState(null); setMessage(null); hadIssue.current = false;
    const update = (): void => { void refresh(controller.signal).catch(() => {}); };
    update();
    window.addEventListener('focus', update);
    const interval = window.setInterval(update, 15_000);
    return () => { controller.abort(); window.removeEventListener('focus', update); window.clearInterval(interval); };
  }, [refresh, target.id]);
  const check = async (): Promise<void> => {
    if (checking.current) return;
    checking.current = true; setBusy(true); setMessage(null);
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 8_000);
    try {
      if (local) await requestLocalNetworkAccess(controller.signal);
      await refresh(controller.signal);
      setMessage('Check complete. If access is not confirmed yet, check the switch below, then try again.');
    } catch {
      setMessage('Couldn’t confirm access. Check that Sessions is on in Local Network settings, then try again. Your sessions have not been stopped.');
    } finally {
      window.clearTimeout(timeout); checking.current = false; setBusy(false);
    }
  };
  const openSettings = async (): Promise<void> => {
    try { await openLocalNetworkSettings(); setMessage('After turning on Sessions, come back here and choose Check again.'); }
    catch { setMessage('Open System Settings yourself, then follow the steps below.'); }
  };
  const status = state?.permission?.status;
  if (status === 'granted' && hadIssue.current) return <div className="local-network-guide is-resolved" role="status">Nearby access is working on {name}. You can return to your projects.</div>;
  if (status !== 'denied' && status !== 'not-yet-asked') return null;
  return <section className="local-network-guide" aria-label={`Nearby access on ${name}`}>
    <h2>Let Sessions find your other devices</h2>
    <p>Check Local Network access on <strong>{name}</strong>. It helps Sessions find and connect to computers on the same Wi-Fi. Your saved conversations stay here.</p>
    <ol>
      <li>On {name}, open <strong>System Settings → Privacy &amp; Security → Local Network</strong>.</li>
      <li>Turn on <strong>Sessions</strong>. If it is already on, leave it on.</li>
      <li>Return to Sessions and choose <strong>Check again</strong>.</li>
    </ol>
    <div className="local-network-guide-actions">
      {local ? <button className="btn" type="button" onClick={() => void openSettings()}>Open System Settings</button> : null}
      <button className="btn btn-ghost" type="button" disabled={busy} onClick={() => void check()}>{busy ? 'Checking…' : 'Check again'}</button>
    </div>
    {message ? <p role="status">{message}</p> : null}
    <details><summary>Don’t see Sessions in the list?</summary>
      <p>{local ? 'Choose Check again to ask macOS for access. If a prompt appears, choose Allow.' : `Open Sessions on ${name}, go to Fleet, and choose Check again. Approve the macOS prompt there.`} If no prompt appears, reopen the Sessions app and try again. Closing the app does not stop your agents.</p>
    </details>
  </section>;
}
