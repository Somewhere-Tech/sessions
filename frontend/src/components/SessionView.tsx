import { memo, useCallback, useEffect, useRef, useState } from 'react';
import { useTerminal } from '../hooks/useTerminal';
import { useSessionSidebar } from '../hooks/useSessionSidebar';
import { RemoteView } from './RemoteView';
import { ScrollToBottomButton } from './ScrollToBottomButton';
import { useSessions } from '../store/sessions';
import { fetchServerHistoryTranscript, wsMuxUrl } from '../api/sessionsd';
import { classifySnapshotComposerState } from '../lib/detectMultiChoice';
import { requestSnapshot } from '../lib/wsMux';
import { SessionDetails } from './SessionDetails';
import { ProviderBadge, normalizeProvider } from './ProviderBadge';
import { getActiveServer } from '../lib/servers';
import { sessionLabel, useTabLabel } from '../lib/tabLabels';
import { SessionHistoryView } from './SessionHistoryView';
import { isCrashedSession, isDegradedSession } from '../lib/sessionStatus';
import { sessionMode, sessionModeName, sessionModeShort } from '../lib/sessionMode';
import { SessionPopOutButton } from './SessionPopOutButton';
import { MachineMark } from './MachineMark';
import { readInitialSessionView, writeSessionView, type SessionViewMode } from '../lib/sessionViewPreference';
import { LoadingShell } from './LoadingShell';

import type { ActiveStatus } from '../App';

interface Props {
  sessionId: string;
  onStatusChange?: (s: ActiveStatus) => void;
  // True when this is the currently-focused session tab. Used to gate
  // expensive per-session work (snapshot polling for picker detection)
  // so we don't burn N×CPU for sessions the user isn't looking at.
  isActive?: boolean;
  onResume?: (
    session: import('../types').SessionInfo,
    destinationProvider?: 'claude' | 'codex',
    runtimeMode?: 'rich' | 'terminal',
    remoteControl?: boolean
  ) => void;
  onFork?: (
    session: import('../types').SessionInfo,
    destinationProvider: 'claude' | 'codex',
    point?: { index: number; messageId: string }
  ) => Promise<void>;
  onCloseView?: (sessionId: string) => void;
  onOpenSession?: (sessionId: string) => void;
  onBack?: () => void;
  preferFullTerminal?: boolean;
}

// View modes:
//   • terminal — raw xterm, sized to whatever screen we're viewing on.
//     For TUI work (slash-command pickers, vim, raw shell output).
//   • remote   — provider-neutral conversation GUI. Sources its message
//     log from Claude JSONL or Codex's normalized/app-server event stream.
//     Stable identities, structured activity, no TUI scraping.
//
// The old `sessions`, `split`, and `reflowed` modes were retired: Remote
// supersedes them for chat reading, and a viewport-sized Terminal view
// covers everything else without the parser pipeline.
type ViewMode = SessionViewMode;
const TERMINAL_NOTICE_ACK_PREFIX = 'sessions:terminal-notice-ack:';

// SessionView instances stay mounted while the app is open. Keep this outside
// React so only one compatibility explanation can appear during an app launch,
// even when the user has several Terminal sessions open.
let terminalNoticeShownThisLaunch = false;

// Owns useTerminal for the active session and exposes a Terminal /
// Sessions layout. The terminal stream stays the source of truth — its
// xterm instance stays mounted across mode toggles so the raw terminal
// is always one click away.
//
// memo()'d: all 36 SessionViews stay mounted, and App re-renders every 3s
// (session poll). Without memo, that parent re-render re-renders every
// child; with it (plus stable session refs from reconcileSessions), an
// unchanged session's view skips the poll entirely. Props are all stable
// per session (sessionId; onStatusChange is setActiveStatus for the active
// tab and undefined otherwise; isActive flips only on switch).
function SessionViewInner({ sessionId, onStatusChange, isActive = false, onResume, onFork, onCloseView, onBack, preferFullTerminal = false }: Props): JSX.Element {
  const [viewMode, setViewMode] = useState<ViewMode>(() => readInitialSessionView(sessionId));
  const [detailsOpen, setDetailsOpen] = useState(false);
  const [terminalExpanded, setTerminalExpanded] = useState(false);
  const sessionViewRef = useRef<HTMLDivElement>(null);
  const terminalModePillRef = useRef<HTMLSpanElement>(null);
  const session = useSessions((s) => s.sessions.find((x) => x.id === sessionId)) ?? null;
  const allSessions = useSessions((s) => s.sessions);
  const endSession = useSessions((s) => s.kill);
  const updateName = useSessions((s) => s.updateName);
  const updateModel = useSessions((s) => s.updateModel);
  const customLabel = useTabLabel(sessionId, session?.cwd);

  // Claude and Codex both expose structured conversation history. Codex TUI
  // rollouts use the normalized event adapter; codex-app-server sessions add
  // live deltas, plans, commands, file diffs, reasoning summaries, and usage.
  // Raw shell sessions remain terminal-only.
  const supportsConversation = !session || session.tool === 'claude-code' || session.tool === 'codex';
  const effectiveView: ViewMode = supportsConversation
    ? viewMode
    : 'terminal';
  const displayParentID = session?.displayParentSessionId !== undefined
    ? session.displayParentSessionId
    : session?.parentSessionId;
  const parent = displayParentID ? allSessions.find((item) => item.id === displayParentID) : null;
  const crashedSession = session ? isCrashedSession(session) : false;
  const degradedSession = session ? isDegradedSession(session) : false;
  const provider = normalizeProvider(session?.tool);
  const richSession = Boolean(session && sessionMode(session) === 'rich');
  const workspaceName = session?.cwd.split('/').filter(Boolean).pop() || 'Workspace';
  const terminalBackedAgent = Boolean(
    session
    && session.tool !== 'terminal'
    && !richSession
  );
  // Claude's Conversation view is sourced from its canonical JSONL, not its
  // screen. Only the Codex PTY compatibility adapter needs the one-time
  // warning about incomplete terminal interpretation.
  const terminalCompatibilityAgent = Boolean(
    session
    && session.tool === 'codex'
    && !richSession
  );
  const terminalDrawerOpen = Boolean(
    supportsConversation
    && effectiveView === 'terminal'
    && !onBack
    && !preferFullTerminal
    && !richSession
  );
  const terminalWarningKey = session ? `sessions:terminal-runtime-warning:${session.tool}` : '';
  const terminalNoticeAckKey = `${TERMINAL_NOTICE_ACK_PREFIX}${sessionId}`;
  const [terminalWarningDismissed, setTerminalWarningDismissed] = useState(() => {
    if (!terminalWarningKey) return false;
    try {
      return window.localStorage.getItem(terminalWarningKey) === 'dismissed';
    } catch {
      return false;
    }
  });
  const [terminalNoticeAcknowledged, setTerminalNoticeAcknowledged] = useState(() => {
    try {
      return window.localStorage.getItem(terminalNoticeAckKey) === 'acknowledged';
    } catch {
      return false;
    }
  });
  const [terminalNoticeOpen, setTerminalNoticeOpen] = useState(false);

  // Sticky "have we ever needed xterm for this session?" Once true,
  // stays true so toggling Sessions↔Terminal doesn't tear down xterm.
  // Starts true if Terminal is the persisted view; otherwise false
  // until the user clicks Terminal. With keep-mounted SessionViews
  // and ~8 sessions open, this saves ~2MB of memory + N×xterm DOM
  // trees when the user lives in Sessions view.
  const [hasMountedTerminal, setHasMountedTerminal] = useState(viewMode === 'terminal');
  // Brief toolbar notice shown when the auto-switch fires (picker
  // detected → Terminal). The draft in InputBar IS preserved because
  // RemoteView stays mounted (only CSS-hidden) — we tell the user
  // explicitly so they're not alarmed when they can't see the draft.
  const [pickerNotice, setPickerNotice] = useState(false);
  useEffect(() => {
    if (effectiveView === 'terminal' && !hasMountedTerminal) setHasMountedTerminal(true);
  }, [effectiveView, hasMountedTerminal]);

  const term = useTerminal(sessionId, hasMountedTerminal && !richSession, isActive);
  const sidebar = useSessionSidebar({
    session,
    events: term.claudeEvents,
    daemonWorking: session?.working ?? false
  });
  const statusLabel = sidebar.isWorking
    ? 'Working'
    : degradedSession
    ? 'Limited'
    : crashedSession
    ? 'Failed'
    : session?.idleReason === 'needs-input'
    ? 'Needs you'
    : session?.exited || session?.idleReason === 'completed'
    ? 'Finished'
    : session?.idleReason === 'never-started'
    ? 'Not started'
    : 'Idle';
  const statusTone = sidebar.isWorking
    ? ' is-running'
    : degradedSession
    ? ' is-limited'
    : crashedSession
    ? ' is-failed'
    : session?.idleReason === 'needs-input'
    ? ' is-needs-you'
    : session?.exited || session?.idleReason === 'completed'
    ? ' is-finished'
    : '';

  useEffect(() => {
    writeSessionView(viewMode);
  }, [viewMode]);

  useEffect(() => {
    if (!terminalWarningKey) return;
    try {
      setTerminalWarningDismissed(window.localStorage.getItem(terminalWarningKey) === 'dismissed');
    } catch {
      setTerminalWarningDismissed(false);
    }
  }, [terminalWarningKey]);

  useEffect(() => {
    try {
      setTerminalNoticeAcknowledged(window.localStorage.getItem(terminalNoticeAckKey) === 'acknowledged');
    } catch {
      setTerminalNoticeAcknowledged(false);
    }
    setTerminalNoticeOpen(false);
  }, [terminalNoticeAckKey]);

  const acknowledgeTerminalNotice = useCallback((returnFocus = true): void => {
    try {
      window.localStorage.setItem(terminalNoticeAckKey, 'acknowledged');
    } catch {
      // The explanation can still be closed for this mounted view.
    }
    setTerminalNoticeAcknowledged(true);
    setTerminalNoticeOpen(false);
    if (returnFocus) {
      requestAnimationFrame(() => terminalModePillRef.current?.focus());
    }
  }, [terminalNoticeAckKey]);

  const dismissTerminalNoticeForProvider = useCallback((): void => {
    try {
      window.localStorage.setItem(terminalWarningKey, 'dismissed');
    } catch {
      // The explanation can still be closed for this mounted view.
    }
    setTerminalWarningDismissed(true);
    setTerminalNoticeOpen(false);
    requestAnimationFrame(() => terminalModePillRef.current?.focus());
  }, [terminalWarningKey]);

  // Explain Terminal compatibility mode once, after the selected
  // Conversation view has settled. The popover is absolutely positioned and
  // never participates in the session grid, so it cannot resize xterm.
  useEffect(() => {
    if (
      !isActive
      || effectiveView !== 'remote'
      || detailsOpen
      || !terminalCompatibilityAgent
      || terminalWarningDismissed
      || terminalNoticeAcknowledged
      || terminalNoticeShownThisLaunch
    ) return;

    const id = window.setTimeout(() => {
      terminalNoticeShownThisLaunch = true;
      setTerminalNoticeOpen(true);
    }, 400);
    return () => window.clearTimeout(id);
  }, [
    detailsOpen,
    effectiveView,
    isActive,
    terminalCompatibilityAgent,
    terminalNoticeAcknowledged,
    terminalWarningDismissed
  ]);

  // Switching to Terminal, opening Details, or leaving this session is an
  // implicit "Okay". It is deliberately not a permanent provider dismissal.
  useEffect(() => {
    if (!terminalNoticeOpen) return;
    if (isActive && effectiveView === 'remote' && !detailsOpen) return;
    acknowledgeTerminalNotice(false);
  }, [acknowledgeTerminalNotice, detailsOpen, effectiveView, isActive, terminalNoticeOpen]);

  useEffect(() => {
    if (!terminalNoticeOpen) return;
    const onKeyDown = (event: KeyboardEvent): void => {
      if (event.key !== 'Escape') return;
      const root = sessionViewRef.current;
      if (!root || !root.contains(document.activeElement)) return;
      event.preventDefault();
      acknowledgeTerminalNotice();
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, [acknowledgeTerminalNotice, terminalNoticeOpen]);

  const sendInput = useCallback((data: string) => {
    term.sendInputRef.current(data);
  }, [term.sendInputRef]);

  const scrollTerminalToBottom = useCallback((): void => {
    term.scrollTerminalToBottomRef.current();
  }, [term.scrollTerminalToBottomRef]);

  const focusTerminal = useCallback((): void => {
    term.focusTerminalRef.current();
  }, [term.focusTerminalRef]);

  const loadEarlierClaudeEvents = useCallback((): void => {
    term.loadEarlierClaudeEventsRef.current();
  }, [term.loadEarlierClaudeEventsRef]);

  const continueInTerminal = useCallback(async (enableRemoteControl: boolean): Promise<void> => {
    if (!session || !onResume) {
      throw new Error('Open Sessions in the main window to continue this chat in Terminal.');
    }
    if (!richSession || session.tool !== 'claude-code') {
      throw new Error('This action is available for Rich Claude sessions.');
    }
    if (session.working) {
      throw new Error('Claude is still working. Wait for this turn to finish; your draft will stay in the composer.');
    }
    await endSession(
      session.id,
      enableRemoteControl
        ? 'Continuing the same Claude conversation in Terminal with Remote Control.'
        : 'Continuing the same Claude conversation in Terminal for slash commands.'
    );
    onResume(session, 'claude', 'terminal', enableRemoteControl);
  }, [endSession, onResume, richSession, session]);

  const forkFromVisibleMessage = useCallback(async (
    message: { role: 'user' | 'assistant'; content: string; createdAt: number },
    destinationProvider: 'claude' | 'codex'
  ): Promise<void> => {
    if (!session || !onFork) {
      throw new Error('Open Sessions in the main window to fork this conversation.');
    }
    const transcript = await fetchServerHistoryTranscript(getActiveServer(), session.id);
    const candidates = transcript.messages.filter((candidate) =>
      candidate.role === message.role && candidate.text.trim() === message.content.trim()
    );
    if (candidates.length === 0) {
      throw new Error('This message is not in the durable provider history yet. Wait a moment and try again.');
    }
    const selected = candidates.reduce((best, candidate) => {
      const candidateTime = candidate.timestamp ? Date.parse(candidate.timestamp) : Number.NaN;
      const bestTime = best.timestamp ? Date.parse(best.timestamp) : Number.NaN;
      const candidateDistance = Number.isFinite(candidateTime)
        ? Math.abs(candidateTime - message.createdAt)
        : Number.POSITIVE_INFINITY;
      const bestDistance = Number.isFinite(bestTime)
        ? Math.abs(bestTime - message.createdAt)
        : Number.POSITIVE_INFINITY;
      return candidateDistance < bestDistance ? candidate : best;
    });
    await onFork(session, destinationProvider, { index: selected.index, messageId: selected.id });
  }, [onFork, session]);

  // Put the cursor in the terminal when this tab becomes the active,
  // terminal-viewed session. Tab switches are a CSS display toggle (no
  // socket reconnect → no 'open'-status focus), so without this you'd
  // land on a visible terminal that isn't focused and have to hunt for
  // the right pixel to click. rAF waits for the pane to be display:flex
  // (it was display:none a frame ago) so focus() actually takes.
  useEffect(() => {
    if (!isActive || effectiveView !== 'terminal' || !hasMountedTerminal) return;
    const id = requestAnimationFrame(() => {
      term.fitTerminalRef.current();
      focusTerminal();
    });
    return () => cancelAnimationFrame(id);
  }, [isActive, effectiveView, hasMountedTerminal, focusTerminal, term.fitTerminalRef, terminalExpanded]);

  // Auto-switch to Terminal when Claude's TUI shows a numbered-choice
  // picker. The picker needs arrow-keys + Enter to interact, which the
  // chat input can't deliver — typing a number lands as a normal user
  // message instead of selecting the option. Polls the server snapshot
  // every 2s while this session is the active tab and we're in Sessions
  // view. Auto-switch fires once per picker (when it transitions from
  // absent → present); user can manually switch back to Sessions if they
  // want, and the next picker triggers a fresh switch.
  const lastPickerSeenRef = useRef(false);
  useEffect(() => {
    if (!isActive || session?.tool !== 'claude-code' || !terminalBackedAgent) return;
    if (effectiveView !== 'remote') return;
    let alive = true;
    const tick = async (): Promise<void> => {
      try {
        const snap = await requestSnapshot(wsMuxUrl(), sessionId);
        if (!alive) return;
        const present = Boolean(
          snap
          && classifySnapshotComposerState(snap.text).kind !== 'normal-composer'
        );
        if (present && !lastPickerSeenRef.current) {
          lastPickerSeenRef.current = true;
          setViewMode('terminal');
          // Tell the user their InputBar draft is safe — RemoteView is
          // CSS-hidden, not unmounted, so state is preserved. Without
          // this notice the user sees the Terminal pane appear with no
          // explanation and may think their draft was lost.
          setPickerNotice(true);
        } else if (!present) {
          lastPickerSeenRef.current = false;
        }
      } catch { /* transient — try again next tick */ }
    };
    void tick();
    const id = window.setInterval(() => { void tick(); }, 2000);
    return () => { alive = false; window.clearInterval(id); };
  }, [sessionId, session?.tool, isActive, effectiveView, terminalBackedAgent]);

  // Auto-clear the picker notice after 4s.
  useEffect(() => {
    if (!pickerNotice) return;
    const id = window.setTimeout(() => setPickerNotice(false), 4000);
    return () => window.clearTimeout(id);
  }, [pickerNotice]);

  // Push the active-session status up to App so the tab strip and mobile
  // nav reflect it.
  useEffect(() => {
    if (!onStatusChange) return;
    onStatusChange({
      isWorking: sidebar.isWorking,
      parserIcon: sidebar.parserIcon,
      parserName: sidebar.parserName,
      terminalStatus: term.status
    });
  }, [
    onStatusChange,
    sidebar.isWorking,
    sidebar.parserIcon,
    sidebar.parserName,
    term.status
  ]);

  return (
    <div ref={sessionViewRef} className={`session-view view-${effectiveView}${terminalDrawerOpen ? ' has-terminal-drawer' : ''}${terminalDrawerOpen && terminalExpanded ? ' terminal-drawer-expanded' : ''}${detailsOpen ? ' view-details' : ''}${session?.continuedFromHistoryId ? ' has-continuation' : ''}`}>
      <header className="session-active-header">
        {onBack ? <button type="button" className="mobile-session-back" onClick={onBack} aria-label="Back to sessions">‹</button> : null}
        <div className="session-active-copy">
          {parent ? <span className="session-parent-breadcrumb">{sessionLabel(parent)} <span>/</span> {session?.displayParentSessionId !== undefined ? 'grouped session' : 'child session'}</span> : null}
          <div className="session-active-title-row">
            <h1>{customLabel ?? (session ? sessionLabel(session) : 'Session')}</h1>
            <span className={`session-live-pill${statusTone}`}>{statusLabel}</span>
            {session ? (
              <span className="session-runtime-anchor">
                <span
                  ref={terminalModePillRef}
                  className={`session-runtime-badge${sessionMode(session) === 'terminal' && session.tool !== 'claude-code' ? ' is-terminal' : ''}`}
                  title={sessionModeName(session)}
                  tabIndex={terminalCompatibilityAgent ? 0 : undefined}
                  aria-describedby={terminalNoticeOpen ? `terminal-runtime-notice-${sessionId}` : undefined}
                >
                  {sessionModeShort(session)}
                </span>
                {terminalNoticeOpen ? (
                  <aside
                    id={`terminal-runtime-notice-${sessionId}`}
                    className="terminal-runtime-notice"
                    role="status"
                    aria-live="polite"
                  >
                    <strong>This Codex session uses its terminal interface</strong>
                    <p>
                      Some status, messages, and tool activity can be delayed or incomplete. Terminal always shows exactly what Codex printed.
                    </p>
                    <p className="terminal-runtime-notice-rich">
                      Rich Codex sessions receive the same information through its app-server protocol.
                    </p>
                    <div className="terminal-runtime-notice-actions">
                      <button type="button" className="btn btn-secondary" onClick={() => acknowledgeTerminalNotice()}>Okay</button>
                      <button type="button" className="btn btn-ghost" onClick={dismissTerminalNoticeForProvider}>Don’t show again</button>
                    </div>
                    <small>Applies only to Codex terminal sessions.</small>
                  </aside>
                ) : null}
              </span>
            ) : null}
          </div>
          <div className="session-active-meta">
            {provider ? <ProviderBadge provider={provider} compact size={20} /> : <span className="provider-badge is-shell is-compact">⌘ Shell</span>}
            <MachineMark machine={getActiveServer().name} size={18} />
            {session?.profile ? <span>{session.profile}</span> : null}
            <span title={session?.cwd}>{workspaceName}</span>
          </div>
        </div>
        <div className="session-active-actions">
          {session ? <SessionPopOutButton sessionId={session.id} label={customLabel ?? sessionLabel(session)} /> : null}
          {onCloseView ? <button type="button" className="btn btn-ghost session-close-view" onClick={() => onCloseView(sessionId)} title="Close this tab. The agent keeps running and remains in Live.">Close tab</button> : null}
        </div>
      </header>

      <div className="session-toolbar">
        <div className="view-toggle is-content-switch" aria-label="Conversation and terminal">
          {supportsConversation ? (
            <button type="button" className="view-toggle-btn is-active" onClick={() => setViewMode('remote')} title="Structured conversation, activity, plans, and usage">Conversation</button>
          ) : null}
          <button
            type="button"
            className={`view-toggle-btn terminal-drawer-toggle${effectiveView === 'terminal' ? ' is-active' : ''}`}
            onClick={() => {
              if (effectiveView === 'terminal' && supportsConversation) {
                setTerminalExpanded(false);
                setViewMode('remote');
              } else {
                setViewMode('terminal');
              }
            }}
            aria-expanded={terminalDrawerOpen}
            aria-controls={`terminal-pane-${sessionId}`}
            disabled={richSession}
            title={richSession ? 'Rich sessions do not have a terminal stream' : supportsConversation ? 'Show the exact provider terminal' : 'Terminal'}
          >
            {richSession ? 'No terminal' : effectiveView === 'terminal' && supportsConversation ? 'Hide terminal' : 'Terminal'}
          </button>
        </div>
        {term.status !== 'open' ? <span className="session-stream-status" role="status">{term.status === 'connecting' || term.status === 'reconnecting' ? 'Reconnecting…' : 'Conversation stream unavailable'}</span> : null}
        {session ? (
          <button
            type="button"
            className={`details-inspector-button${detailsOpen ? ' is-active' : ''}`}
            onClick={() => {
              if (!detailsOpen && supportsConversation) {
                setTerminalExpanded(false);
                setViewMode('remote');
              }
              setDetailsOpen((current) => !current);
            }}
          >
            {detailsOpen ? 'Close details' : 'Details'}
          </button>
        ) : null}
        {pickerNotice ? (
          <span className="status-picker-notice" aria-live="polite">
            Switched to Terminal for picker — your draft is preserved in Sessions view
          </span>
        ) : null}
      </div>
      {session?.continuedFromHistoryId ? (
        <div className="continuation-notice" role="status">
          <strong>
            Continued from {session.continuedFromProvider === 'codex' ? 'Codex' : 'Claude'}
          </strong>
          <span>
            {session.continuationMode === 'native-import'
              ? `${session.importedMessageCount ?? 0} authored messages were imported into this Codex conversation.`
              : `${session.importedMessageCount ?? 0} authored messages are shown here. Claude receives a local-search link on the first turn.`}
          </span>
          <small>The original conversation and its provider files were not modified.</small>
        </div>
      ) : null}
      {/* xterm host stays in the DOM in both modes so the buffer + WS
          stay alive while the user is reading Remote view. CSS hides
          whichever pane the active view-mode doesn't want. */}
      <div className="session-body">
        <div id={`terminal-pane-${sessionId}`} className="session-terminal-pane" onPointerDown={focusTerminal}>
          {terminalDrawerOpen ? (
            <header className="terminal-drawer-header">
              <span><strong>Terminal</strong>Exact provider view</span>
              <div>
                <button
                  type="button"
                  onClick={() => setTerminalExpanded((current) => !current)}
                  aria-label={terminalExpanded ? 'Restore terminal drawer size' : 'Expand terminal drawer'}
                  title={terminalExpanded ? 'Restore drawer size' : 'Expand terminal'}
                >
                  {terminalExpanded ? '↙' : '↗'}
                </button>
                <button type="button" onClick={() => { setTerminalExpanded(false); setViewMode('remote'); }} aria-label="Close terminal drawer">×</button>
              </div>
            </header>
          ) : null}
          {richSession ? (
            <div className="rich-terminal-empty">
              <span>Rich session</span>
              <h2>No terminal for this Rich session</h2>
              <p>Sessions is connected through the provider’s structured interface, which makes messages, plans, tool activity, diffs, and Stop more reliable.</p>
              <p>A Terminal compatibility session would be a separate runtime. Its screen-read status and history can be incomplete or delayed.</p>
              <small>To switch safely, end this runtime, choose Continue conversation, then select Terminal. Sessions will keep the same provider conversation and preserve this runtime in history.</small>
            </div>
          ) : (
            <>
              <div className="terminal-host" ref={term.containerRef} />
              <div className="mobile-terminal-keys" role="toolbar" aria-label="Terminal keys">
                <button type="button" onClick={() => sendInput('\x1b')}>Esc</button>
                <button type="button" onClick={() => sendInput('\x1b[A')}>↑ Prev</button>
                <button type="button" onClick={() => sendInput('\x1b[B')}>↓ Next</button>
                <button type="button" onClick={() => sendInput('\x03')}>Ctrl-C</button>
              </div>
              <ScrollToBottomButton
                visible={!term.terminalAtBottom}
                onClick={scrollTerminalToBottom}
              />
            </>
          )}
        </div>
        <div className="session-remote-pane">
          <RemoteView
            sessionId={sessionId}
            events={term.claudeEvents}
            historyPending={term.historyPending}
            send={sendInput}
            connected={term.status === 'open'}
            hasEarlierClaudeEvents={term.hasEarlierClaudeEvents}
            loadingEarlierClaudeEvents={term.loadingEarlierClaudeEvents}
            onLoadEarlierClaudeEvents={loadEarlierClaudeEvents}
            sidebar={sidebar}
            cwd={session?.cwd}
            onOpenTerminal={() => setViewMode('terminal')}
            terminalAvailable={!richSession}
            provider={session?.tool ?? 'claude-code'}
            model={session?.model}
            effort={session?.effort}
            modelControlSupported={Boolean(richSession && (session?.runnerProtocol ?? 0) >= 2)}
            onConfigureModel={session ? (model, effort) => updateModel(session.id, model, effort) : undefined}
            onRename={session ? (name) => updateName(session.id, name) : undefined}
            onContinueInTerminal={richSession && session?.tool === 'claude-code' && onResume
              ? continueInTerminal
              : undefined}
            onForkFromMessage={onFork ? forkFromVisibleMessage : undefined}
          />
        </div>
        <div className="session-details-pane">
          {session ? <SessionDetails session={session} allSessions={allSessions} onEnd={endSession} onResume={onResume} /> : null}
        </div>
      </div>
    </div>
  );
}

function SessionViewRouter(props: Props): JSX.Element {
  const session = useSessions((state) => state.sessions.find((item) => item.id === props.sessionId)) ?? null;
  const hydrated = useSessions((state) => state.hydrated);
  useEffect(() => {
    if (!session?.exited || !props.onStatusChange) return;
    props.onStatusChange({
      isWorking: false,
      parserIcon: session.tool === 'claude-code' ? '🟠' : session.tool === 'codex' ? '🟢' : '⬛',
      parserName: session.tool === 'claude-code' ? 'Claude' : session.tool === 'codex' ? 'Codex' : 'Terminal',
      terminalStatus: 'closed'
    });
  }, [props.onStatusChange, session?.exited, session?.tool]);
  if (!session && !hydrated) return <LoadingShell label="Loading this session" />;
  if (!session) {
    return (
      <div className="session-missing-shell" role="status">
        <span>Session unavailable</span>
        <h2>This session is no longer on this machine</h2>
        <p>It may have been archived, moved, or removed since this view opened.</p>
        {props.onBack ? <button type="button" className="btn btn-secondary" onClick={props.onBack}>Back to Sessions</button> : null}
      </div>
    );
  }
  if (session.exited) return <SessionHistoryView session={session} onResume={props.onResume} onFork={props.onFork} onCloseView={props.onCloseView} onOpenSession={props.onOpenSession} onBack={props.onBack} />;
  return <SessionViewInner {...props} />;
}

export const SessionView = memo(SessionViewRouter);
