import { memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { useDispatch } from '../hooks/useDispatch';
import { renderContent } from '../lib/contentRender';
import type { SessionSidebarState } from '../hooks/useSessionSidebar';
import type { ClaudeSessionEvent, SessionTool } from '../types';
import { InputBar } from './InputBar';
import { ScrollToBottomButton } from './ScrollToBottomButton';
import StatusSidebar from './StatusSidebar';
import { saveScrollPosition, readScrollPosition } from '../lib/scrollMemory';
import { eventsToMessages } from '../lib/claudeEvents';
import { snapshot as fetchServerSnapshot } from '../api/sessionsd';
import { classifySnapshotComposerState, type SnapshotComposerState } from '../lib/detectMultiChoice';
import type { DispatchMessage } from '../hooks/useDispatch';
import { ProviderMark, type Provider as ProviderIdentity } from './ProviderBadge';
import { CopyButton } from './CopyButton';
import { linkifyFilePaths } from '../lib/filePaths';
import { PlanPanel } from './RemotePlanPanel';

function renderFileReference(path: string, cwd = ''): string {
  const escaped = path
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
  return linkifyFilePaths(escaped, cwd);
}

interface Props {
  sessionId: string;
  // Provider-neutral structured history. Claude supplies JSONL records;
  // Codex supplies normalized rollout or app-server notifications.
  events: ClaudeSessionEvent[];
  historyPending: boolean;
  // Composer sends use the acknowledged path. Raw terminal keystrokes stay
  // inside SessionView and are never used for conversation dispatch.
  sendConfirmed: (data: string) => Promise<void>;
  submitMessage: (data: string) => Promise<void>;
  connected: boolean;
  sendAvailable?: boolean;
  hasEarlierClaudeEvents: boolean;
  loadingEarlierClaudeEvents: boolean;
  onLoadEarlierClaudeEvents: () => void;
  // Working / progress state computed from JSONL events (and the
  // daemon's byte-rate flag). Drives the activity strip + sidebar.
  sidebar: SessionSidebarState;
  // Session cwd for vscode://file/... linkification inside Claude's
  // markdown responses.
  cwd?: string;
  // Switches the parent SessionView to the raw terminal when the
  // snapshot classifier sees a prompt/menu instead of a composer.
  onOpenTerminal: () => void;
  terminalAvailable?: boolean;
  provider: SessionTool;
  model?: string;
  effort?: string;
  modelControlSupported?: boolean;
  onConfigureModel?: (model: string, effort: string) => Promise<void>;
  onRename?: (name: string) => Promise<void>;
  onContinueInTerminal?: (enableRemoteControl: boolean) => Promise<void>;
  onForkFromMessage?: (
    message: { role: 'user' | 'assistant'; content: string; createdAt: number },
    destinationProvider: 'claude' | 'codex'
  ) => Promise<void>;
  forkMode?: boolean;
  onExitForkMode?: () => void;
}

// Provider-neutral conversation view over the durable session transport.
// Provider history is authoritative; localStorage holds only optimistic send
// state until a matching user event confirms delivery. The raw terminal stays
// mounted as an independent fallback and never owns this projection.
//
// Update cadence is intentionally relaxed — the user explicitly said
// it doesn't need to be live as long as it's accurate. The sidebar
// is fed by useSessionSidebar (JSONL-derived) and the visible working
// state is gated on terminal stop_reasons rather than a guessed verb
// cycle.

export function RemoteView({
  sessionId,
  events,
  historyPending,
  sendConfirmed,
  submitMessage,
  connected,
  sendAvailable = connected,
  hasEarlierClaudeEvents,
  loadingEarlierClaudeEvents,
  onLoadEarlierClaudeEvents,
  sidebar,
  cwd,
  onOpenTerminal,
  terminalAvailable = true,
  provider,
  model,
  effort,
  modelControlSupported = false,
  onConfigureModel,
  onRename,
  onContinueInTerminal,
  onForkFromMessage,
  forkMode = false,
  onExitForkMode
}: Props): JSX.Element {
  const providerName = provider === 'codex' ? 'Codex' : 'Claude';
  const providerIdentity: ProviderIdentity = provider === 'codex' ? 'codex' : 'claude';
  // Event-derived user contents — passed to useDispatch so an acknowledged
  // local copy is replaced when provider history contains the same turn.
  // Computed once per events change; the Map is
  // stable across renders when its contents don't change so useDispatch's
  // effect doesn't re-run unnecessarily.
  const eventMessages = useMemo(() => eventsToMessages(events), [events]);
  // Occurrence COUNT per trimmed user content in the JSONL — a count, not
  // a set, so useDispatch can tell a genuinely-new re-send ("continue"
  // again) from a historical duplicate and not false-confirm it.
  const eventUserContentCounts = useMemo(() => {
    const counts = new Map<string, number>();
    for (const m of eventMessages) {
      if (m.role !== 'user' || m.status !== 'sent') continue;
      const c = m.content.trim();
      counts.set(c, (counts.get(c) ?? 0) + 1);
    }
    return counts;
  }, [eventMessages]);

  const { messages: dispatchMessages, recordSent, restoreDraft, remove, resetLog } = useDispatch({
    sessionId,
    eventUserContentCounts
  });
  const hasRecoverableLocalState = dispatchMessages.some(
    (message) => message.status === 'failed'
  );

  // JSONL events are the authoritative chat record. Merge in only the
  // dispatch log's acknowledged or legacy-failed user entries — sends that
  // haven't shown up in provider history yet. useDispatch flips an entry to
  // 'sent' (dropping it from this merge) as soon as a matching JSONL
  // occurrence appears. It is count-aware, so repeated identical turns are
  // matched one-for-one instead of being hidden by an old occurrence.
  const messages = useMemo<DispatchMessage[]>(() => {
    if (eventMessages.length === 0) return dispatchMessages;
    const providerFailures = new Map<string, number>();
    for (const message of eventMessages) {
      if (message.role !== 'user' || message.status !== 'failed') continue;
      const content = message.content.trim();
      providerFailures.set(content, (providerFailures.get(content) ?? 0) + 1);
    }
    const locallyHeld = dispatchMessages.filter(
      (message) => {
        if (message.role !== 'user' || (
          message.status !== 'accepted' && message.status !== 'queued' && message.status !== 'failed'
        )) return false;
        const content = message.content.trim();
        const failedCount = providerFailures.get(content) ?? 0;
        if (failedCount === 0) return true;
        providerFailures.set(content, failedCount - 1);
        return false;
      }
    );
    return [...eventMessages, ...locallyHeld]
      .map((message, index) => ({ message, index }))
      .sort((left, right) => left.message.createdAt - right.message.createdAt || left.index - right.index)
      .map(({ message }) => message);
  }, [eventMessages, dispatchMessages]);
  const changedFiles = useMemo(() => {
    const files = new Set<string>();
    for (const message of eventMessages) {
      for (const call of message.toolCalls ?? []) {
        if (call.kind !== 'fileChange') continue;
        for (const row of (call.inputFull ?? '').split('\n')) {
          const path = row.trim().replace(/^(?:add|added|create|created|delete|deleted|modify|modified|update|updated|rename|renamed)\s+/i, '');
          if (path) files.add(path);
        }
      }
    }
    return Array.from(files);
  }, [eventMessages]);
  const scrollRef = useRef<HTMLDivElement | null>(null);
  const initialPos = useMemo(
    () => readScrollPosition(sessionId, 'remote'),
    [sessionId]
  );
  const stickRef = useRef(initialPos ? initialPos.atBottom : true);
  const [atBottom, setAtBottom] = useState(initialPos ? initialPos.atBottom : true);

  // Tail window. Long sessions (adapted-co + others with hundreds of
  // turns) used to render every message on first paint, which made a
  // tab switch take 3+ seconds while markdown + code highlighting
  // chewed through the whole history. Now: only the last
  // TAIL_WINDOW_INITIAL bubbles are rendered; older ones unmount and
  // weigh nothing. When the user scrolls near the top we prepend
  // TAIL_WINDOW_STEP more, preserving their scroll anchor so they
  // don't get yanked. New messages always slide in at the bottom and
  // are inside the window by definition. Reset to the initial window
  // on session-tab switch so tab switches stay fast.
  const TAIL_WINDOW_INITIAL = 50;
  const TAIL_WINDOW_STEP = 50;
  const TAIL_EXPAND_TRIGGER_PX = 200;
  const [visibleCount, setVisibleCount] = useState(TAIL_WINDOW_INITIAL);
  useEffect(() => {
    setVisibleCount(TAIL_WINDOW_INITIAL);
  }, [sessionId]);

  const visibleMessages = useMemo(() => {
    if (messages.length <= visibleCount) return messages;
    return messages.slice(messages.length - visibleCount);
  }, [messages, visibleCount]);
  const hiddenCount = messages.length - visibleMessages.length;
  const latestFailedSend = useMemo(() => {
    for (let i = messages.length - 1; i >= 0; i--) {
      const m = messages[i]!;
      if (m.role === 'user' && m.status === 'failed') return m;
    }
    return null;
  }, [messages]);
  const recoverDraft = latestFailedSend
    ? { id: latestFailedSend.id, text: latestFailedSend.content, version: latestFailedSend.createdAt }
    : null;
  // The snapshot probe below only cares WHICH failed send it is looking at,
  // never its contents. Naming that identity makes the effect's dependency
  // list say exactly what the effect reads, instead of listing two fields of
  // an object the body then dereferences by another name.
  const failedSendKey = latestFailedSend
    ? `${latestFailedSend.id}:${latestFailedSend.createdAt}`
    : null;
  const [blockingState, setBlockingState] = useState<SnapshotComposerState | null>(null);
  const [forkPointId, setForkPointId] = useState<string | null>(null);
  const [forkBusy, setForkBusy] = useState(false);
  const [forkError, setForkError] = useState<string | null>(null);

  const forkVisibleMessage = useCallback(async (
    message: DispatchMessage,
    destinationProvider: 'claude' | 'codex'
  ): Promise<void> => {
    if (!onForkFromMessage || forkBusy) return;
    setForkBusy(true);
    setForkError(null);
    try {
      await onForkFromMessage({
        role: message.role,
        content: message.content,
        createdAt: message.createdAt
      }, destinationProvider);
      setForkPointId(null);
      onExitForkMode?.();
    } catch (reason) {
      setForkError(reason instanceof Error ? reason.message : 'Could not fork this conversation.');
    } finally {
      setForkBusy(false);
    }
  }, [forkBusy, onExitForkMode, onForkFromMessage]);

  useEffect(() => {
    if (forkMode) return;
    setForkPointId(null);
    setForkError(null);
  }, [forkMode]);

  useEffect(() => {
    // Snapshot prompt classification is a terminal-screen heuristic. Rich
    // sessions have structured provider events but no terminal stream, so
    // applying it there can turn ordinary conversation text into a false
    // "open Terminal" warning with an impossible action.
    if (!terminalAvailable || !failedSendKey) {
      setBlockingState(null);
      return;
    }

    let alive = true;
    const checkSnapshot = async (): Promise<void> => {
      try {
        const snap = await fetchServerSnapshot(sessionId);
        if (!alive) return;
        if (!snap) {
          setBlockingState(null);
          return;
        }
        const state = classifySnapshotComposerState(snap.text);
        setBlockingState(state.kind === 'normal-composer' ? null : state);
      } catch {
        if (alive) setBlockingState(null);
      }
    };

    void checkSnapshot();
    return () => { alive = false; };
  }, [failedSendKey, sessionId, terminalAvailable]);

  // Scroll-anchor preservation across window expansion. Prepending
  // older messages grows scrollHeight by ~the prepended block's
  // height; we add that delta to scrollTop so the visible content
  // doesn't jump. anchorRef is set BEFORE setVisibleCount fires, then
  // the layout effect below restores after React commits the larger
  // window.
  const anchorRef = useRef<{ scrollHeight: number; scrollTop: number } | null>(null);
  const beginAnchor = useCallback((): boolean => {
    const el = scrollRef.current;
    if (!el) return false;
    anchorRef.current = { scrollHeight: el.scrollHeight, scrollTop: el.scrollTop };
    return true;
  }, []);

  const expandWindow = useCallback((): void => {
    if (hiddenCount === 0) return;
    if (!beginAnchor()) return;
    setVisibleCount((n) => n + TAIL_WINDOW_STEP);
  }, [beginAnchor, hiddenCount]);
  const loadEarlier = useCallback((): void => {
    if (hiddenCount > 0) {
      expandWindow();
      return;
    }
    if (!hasEarlierClaudeEvents || loadingEarlierClaudeEvents) return;
    if (!beginAnchor()) return;
    setVisibleCount((n) => n + TAIL_WINDOW_STEP);
    onLoadEarlierClaudeEvents();
  }, [
    expandWindow,
    beginAnchor,
    hasEarlierClaudeEvents,
    hiddenCount,
    loadingEarlierClaudeEvents,
    onLoadEarlierClaudeEvents
  ]);
  useLayoutEffect(() => {
    const anchor = anchorRef.current;
    if (!anchor) return;
    const el = scrollRef.current;
    if (!el) return;
    const delta = el.scrollHeight - anchor.scrollHeight;
    if (delta === 0 && hiddenCount === 0) {
      if (loadingEarlierClaudeEvents) return;
      anchorRef.current = null;
      return;
    }
    el.scrollTop = anchor.scrollTop + delta;
    anchorRef.current = null;
  }, [hiddenCount, loadingEarlierClaudeEvents, messages.length, visibleCount]);

  // Auto-stick to bottom — but only if the user wasn't scrolling up to
  // read history. Depends on the full messages array, not just length,
  // because Claude streams content into existing assistant bubbles
  // (content grows, count doesn't). Without this, the scroll falls
  // behind whenever a long assistant reply expands. useLayoutEffect so
  // the scroll lands before paint and the user never sees a half-
  // scrolled frame.
  useLayoutEffect(() => {
    if (!stickRef.current) return;
    const el = scrollRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [messages]);

  // Restore saved scroll on session-tab switch (which remounts this
  // component). Runs before paint so the user never sees the top→pos
  // jump.
  useLayoutEffect(() => {
    if (!initialPos) return;
    const el = scrollRef.current;
    if (!el) return;
    if (initialPos.atBottom) el.scrollTop = el.scrollHeight;
    else el.scrollTop = initialPos.scrollTop;
  }, [initialPos]);

  const onScroll = (): void => {
    const el = scrollRef.current;
    if (!el) return;
    const isAtBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 60;
    stickRef.current = isAtBottom;
    setAtBottom((prev) => (prev === isAtBottom ? prev : isAtBottom));
    saveScrollPosition(sessionId, 'remote', { scrollTop: el.scrollTop, atBottom: isAtBottom });
    // Tail-window expansion trigger: user is scrolling up to read
    // older history. Expand the window so the next chunk of older
    // messages renders. Anchor preservation keeps them visually in
    // place — no perceptible jump.
    if (el.scrollTop < TAIL_EXPAND_TRIGGER_PX && anchorRef.current === null) {
      if (hiddenCount > 0) expandWindow();
      else if (hasEarlierClaudeEvents && !loadingEarlierClaudeEvents) loadEarlier();
    }
  };

  const scrollToBottom = useCallback((): void => {
    const el = scrollRef.current;
    if (!el) return;
    el.scrollTop = el.scrollHeight;
    stickRef.current = true;
    setAtBottom(true);
    saveScrollPosition(sessionId, 'remote', { scrollTop: el.scrollTop, atBottom: true });
  }, [sessionId]);

  // Two-step confirm for the destructive "clear log" action. window.confirm
  // is suppressed in Tauri/WebViews so we can't use it — instead we toggle
  // an inline confirm state that renders "Clear? [Clear] [Cancel]" in place
  // of the button. Pressing either option clears the confirm state.
  const [clearConfirm, setClearConfirm] = useState(false);
  const handleRefresh = (): void => setClearConfirm(true);
  const handleRefreshConfirm = (): void => { setClearConfirm(false); resetLog(); };
  const handleRefreshCancel = (): void => setClearConfirm(false);

  return (
    <div className="remote-view">
      {/* Recovery valve for legacy failed local-send artifacts. Provider
          history is never cleared. Uses inline confirmation because native
          WebViews suppress window.confirm. */}
      {hasRecoverableLocalState && clearConfirm ? (
        <div className="remote-refresh-confirm">
          <span className="remote-refresh-confirm-label">Clear local sends?</span>
          <button
            type="button"
            className="remote-refresh-confirm-yes"
            onClick={handleRefreshConfirm}
          >
            Clear
          </button>
          <button
            type="button"
            className="remote-refresh-confirm-no"
            onClick={handleRefreshCancel}
          >
            Cancel
          </button>
        </div>
      ) : hasRecoverableLocalState ? (
        <button
          type="button"
          className="remote-refresh"
          onClick={handleRefresh}
          title="Clear pending or failed local sends; provider history remains unchanged"
        >
          reset sends
        </button>
      ) : null}
      {changedFiles.length > 0 ? (
        <details className="remote-changes-strip">
          <summary>Changes <span>{changedFiles.length} loaded {changedFiles.length === 1 ? 'file' : 'files'}</span></summary>
          <div>
            {changedFiles.map((path) => (
              <code key={path} dangerouslySetInnerHTML={{ __html: renderFileReference(path, cwd) }} />
            ))}
          </div>
        </details>
      ) : null}
      <div
        className="remote-scroll"
        ref={scrollRef}
        onScroll={onScroll}
      >
        {messages.length === 0 && historyPending ? (
          <div className="remote-history-loading" role="status" aria-busy="true" aria-label="Loading conversation history">
            <span className="loading-skeleton is-title" />
            <span className="loading-skeleton is-line" />
            <span className="loading-skeleton is-line is-medium" />
            <div>
              <span className="loading-skeleton is-line" />
              <span className="loading-skeleton is-line is-short" />
            </div>
          </div>
        ) : messages.length === 0 ? (
          <div className="remote-empty">
            <ProviderMark provider={providerIdentity} size={48} />
            <span>Ready</span>
            <h2>Start a {providerName} conversation</h2>
            <p className="remote-empty-hint">
              {terminalAvailable
                ? 'Send the first request below. Conversation and Terminal stay connected to the same provider session.'
                : 'Send the first request below. Rich sessions use the provider’s structured conversation interface.'}
            </p>
          </div>
        ) : null}
        {hiddenCount > 0 || hasEarlierClaudeEvents ? (
          <button
            type="button"
            className="remote-load-earlier"
            onClick={loadEarlier}
            disabled={loadingEarlierClaudeEvents}
            title={hiddenCount > 0
              ? `${hiddenCount} older messages hidden — click or scroll up to load`
              : 'Load the previous server-side history page'}
          >
            {hiddenCount > 0
              ? `↑ Load ${Math.min(hiddenCount, TAIL_WINDOW_STEP)} earlier ${hiddenCount === 1 ? 'message' : 'messages'} (${hiddenCount} hidden)`
              : (loadingEarlierClaudeEvents ? 'Loading earlier history…' : '↑ Load earlier history')}
          </button>
        ) : null}
        {forkMode ? (
          <div className="conversation-fork-mode-note" role="status">
            <span>Choose a message to branch from.</span>
            <small>The original conversation stays unchanged.</small>
          </div>
        ) : null}
        {visibleMessages.map((m, i) => (
          <RemoteMessage
            key={m.id}
            message={m}
            cwd={cwd}
            agentName={providerName}
            provider={providerIdentity}
            isLatest={i === visibleMessages.length - 1}
            showAgentHeader={m.role === 'assistant' && visibleMessages[i - 1]?.role !== 'assistant'}
            followedByToolActivity={Boolean(
              m.role === 'assistant'
              && m.content
              && visibleMessages[i + 1]?.role === 'assistant'
              && !visibleMessages[i + 1]?.content
              && visibleMessages[i + 1]?.toolCalls?.length
            )}
            onRetry={() => restoreDraft(m.id)}
            onDelete={() => remove(m.id)}
            forkOpen={forkPointId === m.id}
            forkBusy={forkBusy}
            forkError={forkPointId === m.id ? forkError : null}
            onToggleFork={forkMode && onForkFromMessage && m.status === 'sent' && !m.queued && Boolean(m.content)
              ? () => {
                setForkError(null);
                setForkPointId((current) => current === m.id ? null : m.id);
              }
              : undefined}
            onFork={onForkFromMessage
              ? (destinationProvider) => void forkVisibleMessage(m, destinationProvider)
              : undefined}
          />
        ))}
        {/* Sticky-anchor: pins the down-arrow to the right edge of the
            centered 820px message column (same pattern as Sessions). */}
        <div className="scroll-to-bottom-anchor" aria-hidden={atBottom}>
          <ScrollToBottomButton visible={!atBottom} onClick={scrollToBottom} />
        </div>
      </div>

      <StatusSidebar
        parserName={sidebar.parserName}
        parserIcon={sidebar.parserIcon}
        isWorking={sidebar.isWorking}
        timer={sidebar.timer}
        tokens={sidebar.tokens}
        context={sidebar.context}
        finalElapsed={sidebar.finalElapsed}
        currentTask={sidebar.currentTask}
        checklist={sidebar.checklist}
      />

      {blockingState ? (
        <div className="remote-blocking-banner" role="status" aria-live="polite">
          <span className="remote-blocking-banner-text">
            {blockingState.description} Open Terminal view to respond.
          </span>
          <button
            type="button"
            className="remote-blocking-banner-action"
            onClick={onOpenTerminal}
          >
            Terminal
          </button>
        </div>
      ) : null}

      <div className="remote-input-wrap">
        <InputBar
          send={sendConfirmed}
          submitMessage={submitMessage}
          connected={connected}
          sendAvailable={sendAvailable}
          sessionId={sessionId}
          onSubmitted={recordSent}
          recoverDraft={recoverDraft}
          provider={provider}
          model={model}
          effort={effort}
          modelControlSupported={modelControlSupported}
          providerWorking={sidebar.isWorking}
          onConfigureModel={onConfigureModel}
          richSession={!terminalAvailable}
          onRename={onRename}
          onContinueInTerminal={onContinueInTerminal}
        />
      </div>
    </div>
  );
}

interface RemoteMessageProps {
  message: ReturnType<typeof useDispatch>['messages'][number];
  cwd?: string;
  agentName: string;
  provider: ProviderIdentity;
  isLatest: boolean;
  showAgentHeader: boolean;
  followedByToolActivity: boolean;
  onRetry: () => void;
  onDelete: () => void;
  forkOpen: boolean;
  forkBusy: boolean;
  forkError: string | null;
  onToggleFork?: () => void;
  onFork?: (destinationProvider: 'claude' | 'codex') => void;
}

// Per-message render. Memoized — adapted-co + somewhere-tech both have
// hundreds of bubbles. Without memo, the entire list re-runs through
// markdown + ANSI + linkify on every messages-array reference change
// (which is every claudeEvent batch flush). With memo, only the bubble
// whose props actually changed renders.
//
// The onRetry/onDelete callbacks are pre-bound at the parent, so they
// reference identity changes on every parent render. We don't memoize
// them because the per-message overhead of useCallback would be a wash;
// instead the memo compare below ignores them — they're invoked, never
// compared.
function RemoteMessageInner({
  message: m,
  cwd,
  agentName,
  provider,
  isLatest,
  showAgentHeader,
  followedByToolActivity,
  onRetry,
  onDelete,
  forkOpen,
  forkBusy,
  forkError,
  onToggleFork,
  onFork
}: RemoteMessageProps): JSX.Element {
  const isUser = m.role === 'user';
  const toolOnly = !isUser && !m.content && Boolean(m.toolCalls?.length);
  const cls = `remote-msg remote-msg-${m.role} is-${m.status}${isLatest ? ' is-latest' : ''}${m.interrupted ? ' is-interrupted' : ''}${m.queued ? ' is-queued' : ''}${!isUser && !showAgentHeader ? ' is-continuation' : ''}${toolOnly ? ' is-tool-only' : ''}${followedByToolActivity ? ' has-following-tool-activity' : ''}`;
  const timestamp = formatMessageTimestamp(m.createdAt);
  const timestampTitle = new Date(m.createdAt).toLocaleString();

  // CSS-level height ratchet for the latest bubble: the parser sometimes
  // reports a shorter snapshot mid-stream (1 line) before re-emitting
  // the full set (5 lines), causing the chat to bounce up and down.
  // We measure the rendered bubble height on every render and remember
  // the max — applied as `min-height` so the box never visibly shrinks
  // while this is still the latest message. When a new message arrives
  // (`isLatest` flips false), the lock releases and the bubble settles
  // to its actual content height (which by then has stabilized).
  const bubbleRef = useRef<HTMLDivElement | null>(null);
  const [minHeight, setMinHeight] = useState(0);
  useLayoutEffect(() => {
    if (!isLatest) {
      if (minHeight !== 0) setMinHeight(0);
      return;
    }
    const el = bubbleRef.current;
    if (!el) return;
    const h = el.offsetHeight;
    if (h > minHeight) setMinHeight(h);
  }, [isLatest, m.content, m.status, minHeight]);

  const lockStyle = isLatest && minHeight > 0 ? { minHeight: `${minHeight}px` } : undefined;

  return (
    <div className={cls}>
      {!isUser && showAgentHeader ? (
        <header className="remote-message-meta is-agent">
          <span><ProviderMark provider={provider} size={18} /><strong>{agentName}</strong></span>
          <time dateTime={new Date(m.createdAt).toISOString()} title={timestampTitle}>{timestamp}</time>
        </header>
      ) : null}
      {isUser ? (
        <>
          <div
            className="remote-bubble remote-bubble-user"
            ref={bubbleRef}
            style={lockStyle}
          >
            {m.queued ? (
              <div className="remote-bubble-badge remote-bubble-badge-queued" aria-label="queued">
                <span aria-hidden>⏳</span>
                <span>{agentName === 'Codex'
                  ? 'submitted after Codex’s next tool call'
                  : 'queued — Claude is finishing the previous turn'}</span>
              </div>
            ) : null}
            {m.interrupted ? (
              <div className="remote-bubble-badge remote-bubble-badge-interrupted" aria-label="interrupted">
                <span aria-hidden>⎋</span>
                <span>you interrupted Claude</span>
              </div>
            ) : (
              <div className="remote-bubble-content">{m.content}</div>
            )}
            {m.errorResponse ? (
              <div className="remote-bubble-error">
                <span className="remote-bubble-error-icon" aria-hidden>⚠</span>
                <span>{m.errorResponse}</span>
              </div>
            ) : null}
            {m.status === 'failed' ? (
              <div className="remote-bubble-status remote-bubble-failed">
                <span>{m.failureReason ? `not delivered: ${m.failureReason}` : 'not delivered'}</span>
                <button type="button" className="remote-bubble-retry" onClick={onRetry}>restore draft</button>
                <button
                  type="button"
                  className="remote-bubble-delete"
                  onClick={onDelete}
                  title="Remove this entry from your local log. If Claude actually received the message, it'll reappear as a delivered entry on the next refresh."
                >delete</button>
              </div>
            ) : null}
            {m.status !== 'failed' ? (
              <button
                type="button"
                className="remote-bubble-quickdelete"
                onClick={onDelete}
                title="Remove this entry from your local log."
                aria-label="Delete message"
              >×</button>
            ) : null}
          </div>
          <footer className="remote-message-meta is-user">
            {m.status === 'failed' ? <span>Needs attention</span> : null}
            {m.author ? <span className="remote-message-author">{m.author.name} · via Sessions</span> : null}
            <time dateTime={new Date(m.createdAt).toISOString()} title={timestampTitle}>{timestamp}</time>
            <CopyButton getText={m.content} iconOnly label="Copy message" />
          </footer>
        </>
      ) : (
        <>
          <div
            className="remote-bubble remote-bubble-assistant"
            ref={bubbleRef}
            style={lockStyle}
          >
            {m.streaming ? (
              <div className="remote-bubble-live" role="status">
                <span className="remote-bubble-live-dot" aria-hidden />
                <span>Codex is working</span>
              </div>
            ) : null}
            {m.reasoningSummary ? (
              <details className="remote-bubble-disclosure remote-bubble-reasoning">
                <summary>Reasoning summary</summary>
                <div className="md-content" dangerouslySetInnerHTML={{ __html: renderContent(m.reasoningSummary, cwd) }} />
              </details>
            ) : null}
            {m.updates && m.updates.length > 0 ? (
              <details className="remote-bubble-disclosure remote-bubble-updates">
                <summary>{m.updates.length} progress {m.updates.length === 1 ? 'update' : 'updates'}</summary>
                <div className="remote-bubble-updates-list">
                  {m.updates.map((update, index) => (
                    <div
                      key={`${m.id}-update-${index}`}
                      className="md-content"
                      dangerouslySetInnerHTML={{ __html: renderContent(update, cwd) }}
                    />
                  ))}
                </div>
              </details>
            ) : null}
            {m.plan && m.plan.length > 0 ? (
              <PlanPanel steps={m.plan} explanation={m.planExplanation} />
            ) : null}
            {m.content ? (
              <div
                className="remote-bubble-content md-content"
                dangerouslySetInnerHTML={{ __html: renderContent(m.content, cwd) }}
              />
            ) : null}
            {m.toolCalls && m.toolCalls.length > 0 ? (
              <ToolCallsPanel calls={m.toolCalls} />
            ) : null}
            {m.errorResponse ? (
              <div className="remote-bubble-error">
                <span className="remote-bubble-error-icon" aria-hidden>⚠</span>
                <span>{m.errorResponse}</span>
              </div>
            ) : null}
            {!m.streaming && m.turnStatus && m.turnStatus !== 'completed' ? (
              <div className={`remote-turn-status is-${m.turnStatus}`}>{m.turnStatus}</div>
            ) : null}
          </div>
          {m.content ? (
            <footer className="remote-message-actions is-agent">
              <CopyButton getText={m.content} iconOnly label={`Copy ${agentName} response`} />
            </footer>
          ) : null}
        </>
      )}
      {onToggleFork ? (
        <div className={`remote-message-fork${isUser ? ' is-user' : ''}`}>
          <button type="button" className="remote-message-fork-trigger" disabled={forkBusy} onClick={onToggleFork}>
            Fork here
          </button>
          {forkOpen ? (
            <div className="remote-message-fork-picker">
              <span>Start an independent copy through this message. This conversation keeps running.</span>
              <button type="button" disabled={forkBusy} onClick={() => onFork?.(provider === 'claude' ? 'claude' : 'codex')}>
                {forkBusy ? 'Forking…' : `Fork in ${agentName}`}
              </button>
              <button type="button" disabled={forkBusy} onClick={() => onFork?.(provider === 'claude' ? 'codex' : 'claude')}>
                Open copy in {provider === 'claude' ? 'Codex' : 'Claude'}
              </button>
              {forkError ? <small role="alert">{forkError}</small> : null}
            </div>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

const RemoteMessage = memo(RemoteMessageInner, (a, b) => {
  // Custom equality: skip if everything visible is unchanged. Callbacks
  // are deliberately excluded from the compare — they change identity
  // on every parent render but always do the same thing.
  if (a.isLatest !== b.isLatest) return false;
  if (a.showAgentHeader !== b.showAgentHeader) return false;
  if (a.followedByToolActivity !== b.followedByToolActivity) return false;
  if (Boolean(a.onToggleFork) !== Boolean(b.onToggleFork)) return false;
  if (a.forkOpen !== b.forkOpen || a.forkBusy !== b.forkBusy || a.forkError !== b.forkError) return false;
  if (a.cwd !== b.cwd) return false;
  if (a.agentName !== b.agentName || a.provider !== b.provider) return false;
  const ma = a.message;
  const mb = b.message;
  // Reference equality on the same message object is the common case
  // (memoized by id in the parent, since events flow append-only).
  if (ma === mb) return true;
  return (
    ma.id === mb.id &&
    ma.createdAt === mb.createdAt &&
    ma.author === mb.author &&
    ma.role === mb.role &&
    ma.content === mb.content &&
    ma.status === mb.status &&
    ma.errorResponse === mb.errorResponse &&
    ma.failureReason === mb.failureReason &&
    ma.queued === mb.queued &&
    ma.interrupted === mb.interrupted &&
    ma.hadThinking === mb.hadThinking &&
    ma.reasoningSummary === mb.reasoningSummary &&
    ma.updates === mb.updates &&
    ma.plan === mb.plan &&
    ma.planExplanation === mb.planExplanation &&
    ma.streaming === mb.streaming &&
    ma.turnStatus === mb.turnStatus &&
    ma.toolCalls === mb.toolCalls
  );
});

function formatMessageTimestamp(value: number): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '';
  const now = new Date();
  const sameDay = date.getFullYear() === now.getFullYear()
    && date.getMonth() === now.getMonth()
    && date.getDate() === now.getDate();
  return date.toLocaleString(undefined, sameDay
    ? { hour: 'numeric', minute: '2-digit' }
    : { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
}

// Tool-calls panel: shows a compact, human-readable activity summary by
// default. Click to expand → the ordered list with input previews
// and a per-tool disclosure for the full output. Each chip is
// data-no-copy so clicking inside doesn't trigger the bubble's
// click-to-copy. Stops propagation so the bubble doesn't try to
// copy when the user is interacting with the panel.
function ToolCallsPanel({
  calls
}: {
  calls: import('../hooks/useDispatch').ToolCall[];
}): JSX.Element {
  const [expanded, setExpanded] = useState(false);
  const [openId, setOpenId] = useState<string | null>(null);
  const providerActivity = calls.some((call) => !!call.kind);
  const runningCount = calls.filter((call) => {
    const status = call.status?.toLowerCase() ?? '';
    return status === 'inprogress' || status === 'running' || status === 'pending';
  }).length;
  const summary = summarizeToolActivity(calls);
  return (
    <div
      className={`remote-bubble-tools${expanded ? ' is-expanded' : ''}`}
      data-no-copy
      onClick={(e) => e.stopPropagation()}
    >
      <button
        type="button"
        className="remote-bubble-tools-toggle"
        onClick={() => setExpanded((v) => !v)}
      >
        <span>{summary}</span>
        {providerActivity && runningCount > 0 ? <span className="remote-bubble-tools-summary">{runningCount} running</span> : null}
        <span className="remote-bubble-tools-caret" aria-hidden>{expanded ? '⌄' : '›'}</span>
      </button>
      {expanded ? (
        <div className="remote-bubble-tools-list">
          {calls.map((t) => {
            const isOpen = openId === t.id;
            const hasResult = !!t.resultFull;
            const normalizedStatus = t.status?.toLowerCase() ?? '';
            const showStatus = Boolean(normalizedStatus && !['completed', 'success'].includes(normalizedStatus));
            return (
              <div key={t.id} className={`remote-bubble-tool-row${isOpen ? ' is-open' : ''}`}>
                <button
                  type="button"
                  className="remote-bubble-tool"
                  onClick={() => setOpenId(isOpen ? null : t.id)}
                  title={`${t.name} · ${hasResult ? 'Open details' : 'No captured output'}`}
                >
                  <span className="remote-bubble-tool-input">{toolActivityLabel(t)}</span>
                  {showStatus ? (
                    <span className={`remote-bubble-tool-status is-${normalizedStatus}`}>
                      {t.status}
                    </span>
                  ) : null}
                  <span className="remote-bubble-tool-caret" aria-hidden>{isOpen ? '▾' : '▸'}</span>
                </button>
                {isOpen ? (
                  <div className="remote-bubble-tool-detail">
                    {t.inputFull && t.inputFull !== t.inputPreview ? (
                      <details className="remote-bubble-tool-section" open>
                        <summary>input</summary>
                        <pre>{t.inputFull}</pre>
                      </details>
                    ) : null}
                    {t.resultFull ? (
                      <details className="remote-bubble-tool-section" open>
                        <summary>output</summary>
                        <pre>{t.resultFull}</pre>
                      </details>
                    ) : (
                      <div className="remote-bubble-tool-empty">
                        {t.status && ['inprogress', 'running', 'pending'].includes(t.status.toLowerCase())
                          ? '(running…)'
                          : '(no output captured)'}
                      </div>
                    )}
                    {t.durationMs != null ? (
                      <div className="remote-bubble-tool-meta">{t.durationMs} ms</div>
                    ) : null}
                  </div>
                ) : null}
              </div>
            );
          })}
        </div>
      ) : null}
    </div>
  );
}

function isCommandTool(call: import('../hooks/useDispatch').ToolCall): boolean {
  return call.kind === 'commandExecution' || ['Bash', 'BashOutput', 'KillBash', 'Command'].includes(call.name);
}

function summarizeToolActivity(calls: import('../hooks/useDispatch').ToolCall[]): string {
  if (calls.length > 0 && calls.every(isCommandTool)) {
    return calls.length === 1 ? 'Ran a command' : `Ran ${calls.length} commands`;
  }
  if (calls.length === 1) return 'Used a tool';
  return `Used ${calls.length} tools`;
}

function toolActivityLabel(call: import('../hooks/useDispatch').ToolCall): string {
  const preview = call.inputPreview?.trim();
  if (isCommandTool(call)) return preview ? pastTenseLeadingVerb(preview) : 'Ran a command';
  if (!preview) return call.name;
  switch (call.name) {
    case 'Read': return `Read ${preview}`;
    case 'Write': return `Wrote ${preview}`;
    case 'Edit': return `Edited ${preview}`;
    case 'NotebookEdit': return `Edited ${preview}`;
    case 'Glob': return `Matched files for ${preview}`;
    case 'Grep': return `Searched for ${preview}`;
    case 'WebFetch': return `Fetched ${preview}`;
    case 'WebSearch': return `Searched the web for ${preview}`;
    default: return `${call.name} · ${preview}`;
  }
}

const TOOL_PAST_TENSE = new Map<string, string>([
  ['add', 'Added'], ['build', 'Built'], ['check', 'Checked'], ['copy', 'Copied'],
  ['create', 'Created'], ['delete', 'Deleted'], ['edit', 'Edited'], ['fetch', 'Fetched'],
  ['find', 'Found'], ['inspect', 'Inspected'], ['install', 'Installed'], ['list', 'Listed'],
  ['merge', 'Merged'], ['move', 'Moved'], ['read', 'Read'], ['regenerate', 'Regenerated'],
  ['remove', 'Removed'], ['resolve', 'Resolved'], ['run', 'Ran'], ['search', 'Searched'],
  ['see', 'Saw'], ['show', 'Showed'], ['test', 'Tested'], ['update', 'Updated'],
  ['verify', 'Verified'], ['write', 'Wrote']
]);

function pastTenseLeadingVerb(value: string): string {
  const match = value.match(/^([A-Za-z]+)(\b.*)$/);
  if (!match) return value;
  const replacement = TOOL_PAST_TENSE.get(match[1]!.toLowerCase());
  if (!replacement) return value;
  const rest = match[2]!.replace(/^ and ([A-Za-z]+)\b/i, (full, verb: string) => {
    const second = TOOL_PAST_TENSE.get(verb.toLowerCase());
    return second ? ` and ${second.toLowerCase()}` : full;
  });
  return `${replacement}${rest}`;
}
