import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { SessionTabs, type TabStatus } from './components/SessionTabs';
import { SessionView } from './components/SessionView';
import { EmptyState } from './components/EmptyState';
import { NewSessionDialog } from './components/NewSessionDialog';
import { ResumeDialog } from './components/ResumeDialog';
import { MobileNav } from './components/MobileNav';
import { ConnectionStatus } from './components/ConnectionStatus';
import { GridView } from './components/GridView';
import { FleetView } from './components/FleetView';
import { UsageDashboard } from './components/UsageDashboard';
import { DailyView } from './components/DailyView';
import { SearchView } from './components/SearchView';
import { ProductSidebar, type ProductView, type ThemeMode } from './components/ProductSidebar';
import { SessionNavigator } from './components/SessionNavigator';
import { HomeView } from './components/HomeView';
import { SettingsView } from './components/SettingsView';
import { CommandPalette } from './components/CommandPalette';
import { SessionsWorkspaceSkeleton } from './components/LoadingShell';
import { OnboardingDialog } from './components/OnboardingDialog';
import { useSessions } from './store/sessions';
import { useServers, configureNativeClientOnly, configureNativeLocalPort, getActiveServer, isLocalServer, serverDisplayName } from './lib/servers';
import { SettingsMenu } from './components/SettingsMenu';
import { TailnetAccessInbox } from './components/TailnetAccessInbox';
import { MachineRecoveryNotice } from './components/MachineRecoveryNotice';
import { useIsMobile } from './hooks/useMediaQuery';
import { ParserIcon } from './components/ParserIcon';
import { ConnectScreen } from './components/ConnectScreen';
import { formatServerEndpoint } from './lib/serverEndpoint';
import { readTabOrder, writeTabOrder, applyOrder, moveBefore } from './lib/tabOrder';
import { useTabLabel } from './lib/tabLabels';
import { getNativeConnectionSettings, getNativeRuntimeStatus, isTauri, notify, recoverNativeRuntime, syncTrayServers } from './lib/tauriBridge';
import { readTextSize, writeTextSize, type TextSize } from './lib/textSize';
import { preloadUsage } from './lib/usageCache';
import { preloadDaily } from './lib/dailyCache';
import { providerConversationId } from './lib/sessionStatus';
import { effectiveParentId } from './lib/workingSet';
import { preferNextSessionView } from './lib/sessionViewPreference';
import { handleExternalLinkClick } from './lib/externalLinks';
import {
  adoptConversation,
  fetchServerMachineIdentity,
  fetchServerHealth,
  fetchOnboardingState,
  forkConversation,
  repairAdoption,
  updateOnboardingPreference,
  type OnboardingState,
  type ServerHealth
} from './api/sessionsd';
import type { SessionInfo, SessionTool } from './types';

const TOOL_ICONS: Record<SessionTool, string> = {
  'claude-code': '🟠',
  'codex': '🟢',
  'terminal': '⬛'
};

// Status of the currently-attached session, lifted out of SessionView so
// the tab strip and mobile nav can reflect it. Only the *active* session
// has live data here — inactive tabs stay 'idle' until we add background
// polling (deferred from Phase 4).
export interface ActiveStatus {
  isWorking: boolean;
  parserIcon: string;
  parserName: string;
  terminalStatus: string; // 'open' | 'connecting' | 'reconnecting' | 'error' | 'closed'
}

const INITIAL_STATUS: ActiveStatus = {
  isWorking: false,
  parserIcon: '⬛',
  parserName: 'Terminal',
  terminalStatus: 'connecting'
};

// Pop-out mode: a second window opened by Tauri (or window.open in the
// browser) for a single session. URL signals it: `?session=<id>&mode=single`.
// We render a stripped shell — no tabs, no server selector, no mobile nav,
// no grid toggle — and let SessionView fill the whole window.
function readSingleModeParams(): { sessionId: string } | null {
  if (typeof window === 'undefined') return null;
  const params = new URLSearchParams(window.location.search);
  if (params.get('mode') !== 'single') return null;
  const sessionId = params.get('session');
  return sessionId ? { sessionId } : null;
}

// Layout mode: tabs (default), fleet (all configured machines), or grid
// (active-machine monitor tiles).
// Persisted per-window in localStorage so each window remembers its
// last choice. Grid is best when N ≥ 2 and the window is wide.
type LayoutMode = 'home' | 'tabs' | 'today' | 'fleet' | 'search' | 'usage' | 'settings' | 'feedback' | 'connections' | 'grid';
// Shared empty list for "the loaded sessions belong to a different machine".
// A `[]` literal in that position is a new array identity on every render,
// which made every memo, callback, and effect derived from the session list
// re-run continuously while the scope was mismatched.
const NO_SESSIONS: SessionInfo[] = [];

const LAYOUT_KEY = 'sessions:layout-mode';
const OPEN_TABS_KEY = 'sessions:open-tabs:v1';
const THEME_KEY = 'sessions:theme:v1';
function readStoredLayout(): LayoutMode {
  try {
    const v = window.localStorage.getItem(LAYOUT_KEY);
    if (v === 'home' || v === 'tabs' || v === 'today' || v === 'fleet' || v === 'search' || v === 'usage' || v === 'settings' || v === 'feedback' || v === 'connections' || v === 'grid') return v;
  } catch { /* ignore */ }
  return 'tabs';
}

function readOpenTabs(): string[] {
  try {
    const parsed = JSON.parse(window.localStorage.getItem(OPEN_TABS_KEY) ?? '[]');
    return Array.isArray(parsed) ? parsed.filter((value): value is string => typeof value === 'string').slice(0, 12) : [];
  } catch { return []; }
}

function readTheme(): ThemeMode {
  try {
    const saved = window.localStorage.getItem(THEME_KEY);
    if (saved === 'dark' || saved === 'light') return saved;
  } catch { /* ignore */ }
  return window.matchMedia?.('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
}

function isMessageObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}

export function App(): JSX.Element {
  const [nativeHydrated, setNativeHydrated] = useState(!isTauri());
  const [nativeClientOnly, setNativeClientOnly] = useState(false);
  const activeServerId = useServers((state) => state.activeId);
  const servers = useServers((state) => state.servers);
  const pairingError = useServers((state) => state.pairingError);
  const credentialError = useServers((state) => state.credentialError);
  const updateServer = useServers((state) => state.updateServer);
  // Credentials are part of the endpoint's identity. Saving or replacing a
  // token changes nothing else here, so without it a machine that answered 401
  // would stay unnamed forever — identity is never re-fetched once it becomes
  // reachable. Presence alone is not enough: swapping a rejected token for a
  // working one has to re-probe too. This value is only ever an effect
  // dependency; it is never rendered, logged, or sent anywhere.
  const identityRefreshKey = servers.map((server) => [
    server.id,
    server.scheme ?? 'http',
    server.host,
    server.port,
    server.token ?? '',
    server.machineId ?? '',
    server.systemName ?? ''
  ].join('|')).join('\n');
  useEffect(() => {
    if (!isTauri()) return;
    let active = true;
    void getNativeConnectionSettings()
      .then((settings) => {
        if (!active || !settings) return;
        const clientOnly = settings.runtime.state === 'client-only';
        setNativeClientOnly(clientOnly);
        if (clientOnly) configureNativeClientOnly();
        else configureNativeLocalPort(settings.port);
      })
      .catch(() => { /* runtime status screen will surface an actionable error */ })
      .finally(() => { if (active) setNativeHydrated(true); });
    return () => { active = false; };
  }, []);
  useEffect(() => {
    void syncTrayServers(servers);
  }, [servers]);
  useEffect(() => {
    if (!nativeHydrated) return;
    const controllers = servers.map(() => new AbortController());
    servers.forEach((server, index) => {
      void fetchServerMachineIdentity(server, controllers[index].signal)
        .then(async (identity) => {
          const current = useServers.getState().servers.find((candidate) => candidate.id === server.id);
          if (!current) return;
          // A paired endpoint changing stable identity is not a rename. Keep
          // the approved record unchanged until the user pairs the new host.
          if (current.machineId && current.machineId !== identity.machineId) return;
          if (current.machineId === identity.machineId && current.systemName === identity.name) return;
          await updateServer(current.id, {
            machineId: current.machineId || identity.machineId,
            systemName: identity.name,
            // Keep legacy consumers current while systemName/customName
            // remain the explicit source of truth for new UI.
            name: current.customName || identity.name
          });
        })
        .catch(() => { /* older/offline hosts keep their last known label */ });
    });
    return () => controllers.forEach((controller) => controller.abort());
  // `servers` is intentionally not a dependency: the store hands back a new
  // array on every write, so depending on it would abort and re-issue an
  // identity probe to every configured machine on unrelated state changes.
  // identityRefreshKey is the value-equal summary of exactly the fields these
  // probes read, and it is kept in sync with them at its declaration above.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [identityRefreshKey, nativeHydrated, updateServer]);
  if (!nativeHydrated) return <div className="native-hydration">Connecting to the Sessions runtime…</div>;
  return activeServerId && !pairingError && !credentialError
    ? <ConnectedApp nativeClientOnly={nativeClientOnly} />
    : <ConnectScreen clientOnly={nativeClientOnly} />;
}

function ConnectedApp({ nativeClientOnly = false }: { nativeClientOnly?: boolean }): JSX.Element {
  const activeServerId = useServers((s) => s.activeId);
  const servers = useServers((s) => s.servers);
  const selectedServer = servers.find((server) => server.id === activeServerId) ?? null;
  const sessionsServerId = useSessions((s) => s.serverId);
  const serverScopeMatches = Boolean(activeServerId) && sessionsServerId === activeServerId;
  const storedSessions = useSessions((s) => s.sessions);
  const storedActiveId = useSessions((s) => s.activeId);
  const rawSessions = serverScopeMatches ? storedSessions : NO_SESSIONS;
  const activeId = serverScopeMatches ? storedActiveId : null;
  const setActive = useSessions((s) => s.setActive);
  const setServerScope = useSessions((s) => s.setServerScope);
  const refresh = useSessions((s) => s.refresh);
  const kill = useSessions((s) => s.kill);
  const updateDisplayParent = useSessions((s) => s.updateDisplayParent);
  const updateSetAside = useSessions((s) => s.updateSetAside);
  // Track whether the session list has ever successfully loaded. While
  // hydrated is false, any error means we can't reach the daemon.
  const storedSessionsError = useSessions((s) => s.error);
  const storedSessionsHydrated = useSessions((s) => s.hydrated);
  const sessionsError = serverScopeMatches ? storedSessionsError : null;
  const sessionsHydrated = serverScopeMatches && storedSessionsHydrated;
  const localRuntimeSelected = Boolean(
    selectedServer?.isDefault && isLocalServer(selectedServer) && !nativeClientOnly
  );
  const [nativeRuntimeError, setNativeRuntimeError] = useState<string | null>(null);
  const [nativeRuntimeReconnecting, setNativeRuntimeReconnecting] = useState(false);
  const [serverHealth, setServerHealth] = useState<ServerHealth | null>(null);
  const [knownSessionCount, setKnownSessionCount] = useState(rawSessions.length);
  const [manualRecoveryBusy, setManualRecoveryBusy] = useState(false);
  useEffect(() => {
    if (!isTauri() || !localRuntimeSelected || !sessionsError) {
      setNativeRuntimeError(null);
      setNativeRuntimeReconnecting(false);
      return;
    }
    void getNativeRuntimeStatus()
      .then((status) => {
        setNativeRuntimeError(status?.state === 'error' ? status.detail : null);
        setNativeRuntimeReconnecting(status?.state === 'starting');
      })
      .catch(() => {
        setNativeRuntimeError(null);
        setNativeRuntimeReconnecting(false);
      });
  }, [localRuntimeSelected, sessionsError]);
  useEffect(() => {
    setServerHealth(null);
    setKnownSessionCount(rawSessions.length);
  // Capture only the selected machine's first cached frame. Later refreshes
  // can increase this count but never borrow another machine's total.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeServerId]);
  useEffect(() => {
    // Health polling is a recovery diagnostic, not another permanent network
    // surface. The normal session-list request is enough while connected.
    if (!selectedServer || !sessionsError) {
      setServerHealth(null);
      return;
    }
    let disposed = false;
    let controller: AbortController | null = null;
    const probe = (): void => {
      controller?.abort();
      controller = new AbortController();
      void fetchServerHealth(selectedServer, controller.signal)
        .then((health) => { if (!disposed) setServerHealth(health); })
        .catch(() => { if (!disposed) setServerHealth(null); });
    };
    probe();
    const interval = window.setInterval(probe, 3_000);
    return () => {
      disposed = true;
      controller?.abort();
      window.clearInterval(interval);
    };
  }, [activeServerId, selectedServer, sessionsError]);
  useEffect(() => {
    setKnownSessionCount((current) => Math.max(current, rawSessions.length, serverHealth?.sessionsLoaded ?? 0));
  }, [rawSessions.length, serverHealth?.sessionsLoaded]);

  // User-defined tab order. Persisted in localStorage so the order
  // survives reloads. Server's session list comes back in creation
  // order; we apply the user's preferences on top before passing
  // to SessionTabs / GridView.
  const [tabOrder, setTabOrder] = useState<string[]>(readTabOrder);
  const sessions = useMemo(() => applyOrder(rawSessions, tabOrder), [rawSessions, tabOrder]);
  const reorderTab = useCallback((fromId: string, toId: string): void => {
    setTabOrder((prev) => {
      const next = moveBefore(prev, rawSessions.map((s) => s.id), fromId, toId);
      writeTabOrder(next);
      return next;
    });
  }, [rawSessions]);

  const single = useMemo(() => readSingleModeParams(), []);
  // Dialog state holds null (closed), 'new' (fresh-session mode), or
  // 'resume' (the dedicated ended-conversation picker).
  const [dialogOpen, setDialogOpen] = useState<
    null | 'new' | 'resume' | { delegateFrom: string } | {
      resumeProviderId: string;
      sourceSessionId?: string;
      historyId?: string;
      destinationProvider?: 'claude' | 'codex';
      runtimeMode?: 'rich' | 'terminal';
    }
  >(null);
  const [activeStatus, setActiveStatus] = useState<ActiveStatus>(INITIAL_STATUS);
  const [openTabIds, setOpenTabIds] = useState<string[]>(readOpenTabs);
  const [theme, setTheme] = useState<ThemeMode>(readTheme);
  const [mobileSessionDetail, setMobileSessionDetail] = useState(false);
  const [commandPaletteOpen, setCommandPaletteOpen] = useState(false);
  const tokenRequiredServerId = useServers((s) => s.tokenRequiredServerId);
  const [onboarding, setOnboarding] = useState<OnboardingState | null>(null);
  const [onboardingBusy, setOnboardingBusy] = useState(false);
  const [onboardingError, setOnboardingError] = useState<string | null>(null);
  const onboardingPending = onboarding?.supported !== false && onboarding?.complete === false;

  const writeOpenTabs = useCallback((ids: string[]): void => {
    try { window.localStorage.setItem(OPEN_TABS_KEY, JSON.stringify(ids)); } catch { /* non-fatal */ }
  }, []);
  const openSession = useCallback((id: string): void => {
    if (rawSessions.find((session) => session.id === id)?.setAsideAt != null) {
      void updateSetAside(id, false).catch(() => {
        // Opening remains useful even if an older daemon cannot persist the
        // working-set change. The next refresh will keep the row discoverable.
      });
    }
    setOpenTabIds((current) => {
      const next = current.includes(id) ? current : [...current, id].slice(-12);
      writeOpenTabs(next);
      return next;
    });
    setActive(id);
    setLayoutMode('tabs');
    setMobileSessionDetail(true);
  // setLayoutMode is a stable React setter declared below; callbacks run only
  // after the component has completed initialization.
  }, [rawSessions, setActive, updateSetAside, writeOpenTabs]);
  const resumeSession = useCallback((
    session: SessionInfo,
    destinationProvider?: 'claude' | 'codex',
    runtimeMode?: 'rich' | 'terminal'
  ): void => {
    const providerId = providerConversationId(session);
    setDialogOpen({
      // A Sessions history id is a valid recovery target even when an older
      // runner did not persist the provider UUID. Keep the exact row the user
      // clicked instead of falling back to an unrelated generic picker.
      resumeProviderId: providerId ?? session.id,
      sourceSessionId: session.id,
      historyId: providerId ? undefined : session.id,
      destinationProvider,
      runtimeMode: providerId ? runtimeMode : 'rich'
    });
  }, []);
  const forkSession = useCallback(async (
    session: SessionInfo,
    destinationProvider: 'claude' | 'codex',
    point?: { index: number; messageId: string }
  ): Promise<void> => {
    const result = await forkConversation(session.id, destinationProvider, point);
    await refresh();
    openSession(result.laneId);
  }, [openSession, refresh]);

  // Bound how many sessions are kept LIVE (mounted SessionView → xterm
  // buffer + claudeEvents history + WS attach). Without this, every session
  // the user ever viewed kept its full terminal scrollback and message
  // history resident forever (the view was sticky-mounted and never
  // discarded) — so a long-open tab with dozens of windows accumulated
  // hundreds of MB and degraded into multi-second freezes. We keep only the
  // active session plus the few most-recently-viewed live; the rest are
  // discarded (their SessionView unmounts, freeing xterm + events) but stay
  // in the tab strip (SessionTabs renders ALL sessions, driven by the HTTP
  // poll) and re-hydrate instantly from the server snapshot + event replay
  // when clicked. Nothing is hidden — only un-viewed history is dropped.
  const LIVE_SESSION_CAP = 3;
  const [liveIds, setLiveIds] = useState<string[]>(() => (activeId ? [activeId] : []));
  useEffect(() => {
    if (!activeId || rawSessions.find((session) => session.id === activeId)?.exited) return;
    setLiveIds((prev) => (prev[0] === activeId
      ? prev
      : [activeId, ...prev.filter((id) => id !== activeId)].slice(0, LIVE_SESSION_CAP)));
  }, [activeId, rawSessions]);
  useEffect(() => {
    // Historical rows use a read-only view and never consume a live transport
    // slot. Drop ids for sessions that no longer exist or have ended.
    setLiveIds((prev) => {
      const alive = prev.filter((id) => rawSessions.some((s) => s.id === id && !s.exited));
      return alive.length === prev.length ? prev : alive;
    });
  }, [rawSessions]);

  const isMobile = useIsMobile();
  const [textSize, setTextSize] = useState(readTextSize());
  const changeTextSize = useCallback((size: TextSize): void => {
    writeTextSize(size);
    setTextSize(size);
  }, []);
  // Grid is too cramped for a compact viewport. Fleet, search, and usage are
  // useful on phones and narrow Mac windows, so the mobile nav keeps them.
  const [layoutMode, setLayoutMode] = useState<LayoutMode>(readStoredLayout);
  const effectiveLayout: LayoutMode = isMobile && layoutMode === 'grid' ? 'tabs' : layoutMode;
  const openNewSession = useCallback((): void => {
    setLayoutMode('tabs');
    setMobileSessionDetail(true);
    setDialogOpen('new');
  }, []);
  const localServer = servers.find((server) => server.isDefault && isLocalServer(server)) ?? null;
  const selectedMachineName = selectedServer ? serverDisplayName(selectedServer, true) : 'this machine';
  const recoveryVisible = Boolean(sessionsError || serverHealth?.discovering);
  const recoverSelectedMachine = useCallback(async (): Promise<void> => {
    if (!activeServerId || manualRecoveryBusy) return;
    setManualRecoveryBusy(true);
    try {
      if (localRuntimeSelected && isTauri()) {
        const status = await recoverNativeRuntime();
        setNativeRuntimeError(status.state === 'error' ? status.detail : null);
        setNativeRuntimeReconnecting(status.state === 'starting');
      }
      await refresh(activeServerId);
    } catch (reason) {
      setNativeRuntimeError(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setManualRecoveryBusy(false);
    }
  }, [activeServerId, localRuntimeSelected, manualRecoveryBusy, refresh]);
  const startOnLocalMachine = useCallback((): void => {
    if (!localServer) return;
    useServers.getState().setActive(localServer.id);
    setServerScope(localServer.id);
    setLayoutMode('tabs');
    setMobileSessionDetail(true);
    setDialogOpen('new');
  }, [localServer, setServerScope]);
  useEffect(() => {
    if (single) return;
    const onKeyDown = (event: KeyboardEvent): void => {
      if (onboardingPending) return;
      if (!(event.metaKey || event.ctrlKey)) return;
      const inTerminal = document.activeElement instanceof Element
        && document.activeElement.closest('.terminal-host') !== null;
      // Ctrl-K/Ctrl-N are real terminal controls (readline kill-line and
      // next-history). On macOS Command remains the app shortcut; on Windows
      // and Linux Ctrl opens Sessions only outside the live terminal.
      if (event.ctrlKey && !event.metaKey && inTerminal) return;
      if (event.key.toLowerCase() === 'k') {
        event.preventDefault();
        event.stopPropagation();
        setCommandPaletteOpen((current) => !current);
      } else if (event.key.toLowerCase() === 'n') {
        event.preventDefault();
        event.stopPropagation();
        setCommandPaletteOpen(false);
        openNewSession();
      }
    };
    window.addEventListener('keydown', onKeyDown, true);
    return () => window.removeEventListener('keydown', onKeyDown, true);
  }, [onboardingPending, openNewSession, single]);
  useEffect(() => {
    try { window.localStorage.setItem(LAYOUT_KEY, layoutMode); } catch { /* ignore */ }
  }, [layoutMode]);
  useEffect(() => {
    try { window.localStorage.setItem(THEME_KEY, theme); } catch { /* ignore */ }
  }, [theme]);
  useEffect(() => {
    if (!activeId || !rawSessions.some((session) => session.id === activeId)) return;
    setOpenTabIds((current) => {
      if (current.includes(activeId)) return current;
      const next = [...current, activeId].slice(-12);
      writeOpenTabs(next);
      return next;
    });
  }, [activeId, rawSessions, writeOpenTabs]);
  useEffect(() => {
    setOpenTabIds((current) => {
      const next = current.filter((id) => rawSessions.some((session) => session.id === id));
      if (next.length === current.length) return current;
      if (!activeId && next.length > 0) setActive(next[0]);
      writeOpenTabs(next);
      return next;
    });
  }, [activeId, rawSessions, setActive, writeOpenTabs]);

  const closeTab = useCallback((id: string): void => {
    setOpenTabIds((current) => {
      const index = current.indexOf(id);
      const next = current.filter((item) => item !== id);
      writeOpenTabs(next);
      if (activeId === id) setActive(next[Math.max(0, index - 1)] ?? next[0] ?? null);
      return next;
    });
  }, [activeId, setActive, writeOpenTabs]);

  const openFleetSession = useCallback((serverId: string, sessionId: string): void => {
    useServers.getState().setActive(serverId);
    setServerScope(serverId);
    openSession(sessionId);
  }, [openSession, setServerScope]);
  const openFleetMachine = useCallback((serverId: string): void => {
    useServers.getState().setActive(serverId);
    setServerScope(serverId);
    setActive(null);
    setLayoutMode('tabs');
    setMobileSessionDetail(false);
  }, [setActive, setServerScope]);
  const continueExactConversation = useCallback(async (
    serverId: string,
    providerSessionId: string,
    sourceSessionId?: string,
    historyId?: string
  ): Promise<void> => {
    useServers.getState().setActive(serverId);
    setServerScope(serverId);
    let result = await adoptConversation(providerSessionId, sourceSessionId, historyId);
    if (result.partial && result.repair) {
      try {
        result = await repairAdoption(result.repair);
      } catch (reason) {
        // The first response already created the one live successor. Repair is
        // record-only; never turn an annotation failure into a second runtime.
        console.warn('Sessions resumed the conversation but could not finish its history annotations.', reason);
      }
    }
    await refresh(serverId);
    // Native Claude resume can immediately show a provider-only picker in its
    // terminal. Imported Codex and Sessions transcript recovery are authored
    // conversations, so open their Conversation view instead.
    preferNextSessionView(
      result.laneId,
      result.transcriptRecovery || result.destinationProvider === 'codex' || result.mode
        ? 'remote'
        : 'terminal'
    );
    openSession(result.laneId);
  }, [openSession, refresh, setServerScope]);
  useEffect(() => {
    if (!('serviceWorker' in navigator)) return;
    const onMessage = (event: MessageEvent<unknown>): void => {
      const data = event.data;
      if (!isMessageObject(data)) return;
      if (data.type !== 'push-open-session' || typeof data.sessionId !== 'string') return;
      openSession(data.sessionId);
    };
    navigator.serviceWorker.addEventListener('message', onMessage);
    return () => navigator.serviceWorker.removeEventListener('message', onMessage);
  }, [openSession]);

  // (Previously: reset activeStatus to INITIAL_STATUS on tab switch so
  // a freshly-mounting SessionView wouldn't briefly show stale values.
  // Removed — with keep-mounted SessionViews, the new active session's
  // WS is already 'open' and its onStatusChange effect fires immediately
  // with the real values. The reset was actively wrong: React runs
  // child effects before parent effects, so the parent's setActiveStatus
  // (INITIAL_STATUS, 'connecting') was overwriting the child's correct
  // push of 'open'. That's why the toolbar's connection pill was stuck
  // saying 'Connecting…' even though the WS was live.)

  // Pull session list on mount, then refresh every 3s so inactive tabs
  // get their daemon-reported `working` flag updated. The active tab
  // is overwritten below from the live parser state — the daemon
  // signal is just for sessions we aren't currently attached to. Also
  // re-runs whenever the active server changes so switching servers
  // immediately repopulates the tab strip from the new sessionsd.
  useEffect(() => {
    if (!activeServerId) return;
    setServerScope(activeServerId);
    void refresh(activeServerId);
    const id = window.setInterval(() => { void refresh(activeServerId); }, 3000);
    return () => window.clearInterval(id);
  }, [refresh, activeServerId, setServerScope]);
  useEffect(() => {
    if (!activeServerId || single) return;
    const controller = new AbortController();
    setOnboarding(null);
    setOnboardingError(null);
    void fetchOnboardingState(controller.signal)
      .then(setOnboarding)
      .catch(() => {
        if (!controller.signal.aborted) {
          // The daemon enforces the consent gate even if this read fails.
          // Keep the app usable and conservatively describe the machine as
          // local-only until the next reload or server switch.
          setOnboarding({ version: 0, complete: true, remoteControl: 'local-only', delegatedAccess: 'inherit', supported: false });
        }
      });
    return () => controller.abort();
  }, [activeServerId, single]);
  useEffect(() => {
    if (!onboardingPending) return;
    setDialogOpen(null);
    setCommandPaletteOpen(false);
  }, [onboardingPending]);
  const chooseOnboardingPreference = useCallback(async (
    remoteControl: 'enabled' | 'local-only',
    delegatedAccess: 'inherit' | 'autonomous'
  ): Promise<void> => {
    if (onboardingBusy) return;
    setOnboardingBusy(true);
    setOnboardingError(null);
    try {
      setOnboarding(await updateOnboardingPreference(remoteControl, delegatedAccess));
    } catch (reason) {
      setOnboardingError(reason instanceof Error ? reason.message : 'Could not save this choice.');
    } finally {
      setOnboardingBusy(false);
    }
  }, [onboardingBusy]);

  // Warm the local usage index as part of app startup. Usage is derived from
  // local provider files, so this stays on the selected machine and makes the
  // dashboard current before the user visits it.
  useEffect(() => {
    if (!activeServerId) return;
    const id = window.setTimeout(() => {
      void preloadUsage(activeServerId).catch(() => {
        // UsageDashboard owns the visible, actionable error state. Startup
        // warming must never block the sessions inbox.
      });
    }, 350);
    return () => window.clearTimeout(id);
  }, [activeServerId]);

  // Daily is part of startup hydration rather than a cold, on-navigation
  // request. The view adopts this cached result immediately and still renders
  // its complete skeleton if the local index has not finished yet.
  useEffect(() => {
    if (!activeServerId) return;
    const id = window.setTimeout(() => {
      void preloadDaily(activeServerId);
    }, 450);
    return () => window.clearTimeout(id);
  }, [activeServerId]);

  // Build per-session status/icon maps. Inactive tabs use the daemon's
  // activity-derived `working` flag (computed from PTY byte rate) and
  // its `tool` field (classified from the cmd at session create) for
  // the icon. Active tab is overwritten by the live parser state from
  // SessionView — that's strictly more accurate than the cmd-based
  // classification because it reads the actual buffer, but it's only
  // available for the session we're currently attached to.
  const statusBySession: Record<string, TabStatus> = {};
  const iconBySession: Record<string, string> = {};
  for (const s of sessions) {
    statusBySession[s.id] = s.working ? 'working' : 'idle';
    iconBySession[s.id] = TOOL_ICONS[s.tool];
  }
  if (activeId) {
    statusBySession[activeId] = activeStatus.isWorking ? 'working' : 'idle';
    iconBySession[activeId] = activeStatus.parserIcon;
  }

  // Working → idle desktop notifications. Track last-seen working state
  // per session id; fire whenever a session flips from true to false.
  // Compares against the daemon-reported `working` flag for *every* tab,
  // not just the active one — so a Claude turn finishing on a different
  // tab still pings. Skip the very first refresh so a cold app boot
  // doesn't fire N notifications for sessions that were idle before.
  const prevWorkingRef = useRef<Map<string, boolean> | null>(null);
  useEffect(() => {
    const prev = prevWorkingRef.current;
    const next = new Map<string, boolean>();
    for (const s of sessions) {
      // Use the daemon's `working` flag for EVERY session, including the
      // active one. It's now the honest footer-derived signal, and using
      // it uniformly avoids a phantom "done": on a tab switch, activeStatus
      // still holds the PREVIOUS session's value for one commit (the child
      // pushes the new one a render later), which used to record the new
      // tab as working=true and then fire a spurious "done" when it
      // corrected.
      const isWorking = s.working;
      next.set(s.id, isWorking);
      if (prev && prev.get(s.id) === true && isWorking === false) {
        const label = (s.cwd?.split('/').filter(Boolean).pop()) || s.cmd || s.id.slice(0, 8);
        void notify(`${label} — done`, 'Claude finished');
      }
    }
    prevWorkingRef.current = next;
  }, [sessions]);

  // Single-session pop-out window: skip every chrome element except
  // SessionView itself. The session id comes from the URL.
  if (single) {
    return (
      <SinglePopOut
        sessionId={single.sessionId}
        sessions={sessions}
        textSize={textSize}
      />
    );
  }

  const machine = serverDisplayName(getActiveServer(), true);
  const liveSessions = sessions.filter((session) => !session.exited);
  const sessionWorkspace = effectiveLayout === 'tabs' || effectiveLayout === 'grid';
  const openedSessions = sessions.filter((session) => openTabIds.includes(session.id));
  const activeSession = activeId ? sessions.find((session) => session.id === activeId) ?? null : null;
  const activeManagerId = activeSession && !effectiveParentId(activeSession) ? activeSession.id : null;
  const activeManagerTabs = activeManagerId
    ? openedSessions.filter((session) => {
      if (session.id === activeManagerId) return true;
      const seen = new Set<string>();
      let current: SessionInfo | undefined = session;
      while (current) {
        const parentId = effectiveParentId(current);
        if (!parentId || seen.has(parentId)) return false;
        if (parentId === activeManagerId) return true;
        seen.add(parentId);
        current = sessions.find((candidate) => candidate.id === parentId);
      }
      return false;
    })
    : [];
  const showManagerTabs = activeManagerTabs.length > 1;
  const productView: ProductView = effectiveLayout === 'grid'
    ? 'tabs'
    : effectiveLayout === 'connections'
      ? 'settings'
      : effectiveLayout as ProductView;
  const navigateProduct = (view: ProductView): void => {
    setLayoutMode(view);
    if (view === 'tabs') setMobileSessionDetail(false);
  };

  return (
    <div className={`app-shell operations-shell text-size-${textSize.toLowerCase()}`} data-theme={theme} onClickCapture={handleExternalLinkClick}>
      {!isMobile ? <ProductSidebar active={productView} theme={theme} onNavigate={navigateProduct} onNewSession={openNewSession} onOpenCommandPalette={() => setCommandPaletteOpen(true)} onToggleTheme={() => setTheme((current) => current === 'dark' ? 'light' : 'dark')} /> : null}
      <div className="operations-frame">
        {sessionWorkspace && !isMobile ? (
          <SessionNavigator
            sessions={sessions}
            activeId={activeId}
            machine={machine}
            onOpen={openSession}
            onOpenMachineSession={openFleetSession}
            onNew={openNewSession}
            onContinue={() => setDialogOpen('resume')}
            onResumeSession={resumeSession}
            onForkSession={forkSession}
            onStartLinked={(id) => setDialogOpen({ delegateFrom: id })}
            openSessionIds={openTabIds}
            onCloseView={closeTab}
            onReparent={updateDisplayParent}
          />
        ) : null}
        <section className="operations-content">
          <TailnetAccessInbox />
          {recoveryVisible ? (
            <MachineRecoveryNotice
              machine={selectedMachineName}
              local={localRuntimeSelected}
              discovering={Boolean(serverHealth?.discovering)}
              recovered={serverHealth?.sessionsLoaded ?? rawSessions.length}
              expected={knownSessionCount}
              busy={manualRecoveryBusy || nativeRuntimeReconnecting}
              detail={nativeRuntimeError ?? sessionsError}
              localAlternative={!localRuntimeSelected && localServer ? serverDisplayName(localServer, true) : undefined}
              onRecover={() => void recoverSelectedMachine()}
              onStartLocal={!localRuntimeSelected && localServer ? startOnLocalMachine : undefined}
            />
          ) : null}
          {sessionWorkspace && showManagerTabs && (!isMobile || mobileSessionDetail) ? (
            <header className="app-header operations-tabs-header">
              <SessionTabs
                sessions={activeManagerTabs}
                activeId={activeId}
                statusBySession={statusBySession}
                iconBySession={iconBySession}
                onSwitch={openSession}
                onAdd={openNewSession}
                onClose={closeTab}
                onReorder={reorderTab}
              />
              {!isMobile ? <div className="session-layout-switch"><button type="button" className={effectiveLayout === 'tabs' ? 'is-active' : ''} onClick={() => setLayoutMode('tabs')}>Tabs</button><button type="button" className={effectiveLayout === 'grid' ? 'is-active' : ''} onClick={() => setLayoutMode('grid')}>Grid</button></div> : null}
              <ConnectionStatus machine={machine} hydrated={sessionsHydrated} error={sessionsError} />
              <SettingsMenu textSize={textSize} onTextSizeChange={changeTextSize} onNewSession={openNewSession} onOpenConnections={() => setLayoutMode('settings')} />
            </header>
          ) : null}

      <main className="app-main operations-main">
        {tokenRequiredServerId === activeServerId ? (
          <DaemonBanner
            error="sessionsd: authentication required (401)"
            onRetry={() => void refresh()}
          />
        ) : sessionWorkspace && dialogOpen === 'new' ? (
          <NewSessionDialog
            embedded
            onClose={() => setDialogOpen(null)}
            onStarted={openSession}
            onOpenResume={(providerId) => setDialogOpen(providerId ? { resumeProviderId: providerId } : 'resume')}
          />
        ) : sessionWorkspace && sessions.length === 0 && !sessionsHydrated ? (
          <SessionsWorkspaceSkeleton />
        ) : sessionWorkspace && isMobile && !mobileSessionDetail ? (
          <SessionNavigator
            sessions={sessions}
            activeId={activeId}
            machine={machine}
            onOpen={openSession}
            onOpenMachineSession={openFleetSession}
            onNew={openNewSession}
            onContinue={() => setDialogOpen('resume')}
            onResumeSession={resumeSession}
            onForkSession={forkSession}
            onStartLinked={(id) => setDialogOpen({ delegateFrom: id })}
            openSessionIds={openTabIds}
            onCloseView={closeTab}
            onReparent={updateDisplayParent}
          />
        ) : effectiveLayout === 'home' ? (
          <HomeView sessions={sessions} machine={machine} onOpen={openSession} onNew={openNewSession} onNavigate={(view) => setLayoutMode(view)} />
        ) : effectiveLayout === 'fleet' ? (
          <FleetView onOpenSession={openFleetSession} onOpenMachine={openFleetMachine} />
        ) : effectiveLayout === 'today' ? (
          <DailyView />
        ) : effectiveLayout === 'search' ? (
          <SearchView
            onResumeConversation={continueExactConversation}
          />
        ) : effectiveLayout === 'usage' ? (
          <UsageDashboard />
        ) : effectiveLayout === 'settings' || effectiveLayout === 'feedback' || effectiveLayout === 'connections' ? (
          <SettingsView
            theme={theme}
            onThemeChange={setTheme}
            textSize={textSize}
            onTextSizeChange={changeTextSize}
            initialSection={effectiveLayout === 'feedback' ? 'support' : effectiveLayout === 'connections' ? 'network' : 'general'}
          />
        ) : effectiveLayout === 'grid' ? (
          liveSessions.length > 0 ? (
            <GridView
              sessions={liveSessions}
              statusBySession={statusBySession}
              iconBySession={iconBySession}
              onEnd={(id) => kill(id)}
              // Optional expand affordance — exposed via the ⤢ button
              // on each cell. Click on the cell body itself focuses the
              // input rather than switching to tabs view.
              onExpand={(id) => { setActive(id); setLayoutMode('tabs'); }}
            />
          ) : recoveryVisible ? (
            <SessionsWorkspaceSkeleton />
          ) : (
            <EmptyState onNew={openNewSession} />
          )
        ) : openedSessions.length === 0 ? (
          recoveryVisible ? (
            <SessionsWorkspaceSkeleton />
          ) : sessions.length > 0 ? (
            <div className="session-workspace-empty"><span>Operations inbox</span><h1>Select a session</h1><p>Choose a manager or child from the navigator. It will open here without resuming or restarting anything.</p><button type="button" className="btn btn-primary" onClick={openNewSession}>＋ New Session</button></div>
          ) : (
            <EmptyState onNew={openNewSession} />
          )
        ) : (
          // Mount a SessionView only for the LIVE set (active + a few
          // recently-viewed), not all N sessions — see LIVE_SESSION_CAP.
          // The active one is always included even if the LRU effect hasn't
          // caught up yet. Visibility within the live set is still a CSS
          // display toggle, so switching between recently-viewed tabs stays
          // instant; switching to a long-dormant one re-mounts and
          // snapshot-prefills (fast, not blank). Every session still appears
          // in the tab strip above regardless of live state.
          sessions
            .filter((s) => s.id === activeId || liveIds.includes(s.id))
            .map((s) => (
              <div
                key={s.id}
                data-session-id={s.id}
                className={`session-view-host${s.id === activeId ? '' : ' is-hidden'}`}
              >
                <SessionView
                  sessionId={s.id}
                  isActive={s.id === activeId}
                  onStatusChange={s.id === activeId ? setActiveStatus : undefined}
                  onResume={resumeSession}
                  onFork={forkSession}
                  onCloseView={closeTab}
                  onOpenSession={openSession}
                  onBack={isMobile ? () => setMobileSessionDetail(false) : undefined}
                />
              </div>
            ))
        )}
      </main>
        </section>
      </div>

      <MobileNav
        layoutMode={effectiveLayout === 'grid' ? 'tabs' : effectiveLayout === 'feedback' ? 'settings' : effectiveLayout}
        showingSessionDetail={effectiveLayout === 'tabs' && mobileSessionDetail}
        onLayoutChange={(mode) => {
          setLayoutMode(mode);
          if (mode === 'tabs') setMobileSessionDetail(false);
        }}
        onShowSessions={() => {
          setLayoutMode('tabs');
          setMobileSessionDetail(false);
        }}
      />

      <CommandPalette
        open={commandPaletteOpen}
        sessions={sessions}
        onClose={() => setCommandPaletteOpen(false)}
        onNavigate={navigateProduct}
        onNewSession={openNewSession}
        onContinue={() => setDialogOpen('resume')}
        onOpenSession={openSession}
      />

      {dialogOpen === 'resume' || (dialogOpen && typeof dialogOpen === 'object' && 'resumeProviderId' in dialogOpen) ? (
        <ResumeDialog
          onClose={() => setDialogOpen(null)}
          onResumed={(laneId) => openSession(laneId)}
          onStartNew={openNewSession}
          preferredProviderId={typeof dialogOpen === 'object' && 'resumeProviderId' in dialogOpen
            ? dialogOpen.resumeProviderId
            : undefined}
          preferredSourceSessionId={typeof dialogOpen === 'object' && 'resumeProviderId' in dialogOpen
            ? dialogOpen.sourceSessionId
            : undefined}
          preferredHistoryId={typeof dialogOpen === 'object' && 'resumeProviderId' in dialogOpen
            ? dialogOpen.historyId
            : undefined}
          preferredDestinationProvider={typeof dialogOpen === 'object' && 'resumeProviderId' in dialogOpen
            ? dialogOpen.destinationProvider
            : undefined}
          preferredRuntimeMode={typeof dialogOpen === 'object' && 'resumeProviderId' in dialogOpen
            ? dialogOpen.runtimeMode
            : undefined}
        />
      ) : null}
      {dialogOpen && typeof dialogOpen === 'object' && 'delegateFrom' in dialogOpen ? (
        <NewSessionDialog
          parentSession={sessions.find((session) => session.id === dialogOpen.delegateFrom) ?? null}
          onClose={() => setDialogOpen(null)}
          onStarted={openSession}
        />
      ) : null}
      {onboarding && onboarding.supported !== false && !onboarding.complete ? (
        <OnboardingDialog
          machine={machine}
          busy={onboardingBusy}
          error={onboardingError}
          onChoose={chooseOnboardingPreference}
        />
      ) : null}
    </div>
  );
}

// Daemon-unreachable banner — shown when the first session-list fetch
// fails and we have no live data (hydrated is false). Two variants:
//   • Auth (401): token input + save + retry.
//   • Network: shows host:port so the user knows which sessionsd to check.
//
// Auth detection: stream E's api/sessionsd.ts throws an AuthError for 401
// responses; the sessions store stores its message. We check for '401'
// in the string — stable regardless of the exact message wording.
// updateServer is added by stream E to lib/servers.ts; we call it via
// getState() with a runtime guard so this compiles without a type cast.
function DaemonBanner({
  error,
  onRetry
}: {
  error: string;
  onRetry: () => void;
}): JSX.Element {
  const isAuthError = /\b401\b/.test(error);
  const server = getActiveServer();
  const [tokenInput, setTokenInput] = useState('');
  const [tokenSaveError, setTokenSaveError] = useState('');

  const handleTokenSubmit = async (): Promise<void> => {
    const token = tokenInput.trim();
    if (!token) return;
    // Save the pasted token onto the active server config, then retry.
    setTokenSaveError('');
    try {
      await useServers.getState().updateServer(server.id, { token });
      onRetry();
    } catch (reason) {
      setTokenSaveError(
        reason instanceof Error
          ? reason.message
          : 'Sessions could not protect and save this machine credential.'
      );
    }
  };

  return (
    <div className="daemon-banner">
      {isAuthError ? (
        <>
          <p className="daemon-banner-title">Authentication required</p>
          <p className="daemon-banner-host">{formatServerEndpoint(server)}</p>
          <p className="daemon-banner-hint">Enter the daemon token to connect.</p>
          <div className="daemon-banner-token-row">
            <input
              type="password"
              className="daemon-banner-token-input"
              placeholder="Token"
              value={tokenInput}
              autoFocus
              onChange={(e) => setTokenInput(e.target.value)}
              onKeyDown={(e) => { if (e.key === 'Enter') void handleTokenSubmit(); }}
            />
            <button
              type="button"
              className="btn btn-primary daemon-banner-token-submit"
              disabled={!tokenInput.trim()}
              onClick={() => void handleTokenSubmit()}
            >
              Connect
            </button>
          </div>
          {tokenSaveError ? <p className="daemon-banner-hint" role="alert">{tokenSaveError}</p> : null}
        </>
      ) : (
        <>
          <p className="daemon-banner-title">Daemon unreachable</p>
          <p className="daemon-banner-host">{server.host}:{server.port}</p>
          <p className="daemon-banner-hint">
            sessionsd is not responding. Check that it is running on{' '}
            <strong>{server.host}</strong> port <strong>{server.port}</strong>.
          </p>
          <button
            type="button"
            className="btn daemon-banner-retry"
            onClick={onRetry}
          >
            Retry
          </button>
        </>
      )}
    </div>
  );
}

// Pop-out window shell. Renders a minimal top bar showing WHICH session
// this window is attached to (cwd basename + parser icon + working
// indicator) so the user can tell windows apart at a glance. Also
// drives document.title — Tauri's native window title is initially
// set in Rust via .title(...), but the WebView replaces it with the
// HTML <title> on load; setting document.title here keeps the macOS
// title bar in sync with the session.
function SinglePopOut({
  sessionId,
  sessions,
  textSize
}: {
  sessionId: string;
  sessions: import('./types').SessionInfo[];
  textSize: import('./lib/textSize').TextSize;
}): JSX.Element {
  const session = sessions.find((s) => s.id === sessionId);
  const overrideLabel = useTabLabel(sessionId);
  // Display label — same resolution as SessionTabs:
  //   user sessions override > claude custom > claude ai-title > cwd > cmd > short id.
  const label = useMemo(() => {
    if (overrideLabel) return overrideLabel;
    if (!session) return 'session';
    if (session.claudeCustomTitle) return session.claudeCustomTitle;
    if (session.claudeAiTitle) return session.claudeAiTitle;
    if (session.cwd) {
      const parts = session.cwd.split('/').filter(Boolean);
      const last = parts[parts.length - 1];
      if (last) return last;
    }
    return session.cmd || session.id.slice(0, 6);
  }, [overrideLabel, session]);

  const [status, setStatus] = useState<ActiveStatus>(INITIAL_STATUS);
  const cwdShort = useMemo(() => {
    const c = session?.cwd ?? '';
    if (!c) return '';
    // Shorten the OS home dir to ~ for compactness, without hardcoding a
    // username — match the standard macOS (/Users/<user>) and Linux
    // (/home/<user>) home layouts so it works for any operator.
    return c.replace(/^\/(Users|home)\/[^/]+/, '~');
  }, [session?.cwd]);

  // Keep the OS window title (and tab title) in sync with the session
  // and its live status. The working glyph in the title is a useful
  // peripheral signal when the window is in the background.
  useEffect(() => {
    const workingMark = status.isWorking ? '✻ ' : '';
    document.title = `${workingMark}${label} — Sessions`;
  }, [label, status.isWorking]);

  return (
    <div className={`app-shell single-mode text-size-${textSize.toLowerCase()}`} onClickCapture={handleExternalLinkClick}>
      <header className="single-mode-header">
        <ParserIcon icon={status.parserIcon} size={18} />
        <span className="single-mode-label">{label}</span>
        {cwdShort ? <span className="single-mode-cwd">{cwdShort}</span> : null}
        <span className="single-mode-spacer" />
        {status.isWorking ? (
          <span className="single-mode-working" aria-label="working">✻ working</span>
        ) : (
          <span className="single-mode-idle" aria-label="idle">○ idle</span>
        )}
      </header>
      <SessionView
        key={sessionId}
        sessionId={sessionId}
        isActive
        preferFullTerminal
        onStatusChange={setStatus}
        onOpenSession={(nextSessionId) => {
          const next = new URL(window.location.href);
          next.searchParams.set('session', nextSessionId);
          window.location.assign(next);
        }}
      />
    </div>
  );
}
