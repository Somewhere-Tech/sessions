import { useCallback, useEffect, useRef, useState } from 'react';
import { fetchLANState, requestLocalNetworkAccess, type LANState } from '../api/sessionsd';
import { connectionSettingsTarget } from '../lib/connectionSettingsTarget';
import { localNetworkGuideMode, type LocalNetworkCheck } from '../lib/localNetworkGuide';
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
  const [lastCheck, setLastCheck] = useState<LocalNetworkCheck>('none');
  const [busy, setBusy] = useState(false);
  const sawUnproven = useRef(false);
  const checking = useRef(false);
  const onStateRef = useRef(onState);
  onStateRef.current = onState;
  const refresh = useCallback(async (signal: AbortSignal): Promise<void> => {
    const next = await fetchLANState(signal);
    if (signal.aborted) return;
    if (next.permission?.status === 'denied' || next.permission?.status === 'not-yet-asked') sawUnproven.current = true;
    setState(next);
    onStateRef.current?.(next);
  }, []);
  useEffect(() => {
    const controller = new AbortController();
    setState(null); setMessage(null); setLastCheck('none'); sawUnproven.current = false;
    const update = (): void => { void refresh(controller.signal).catch(() => {}); };
    update();
    window.addEventListener('focus', update);
    const interval = window.setInterval(update, 15_000);
    return () => { controller.abort(); window.removeEventListener('focus', update); window.clearInterval(interval); };
  }, [refresh, target.id]);
  const check = async (): Promise<void> => {
    if (checking.current) return;
    checking.current = true; setBusy(true); setMessage(null); setLastCheck('none');
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 8_000);
    try {
      // Only the local Mac's own daemon can browse for us; from a phone or
      // another machine this check just re-reads what the host last observed,
      // which proves nothing about this moment.
      const reached = local ? await requestLocalNetworkAccess(controller.signal) : null;
      await refresh(controller.signal);
      setLastCheck(reached === null ? 'none' : reached > 0 ? 'reached' : 'none-found');
      setMessage(reached !== null && reached > 0
        ? null
        : 'Check complete. If access is not confirmed yet, check the switch below, then try again.');
    } catch {
      setLastCheck('failed');
      setMessage('Couldn’t confirm nearby access. If Sessions is already on, leave it on. You can still try your saved computer connection in Fleet. Your sessions have not been stopped.');
    } finally {
      window.clearTimeout(timeout); checking.current = false; setBusy(false);
    }
  };
  const openSettings = async (): Promise<void> => {
    try { await openLocalNetworkSettings(); setMessage('After turning on Sessions, come back here and choose Check again.'); }
    catch { setMessage('Open System Settings yourself, then follow the steps below.'); }
  };
  const checkAgain = <button className="btn btn-ghost" type="button" disabled={busy} onClick={() => void check()}>{busy ? 'Checking…' : 'Check again'}</button>;
  const mode = localNetworkGuideMode({ status: state?.permission?.status, sawUnproven: sawUnproven.current, lastCheck });
  if (mode === 'hidden') return null;
  if (mode === 'confirmed') return <div className="local-network-guide is-resolved" role="status">Nearby access is working on {name}. You can return to your projects.</div>;
  if (mode === 'previously-worked') return <div className="local-network-guide" role="status">
    <p>{name} last reported nearby access working. That is its most recent observation, not a live test — macOS does not let Sessions read the setting itself.</p>
    {message ? <p>{message}</p> : null}
    <div className="local-network-guide-actions">{checkAgain}</div>
  </div>;
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
      {checkAgain}
    </div>
    {message ? <p role="status">{message}</p> : null}
    <details><summary>Don’t see Sessions in the list?</summary>
      <p>{local ? 'Choose Check again to ask macOS for access. If a prompt appears, choose Allow.' : `Open Sessions on ${name}, go to Fleet, and choose Check again. Approve the macOS prompt there.`} If no prompt appears, reopen the Sessions app and try again. Closing the app does not stop your agents.</p>
    </details>
    <details><summary>Already on, but the check still fails?</summary>
      <p>A failed network check does not necessarily mean macOS blocked access. Leave the switch on. Try opening a saved computer in Fleet; if it connects, you can keep working. Otherwise, check that both devices are online and use the same private Wi-Fi or Tailscale.</p>
    </details>
  </section>;
}
