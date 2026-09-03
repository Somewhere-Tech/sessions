import { useEffect, useRef, useState } from 'react';
import {
  fetchAISettings,
  fetchRecapSettings,
  getPushVapidPublicKey,
  subscribePush,
  unsubscribePush,
  updateAISettings,
  updateRecapSettings,
  type AIProvider,
  type AISettings,
  type RecapProvider,
  type RecapSettings
} from '../api/sessionsd';
import { type TextSize, nextSize, sizeLabel } from '../lib/textSize';
import { useServers } from '../lib/servers';
import {
  NEW_SESSION_DIMENSIONS,
  isNewSessionTool,
  normalizeNewSessionDefaults,
  readNewSessionDefaults,
  type NewSessionDefaults,
  type NewSessionTool,
  writeNewSessionDefaults
} from '../lib/newSessionDefaults';
import { ServerSelector } from './ServerSelector';
import { TagEditor } from './TagEditor';
import { claimCurrentOriginPairing, claimNativeMachinePairing } from '../lib/hostedBootstrap';
import {
  checkForNativeUpdate,
  installNativeUpdate,
  isTauri,
  type NativeUpdateInfo,
  type NativeUpdateProgress
} from '../lib/tauriBridge';
import { useSessions } from '../store/sessions';

interface Props {
  clientOnly?: boolean;
  hostName?: string;
  textSize: TextSize;
  onTextSizeChange: (size: TextSize) => void;
  onNewSession?: () => void;
  onOpenConnections?: () => void;
}

const PUSH_ENABLED_KEY = 'sessions:push-enabled';
const NEW_SESSION_TOOL_OPTIONS: { id: NewSessionTool; label: string }[] = [
  { id: 'claude-code', label: 'Claude Code' },
  { id: 'codex', label: 'Codex' },
  { id: 'shell', label: 'Shell' }
];

function readPushEnabled(): boolean {
  try {
    return window.localStorage.getItem(PUSH_ENABLED_KEY) === '1';
  } catch {
    return false;
  }
}

function writePushEnabled(enabled: boolean): void {
  try {
    if (enabled) window.localStorage.setItem(PUSH_ENABLED_KEY, '1');
    else window.localStorage.removeItem(PUSH_ENABLED_KEY);
  } catch { /* ignore */ }
}

function urlBase64ToUint8Array(base64: string): Uint8Array<ArrayBuffer> {
  const padding = '='.repeat((4 - (base64.length % 4)) % 4);
  const normalized = `${base64}${padding}`.replace(/-/g, '+').replace(/_/g, '/');
  const raw = window.atob(normalized);
  const output = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) {
    output[i] = raw.charCodeAt(i);
  }
  return output;
}

async function getPushRegistration(): Promise<ServiceWorkerRegistration> {
  const scope = new URL('.', document.baseURI);
  const existing = await navigator.serviceWorker.getRegistration(scope.href);
  if (existing) return existing;
  if (import.meta.env.DEV) {
    throw new Error('Requires daemon-hosted HTTPS (enable tailscale serve)');
  }
  return navigator.serviceWorker.register(new URL('sw.js', scope), { scope: scope.pathname });
}

// Settings popover anchored to a header button.
export function SettingsMenu({ clientOnly = false, hostName = 'this computer', textSize, onTextSizeChange, onNewSession, onOpenConnections }: Props): JSX.Element {
  const activeServerId = useServers((state) => state.activeId);
  const activeServerIsLocal = useServers((state) =>
    state.servers.find((server) => server.id === state.activeId)?.isDefault === true
  );
  const sessions = useSessions((state) => state.sessions);
  const workingAgents = activeServerIsLocal
    ? sessions.filter((session) => !session.exited && session.working).length
    : null;
  const [open, setOpen] = useState(false);
  const [pushEnabled, setPushEnabled] = useState(readPushEnabled);
  const [pushBusy, setPushBusy] = useState(false);
  const [pushMessage, setPushMessage] = useState<string | null>(null);
  const [pairTicket, setPairTicket] = useState('');
  const [pairBusy, setPairBusy] = useState(false);
  const [pairMessage, setPairMessage] = useState<string | null>(null);
  const [updateInfo, setUpdateInfo] = useState<NativeUpdateInfo | null>(null);
  const [updateProgress, setUpdateProgress] = useState<NativeUpdateProgress | null>(null);
  const [updateBusy, setUpdateBusy] = useState(false);
  const [updateMessage, setUpdateMessage] = useState<string | null>(null);
  const [recapSettings, setRecapSettings] = useState<RecapSettings>({ provider: 'off' });
  const [recapBusy, setRecapBusy] = useState(false);
  const [recapAvailable, setRecapAvailable] = useState(true);
  const [recapMessage, setRecapMessage] = useState<string | null>(null);
  const [aiSettings, setAISettings] = useState<AISettings>({ provider: 'codex' });
  const [aiBusy, setAIBusy] = useState(false);
  const [aiAvailable, setAIAvailable] = useState(true);
  const [aiMessage, setAIMessage] = useState<string | null>(null);
  const [sessionDefaults, setSessionDefaults] = useState<NewSessionDefaults>(readNewSessionDefaults);
  const wrapRef = useRef<HTMLDivElement | null>(null);
  const recapGeneration = useRef(0);
  const aiGeneration = useRef(0);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: PointerEvent): void => {
      if (!wrapRef.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('pointerdown', onDown);
    return () => document.removeEventListener('pointerdown', onDown);
  }, [open]);

  useEffect(() => {
    if (!isTauri()) return;
    const nextRecapGeneration = recapGeneration.current + 1;
    const nextAIGeneration = aiGeneration.current + 1;
    recapGeneration.current = nextRecapGeneration;
    aiGeneration.current = nextAIGeneration;
    const controller = new AbortController();
    setRecapSettings({ provider: 'off' });
    setAISettings({ provider: 'codex' });
    setRecapBusy(false);
    setAIBusy(false);
    setRecapAvailable(true);
    setAIAvailable(true);
    setRecapMessage(null);
    setAIMessage(null);
    void fetchRecapSettings(controller.signal)
      .then((settings) => { if (recapGeneration.current === nextRecapGeneration) setRecapSettings(settings); })
      .catch(() => {
        if (!controller.signal.aborted && recapGeneration.current === nextRecapGeneration) {
          setRecapAvailable(false);
          setRecapMessage('Daily recaps require a current Sessions runtime.');
        }
      });
    void fetchAISettings(controller.signal)
      .then((settings) => { if (aiGeneration.current === nextAIGeneration) setAISettings(settings); })
      .catch(() => {
        if (!controller.signal.aborted && aiGeneration.current === nextAIGeneration) {
          setAIAvailable(false);
          setAIMessage('AI search requires a current Sessions runtime.');
        }
      });
    return () => controller.abort();
  }, [activeServerId]);

  const cycleSize = (): void => {
    const next = nextSize(textSize);
    onTextSizeChange(next);
  };

  const saveSessionDefaults = (patch: Partial<NewSessionDefaults>): void => {
    setSessionDefaults((current) => {
      const next = normalizeNewSessionDefaults({ ...current, ...patch });
      writeNewSessionDefaults(next);
      return next;
    });
  };

  const saveRecapSettings = async (next: RecapSettings): Promise<void> => {
    if (clientOnly || recapBusy || !recapAvailable) return;
    const previous = recapSettings;
    const generation = recapGeneration.current + 1;
    recapGeneration.current = generation;
    setRecapBusy(true);
    setRecapMessage(null);
    setRecapSettings(next);
    try {
      const saved = await updateRecapSettings(next);
      if (recapGeneration.current !== generation) return;
      setRecapSettings(saved);
      setRecapMessage(saved.provider === 'off' ? 'Daily model calls are off' : `${saved.provider === 'codex' ? 'Codex' : 'Claude'} will write recaps only when requested`);
    } catch (error) {
      if (recapGeneration.current === generation) {
        setRecapSettings(previous);
        setRecapMessage(error instanceof Error ? error.message : 'Could not save recap settings');
      }
    } finally {
      if (recapGeneration.current === generation) setRecapBusy(false);
    }
  };

  const saveAISettings = async (next: AISettings): Promise<void> => {
    if (clientOnly || aiBusy || !aiAvailable) return;
    const previous = aiSettings;
    const generation = aiGeneration.current + 1;
    aiGeneration.current = generation;
    setAIBusy(true);
    setAIMessage(null);
    setAISettings(next);
    try {
      const saved = await updateAISettings(next);
      if (aiGeneration.current !== generation) return;
      setAISettings(saved);
      setAIMessage(`${saved.provider === 'codex' ? 'Codex' : 'Claude'} will plan explicitly requested AI searches`);
    } catch (error) {
      if (aiGeneration.current === generation) {
        setAISettings(previous);
        setAIMessage(error instanceof Error ? error.message : 'Could not save the smart-feature provider');
      }
    } finally {
      if (aiGeneration.current === generation) setAIBusy(false);
    }
  };

  const enablePush = async (): Promise<void> => {
    if (!window.isSecureContext) {
      setPushMessage('Requires HTTPS (enable tailscale serve)');
      return;
    }
    if (
      typeof Notification === 'undefined' ||
      !('serviceWorker' in navigator) ||
      !('PushManager' in window)
    ) {
      setPushMessage('Push notifications are not supported in this browser');
      return;
    }
    if (Notification.permission === 'denied') {
      setPushMessage('Notifications are blocked in browser settings');
      return;
    }

    const permission = Notification.permission === 'granted'
      ? 'granted'
      : await Notification.requestPermission();
    if (permission !== 'granted') {
      setPushMessage('Notification permission was not granted');
      return;
    }

    const publicKey = await getPushVapidPublicKey();
    const registration = await getPushRegistration();
    const existing = await registration.pushManager.getSubscription();
    const subscription = existing ?? await registration.pushManager.subscribe({
      userVisibleOnly: true,
      applicationServerKey: urlBase64ToUint8Array(publicKey)
    });
    await subscribePush(subscription);
    writePushEnabled(true);
    setPushEnabled(true);
    setPushMessage('Notifications enabled');
  };

  const disablePush = async (): Promise<void> => {
    let endpoint: string | null = null;
    if ('serviceWorker' in navigator) {
      const registration = await navigator.serviceWorker.getRegistration(new URL('.', document.baseURI).href);
      const subscription = await registration?.pushManager.getSubscription();
      if (subscription) {
        endpoint = subscription.endpoint;
        try { await subscription.unsubscribe(); } catch { /* best effort */ }
      }
    }
    let cleanupError: string | null = null;
    if (endpoint) {
      try {
        await unsubscribePush(endpoint);
      } catch (err) {
        cleanupError = (err as Error).message;
      }
    }
    writePushEnabled(false);
    setPushEnabled(false);
    setPushMessage(cleanupError ? `Disabled locally; daemon cleanup failed: ${cleanupError}` : 'Notifications disabled');
  };

  const togglePush = async (): Promise<void> => {
    if (pushBusy) return;
    setPushBusy(true);
    setPushMessage(null);
    try {
      if (pushEnabled) await disablePush();
      else await enablePush();
    } catch (err) {
      setPushMessage((err as Error).message);
    } finally {
      setPushBusy(false);
    }
  };

  const claimPairTicket = async (): Promise<void> => {
    if (pairBusy || !pairTicket.trim()) return;
    setPairBusy(true);
    setPairMessage(null);
    try {
      if (isTauri()) {
        const { claim, server } = await claimNativeMachinePairing(pairTicket);
        setPairTicket('');
        setPairMessage(`Paired with ${server.name} as ${claim.name}`);
        return;
      }
      const claimed = await claimCurrentOriginPairing(pairTicket);
      setPairTicket('');
      setPairMessage(`Paired as ${claimed.name}`);
    } catch (error) {
      setPairMessage(error instanceof Error ? error.message : 'Pairing failed. Run `sessions pair` again.');
    } finally {
      setPairBusy(false);
    }
  };

  const checkForUpdate = async (): Promise<void> => {
    if (updateBusy) return;
    setUpdateBusy(true);
    setUpdateMessage(null);
    setUpdateProgress(null);
    try {
      const available = await checkForNativeUpdate();
      setUpdateInfo(available);
      setUpdateMessage(available ? null : 'Sessions is up to date');
    } catch (error) {
      setUpdateInfo(null);
      setUpdateMessage(error instanceof Error ? error.message : 'Could not check for updates');
    } finally {
      setUpdateBusy(false);
    }
  };

  const installUpdate = async (): Promise<void> => {
    if (!updateInfo || updateBusy) return;
    setUpdateBusy(true);
    setUpdateMessage('Downloading update…');
    try {
      await installNativeUpdate((progress) => {
        setUpdateProgress(progress);
        if (progress.totalBytes) {
          const percent = Math.min(100, Math.round((progress.downloadedBytes / progress.totalBytes) * 100));
          setUpdateMessage(`Downloading update… ${percent}%`);
        }
      });
    } catch (error) {
      setUpdateMessage(error instanceof Error ? error.message : 'Could not install update');
      setUpdateBusy(false);
    }
  };

  return (
    <div className="settings-menu" ref={wrapRef}>
      <button
        type="button"
        className="settings-menu-trigger"
        onClick={() => setOpen((v) => !v)}
        aria-haspopup="menu"
        aria-expanded={open}
        title="Settings"
      >
        ⚙
        {updateInfo ? <span className="settings-update-dot" aria-label={`Sessions ${updateInfo.version} available`} /> : null}
      </button>
      {open ? (
        <div className="settings-menu-popover" role="menu">
          {onNewSession ? (
            <button
              type="button"
              className="settings-menu-row settings-menu-clickable"
              onClick={() => { setOpen(false); onNewSession(); }}
            >
              <span className="settings-menu-icon">+</span>
              <span className="settings-menu-label">New session</span>
            </button>
          ) : null}
          <button
            type="button"
            className="settings-menu-row settings-menu-clickable"
            onClick={cycleSize}
            title="Cycle interface size: Compact → Comfortable → Large"
          >
            <span className="settings-menu-icon">Aa</span>
            <span className="settings-menu-label">View size</span>
            <span className="settings-menu-value">{sizeLabel(textSize)}</span>
          </button>
          <div className="settings-menu-divider" />
          <div className="settings-menu-section" aria-label="Default new-session options">
            <div className="settings-menu-section-title">Default new-session options</div>
            <label className="settings-menu-field">
              <span>Tool</span>
              <select
                className="settings-menu-input"
                value={sessionDefaults.tool}
                onChange={(e) => {
                  if (isNewSessionTool(e.currentTarget.value)) {
                    saveSessionDefaults({ tool: e.currentTarget.value });
                  }
                }}
              >
                {NEW_SESSION_TOOL_OPTIONS.map((option) => (
                  <option key={option.id} value={option.id}>{option.label}</option>
                ))}
              </select>
            </label>
            <label className="settings-menu-row settings-menu-toggle settings-menu-default-toggle">
              <input
                type="checkbox"
                checked={sessionDefaults.skipPerms}
                onChange={(e) => saveSessionDefaults({ skipPerms: e.currentTarget.checked })}
              />
              <span className="settings-menu-label">Full access by default</span>
              <span className="settings-menu-value">{sessionDefaults.skipPerms ? 'On' : 'Ask first'}</span>
            </label>
            <label className="settings-menu-field">
              <span>Default cwd</span>
              <input
                className="settings-menu-input settings-menu-path-input"
                type="text"
                value={sessionDefaults.cwd}
                onChange={(e) => saveSessionDefaults({ cwd: e.currentTarget.value })}
                placeholder="Server default"
                spellCheck={false}
                autoCorrect="off"
                autoCapitalize="off"
              />
            </label>
            <div className="settings-menu-field-row">
              <label className="settings-menu-field">
                <span>Cols</span>
                <input
                  className="settings-menu-input"
                  type="number"
                  min={NEW_SESSION_DIMENSIONS.minCols}
                  max={NEW_SESSION_DIMENSIONS.maxCols}
                  value={sessionDefaults.cols}
                  onChange={(e) => saveSessionDefaults({ cols: e.currentTarget.valueAsNumber })}
                />
              </label>
              <label className="settings-menu-field">
                <span>Rows</span>
                <input
                  className="settings-menu-input"
                  type="number"
                  min={NEW_SESSION_DIMENSIONS.minRows}
                  max={NEW_SESSION_DIMENSIONS.maxRows}
                  value={sessionDefaults.rows}
                  onChange={(e) => saveSessionDefaults({ rows: e.currentTarget.valueAsNumber })}
                />
              </label>
            </div>
            <div className="settings-menu-field settings-menu-default-tags">
              <span>Default tags</span>
              <TagEditor
                value={sessionDefaults.tags}
                onChange={(tags) => saveSessionDefaults({ tags })}
              />
              <span className="settings-menu-field-hint">Inherited by future sessions and always editable before start.</span>
            </div>
          </div>
          <label className="settings-menu-row settings-menu-toggle">
            <input
              type="checkbox"
              checked={pushEnabled}
              onChange={() => void togglePush()}
              disabled={pushBusy}
            />
            <span className="settings-menu-label">Notify when a session finishes</span>
            <span className="settings-menu-value">{pushBusy ? '...' : pushEnabled ? 'On' : 'Off'}</span>
          </label>
          {pushMessage ? (
            <div className="settings-menu-status">{pushMessage}</div>
          ) : null}
          {isTauri() ? (
            <>
              <div className="settings-menu-divider" />
              <div className="settings-menu-section" aria-label="Smart features">
                <div className="settings-menu-section-title">Smart features</div>
                {clientOnly ? <div className="settings-menu-status">Chosen on {hostName}</div> : null}
                <label className="settings-menu-field">
                  <span>Provider</span>
                  <select
                    className="settings-menu-input"
                    value={aiSettings.provider}
                    disabled={clientOnly || aiBusy || !aiAvailable}
                    onChange={(event) => void saveAISettings({ provider: event.currentTarget.value as AIProvider })}
                  >
                    <option value="codex">Codex · recommended</option>
                    <option value="claude">Claude</option>
                  </select>
                </label>
                <span className="settings-menu-field-hint">Used only when you explicitly submit an AI action. Search sends the natural-language query—not transcripts—then searches the local index. Your CLI chooses its default model.</span>
                {aiMessage ? <div className="settings-menu-status">{aiMessage}</div> : null}
              </div>
              <div className="settings-menu-divider" />
              <div className="settings-menu-section" aria-label="Daily recap">
                <div className="settings-menu-section-title">Daily recap</div>
                {clientOnly ? <div className="settings-menu-status">Chosen on {hostName}</div> : null}
                <label className="settings-menu-field">
                  <span>Writer</span>
                  <select
                    className="settings-menu-input"
                    value={recapSettings.provider}
                    disabled={clientOnly || recapBusy || !recapAvailable}
                    onChange={(event) => void saveRecapSettings({ ...recapSettings, provider: event.currentTarget.value as RecapProvider })}
                  >
                    <option value="off">Off · no model calls</option>
                    <option value="codex">Codex · recommended</option>
                    <option value="claude">Claude</option>
                  </select>
                </label>
                <span className="settings-menu-field-hint">One manually requested call, capped at 32 KiB and lowest reasoning effort. Your CLI chooses its default model; full transcripts are never sent.</span>
                {recapMessage ? <div className="settings-menu-status">{recapMessage}</div> : null}
              </div>
            </>
          ) : null}
          {isTauri() ? (
            <>
              <div className="settings-menu-divider" />
              <div className="settings-menu-section" aria-label="App updates">
                <div className="settings-menu-section-title">Sessions updates</div>
                {updateInfo ? (
                  <button
                    type="button"
                    className="settings-menu-row settings-menu-clickable"
                    disabled={updateBusy}
                    onClick={() => void installUpdate()}
                  >
                    <span className="settings-menu-icon">↥</span>
                    <span className="settings-menu-label">Install Sessions {updateInfo.version}</span>
                    <span className="settings-menu-value">
                      {workingAgents === null ? 'Agents keep running' : workingAgents > 0 ? `${workingAgents} continue` : 'No agents running'}
                    </span>
                  </button>
                ) : (
                  <button
                    type="button"
                    className="settings-menu-row settings-menu-clickable"
                    disabled={updateBusy}
                    onClick={() => void checkForUpdate()}
                  >
                    <span className="settings-menu-icon">↻</span>
                    <span className="settings-menu-label">Check for updates</span>
                    <span className="settings-menu-value">{updateBusy ? 'Checking…' : ''}</span>
                  </button>
                )}
                {updateMessage ? (
                  <div className="settings-menu-status" role="status">{updateMessage}</div>
                ) : null}
                {updateInfo ? (
                  <div className="settings-menu-status">
                    {workingAgents === null
                      ? 'Safe to install: this updates the client. Agents and open sessions on every host keep running.'
                      : workingAgents > 0
                      ? 'Safe to install: working agents keep their current runner. New sessions use the updated runner after relaunch.'
                      : 'No agents are working. The app can update now without losing open sessions.'}
                  </div>
                ) : null}
                {updateBusy && updateProgress?.totalBytes ? (
                  <progress
                    className="settings-menu-progress"
                    value={updateProgress.downloadedBytes}
                    max={updateProgress.totalBytes}
                    aria-label="Update download progress"
                  />
                ) : null}
                {updateInfo?.notes ? (
                  <div className="settings-menu-update-notes">{updateInfo.notes}</div>
                ) : null}
                {updateInfo ? (
                  <div className="settings-menu-update-safe">Sessions keep running while the app restarts.</div>
                ) : null}
              </div>
            </>
          ) : null}
          {onOpenConnections ? (
            <>
              <div className="settings-menu-divider" />
              <button
                type="button"
                className="settings-menu-row settings-menu-clickable"
                onClick={() => { setOpen(false); onOpenConnections(); }}
              >
                <span className="settings-menu-icon">⌁</span>
                <span className="settings-menu-label">Connections, Tailscale, and port</span>
                <span className="settings-menu-value">Open</span>
              </button>
            </>
          ) : null}
          {/* Server selector — "this machine" + IP picker. Tucked into
              Settings because the user doesn't need to see the host:port
              in the chrome all the time; it only matters when switching
              between machines. */}
          <div className="settings-menu-divider" />
          <div className="settings-menu-section" aria-label="Pair">
            <div className="settings-menu-section-title">{isTauri() ? 'LAN pairing fallback' : 'Pair this browser'}</div>
            <div className="settings-menu-status">
              {isTauri()
                ? 'Normally use Connections → Find Sessions Macs. Paste a one-time link here only when both devices share a LAN without Tailscale.'
                : 'Paste the one-time ticket created by `sessions pair` on this machine.'}
            </div>
            <div className="settings-menu-field-row">
              <label className="settings-menu-field">
                <span>{isTauri() ? 'Pairing link' : 'Ticket'}</span>
                <input
                  className="settings-menu-input"
                  type="text"
                  value={pairTicket}
                  onChange={(event) => setPairTicket(event.currentTarget.value)}
                  onKeyDown={(event) => {
                    if (event.key === 'Enter') void claimPairTicket();
                  }}
                  placeholder={isTauri() ? 'http://192.168.…/#pair=…' : 'From sessions pair'}
                  autoComplete="off"
                  spellCheck={false}
                />
              </label>
              <button
                type="button"
                className="btn settings-menu-clickable"
                disabled={pairBusy || !pairTicket.trim()}
                onClick={() => void claimPairTicket()}
              >
                {pairBusy ? 'Pairing…' : isTauri() ? 'Add' : 'Pair'}
              </button>
            </div>
            {pairMessage ? (
              <div className="settings-menu-status" role="status">{pairMessage}</div>
            ) : null}
          </div>
          <div className="settings-menu-divider" />
          <div className="settings-menu-row settings-menu-server">
            <span className="settings-menu-icon">🖥</span>
            <ServerSelector />
          </div>
        </div>
      ) : null}
    </div>
  );
}
