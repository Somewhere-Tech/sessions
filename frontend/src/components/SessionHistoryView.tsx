import { useCallback, useEffect, useRef, useState } from 'react';
import { fetchServerHistoryTranscript, type HistoryTranscript } from '../api/sessionsd';
import { getActiveServer, useServers } from '../lib/servers';
import { sessionLabel, useTabLabel } from '../lib/tabLabels';
import { useSessions } from '../store/sessions';
import type { SessionInfo } from '../types';
import { ProviderBadge, normalizeProvider } from './ProviderBadge';
import { SessionDetails } from './SessionDetails';
import { canContinueSession, continuationSession, endedAtLabel, endedSummary } from '../lib/sessionStatus';
import { sessionMode, sessionModeName, sessionModeShort } from '../lib/sessionMode';
import { SessionPopOutButton } from './SessionPopOutButton';
import { ContinueElsewhereButton } from './ContinueElsewhereButton';

interface Props {
  session: SessionInfo;
  onResume?: (session: SessionInfo) => void;
  onFork?: (
    session: SessionInfo,
    destinationProvider: 'claude' | 'codex',
    point: { index: number; messageId: string }
  ) => Promise<void>;
  onCloseView?: (sessionId: string) => void;
  onOpenSession?: (sessionId: string) => void;
  onBack?: () => void;
}

export function SessionHistoryView({ session, onResume, onFork, onCloseView, onOpenSession, onBack }: Props): JSX.Element {
  const activeServerId = useServers((state) => state.activeId);
  const allSessions = useSessions((state) => state.sessions);
  const archiveSessions = useSessions((state) => state.archive);
  const label = useTabLabel(session.id, session.cwd) ?? sessionLabel(session);
  const supportsConversation = session.tool !== 'terminal';
  const [detailsOpen, setDetailsOpen] = useState(!supportsConversation);
  const [transcript, setTranscript] = useState<HistoryTranscript | null>(null);
  const [loading, setLoading] = useState(supportsConversation);
  const [error, setError] = useState<string | null>(null);
  const [archiveBusy, setArchiveBusy] = useState(false);
  const [archiveError, setArchiveError] = useState<string | null>(null);
  const [showJumpToLatest, setShowJumpToLatest] = useState(false);
  const [forkPoint, setForkPoint] = useState<number | null>(null);
  const [forkBusy, setForkBusy] = useState(false);
  const [forkError, setForkError] = useState<string | null>(null);
  const historyBodyRef = useRef<HTMLDivElement>(null);
  const displayParentID = session.displayParentSessionId !== undefined
    ? session.displayParentSessionId
    : session.parentSessionId;
  const parent = displayParentID ? allSessions.find((item) => item.id === displayParentID) : null;
  const provider = normalizeProvider(session.tool);
  const end = endedSummary(session, allSessions);
  const continuation = continuationSession(session, allSessions);
  const hasContinuation = Boolean(continuation || session.reopenedAs || session.movedToSessionId);
  const continuationIsLive = Boolean(continuation && !continuation.exited);
  const lifecycleLabel = continuationIsLive ? 'Continued · live' : hasContinuation ? 'Continued' : 'Ended';
  const endInitiator = session.endedByKind === 'session' && session.endedById
    ? allSessions.find((item) => item.id === session.endedById)
    : null;
  const archiveFromInbox = async (): Promise<void> => {
    if (archiveBusy) return;
    setArchiveBusy(true);
    setArchiveError(null);
    try {
      const result = await archiveSessions([session.id]);
      const item = result.items[0];
      if (item?.status === 'skipped') {
        setArchiveError(item.reason ?? 'This session could not be archived.');
        return;
      }
      onCloseView?.(session.id);
    } catch (reason) {
      setArchiveError(reason instanceof Error ? reason.message : 'This session could not be archived.');
    } finally {
      setArchiveBusy(false);
    }
  };

  useEffect(() => {
    if (!supportsConversation) return;
    const controller = new AbortController();
    setLoading(true);
    setError(null);
    setTranscript(null);
    void fetchServerHistoryTranscript(getActiveServer(), session.id, controller.signal, { preview: true })
      .then((value) => {
        if (!controller.signal.aborted) setTranscript(value);
      })
      .catch((reason: unknown) => {
        if (!controller.signal.aborted) setError(reason instanceof Error ? reason.message : 'Could not load the conversation.');
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [activeServerId, session.id, supportsConversation]);

  const updateJumpToLatest = useCallback(() => {
    const element = historyBodyRef.current;
    if (!element) return;
    setShowJumpToLatest(element.scrollHeight - element.scrollTop - element.clientHeight > 120);
  }, []);

  useEffect(() => {
    const frame = window.requestAnimationFrame(updateJumpToLatest);
    return () => window.cancelAnimationFrame(frame);
  }, [detailsOpen, transcript, updateJumpToLatest]);

  const jumpToLatest = (): void => {
    const element = historyBodyRef.current;
    if (!element) return;
    element.scrollTo({ top: element.scrollHeight, behavior: 'smooth' });
  };
  const forkFromMessage = async (
    message: NonNullable<typeof transcript>['messages'][number],
    destinationProvider: 'claude' | 'codex'
  ): Promise<void> => {
    if (!onFork || forkBusy) return;
    setForkBusy(true);
    setForkError(null);
    try {
      await onFork(session, destinationProvider, { index: message.index, messageId: message.id });
      setForkPoint(null);
    } catch (reason) {
      setForkError(reason instanceof Error ? reason.message : 'Could not fork this conversation.');
    } finally {
      setForkBusy(false);
    }
  };

  return (
    <div className={`session-view view-history${detailsOpen ? ' view-details' : ''}`}>
      <header className="session-active-header">
        {onBack ? <button type="button" className="mobile-session-back" onClick={onBack} aria-label="Back to sessions">‹</button> : null}
        <div className="session-active-copy">
          <span className="session-parent-breadcrumb">{parent ? `${sessionLabel(parent)} / ${session.displayParentSessionId !== undefined ? 'grouped session' : 'child session'}` : 'Manager session'} · saved history</span>
          <div className="session-active-title-row"><h1>{label}</h1><span className={`session-live-pill ${continuationIsLive ? 'is-completed' : 'is-finished'}`}>{lifecycleLabel}</span><span className={`session-runtime-badge${sessionMode(session) === 'terminal' && session.tool !== 'claude-code' ? ' is-terminal' : ''}`} title={sessionModeName(session)}>{sessionModeShort(session)}</span></div>
          <div className="session-active-meta">
            {provider ? <ProviderBadge provider={provider} compact /> : <span className="provider-badge is-shell is-compact">⌘ Shell</span>}
            <span>{session.profile || 'Default profile'}</span><span>Saved on {getActiveServer().name}</span><span title={session.cwd}>{session.cwd}</span>
          </div>
        </div>
        <div className="session-active-actions">
          <SessionPopOutButton sessionId={session.id} label={label} />
          {onCloseView ? <button type="button" className="btn btn-ghost session-close-view" onClick={() => onCloseView(session.id)} title="Close this tab. The saved conversation remains available."><span aria-hidden>×</span> Close tab</button> : null}
        </div>
      </header>
      <div className="session-toolbar">
        {supportsConversation ? <div className="view-toggle is-content-switch"><button type="button" className="view-toggle-btn is-active">Conversation</button></div> : <span className="history-shell-label">Shell session</span>}
        <span className="status-text">{hasContinuation ? `Original runtime ended ${endedAtLabel(session)} · ${continuationIsLive ? 'live continuation' : 'continued elsewhere'}` : `Ended ${endedAtLabel(session)} · read-only history`}</span>
        <button type="button" className={`details-inspector-button${detailsOpen ? ' is-active' : ''}`} onClick={() => setDetailsOpen((current) => !current)}>{detailsOpen ? 'Close details' : 'Details'}</button>
      </div>
      <div ref={historyBodyRef} className="session-history-body" onScroll={updateJumpToLatest}>
        {detailsOpen ? (
          <SessionDetails session={session} allSessions={allSessions} onEnd={async () => undefined} onResume={onResume} />
        ) : (
          <div className="session-history-transcript">
            <div className={`session-ended-summary is-${end.tone}`}>
              <div>
                {endInitiator && onOpenSession ? (
                  <button type="button" className="session-ended-actor" onClick={() => onOpenSession(endInitiator.id)}>{end.label}</button>
                ) : <strong>{end.label}</strong>}
                <span>{endedAtLabel(session)}</span>
              </div>
              <p>{end.detail}</p>
              <p className="session-ended-read-only">{continuationIsLive ? 'You are viewing the original runtime. Open the live continuation to send a message.' : 'Viewing does not resume or send anything.'}</p>
              <div className="session-ended-actions">
                {continuation && onOpenSession ? (
                  <button type="button" className="btn btn-primary" onClick={() => onOpenSession(continuation.id)}>Open {continuationIsLive ? 'live ' : ''}continuation <span aria-hidden>→</span></button>
                ) : canContinueSession(session) ? (
                  <button type="button" className="btn btn-primary" onClick={() => onResume?.(session)}>Continue conversation <span aria-hidden>→</span></button>
                ) : hasContinuation ? (
                  <span>The continuation is recorded on another machine.</span>
                ) : (
                  <span>{supportsConversation
                    ? 'This runtime ended before Sessions recorded an agent conversation ID.'
                    : 'Shell sessions do not have an agent conversation to resume.'}</span>
                )}
                {canContinueSession(session) ? <ContinueElsewhereButton sessionId={session.id} label={label} /> : null}
                <button type="button" className="btn btn-secondary" disabled={archiveBusy} onClick={() => void archiveFromInbox()}>{archiveBusy ? 'Archiving…' : 'Archive from list'}</button>
              </div>
            </div>
            {archiveError ? <div className="session-history-action-error" role="alert">{archiveError}</div> : null}
            {loading ? <div className="usage-empty">Loading the conversation…</div> : null}
            {error ? <div className="search-errors">{error}</div> : null}
            {transcript?.truncated ? <div className="search-preview-notice">Showing {transcript.messages.length} recent messages from a bounded preview (up to 400).</div> : null}
            {transcript?.messages.map((message, index) => {
              const continuation = message.role === 'assistant' && transcript.messages[index - 1]?.role === 'assistant';
              return (
              <article className={`session-history-message is-${message.role}${continuation ? ' is-continuation' : ''}`} key={`${message.timestamp ?? 'none'}:${index}`}>
                {message.role === 'assistant' && !continuation ? (
                  <header>
                    {provider ? <ProviderBadge provider={provider} compact /> : <span>Agent</span>}
                    <time>{message.timestamp ? formatDate(message.timestamp) : ''}</time>
                  </header>
                ) : null}
                <p>{message.text}</p>
                {message.role === 'user' ? (
                  <footer>
                    <span>{message.author ? `${message.author.name} · via Sessions` : 'You'}</span>
                    <time>{message.timestamp ? formatDate(message.timestamp) : ''}</time>
                  </footer>
                ) : null}
                {onFork && provider ? (
                  <div className="session-history-message-actions">
                    <button
                      type="button"
                      className="btn btn-ghost"
                      disabled={forkBusy}
                      onClick={() => {
                        setForkError(null);
                        setForkPoint((current) => current === message.index ? null : message.index);
                      }}
                    >
                      Fork from here…
                    </button>
                    {forkPoint === message.index ? (
                      <div className="session-history-fork-picker">
                        <span>Start an independent copy through this message. The original stays unchanged.</span>
                        <button type="button" className="btn btn-secondary" disabled={forkBusy} onClick={() => void forkFromMessage(message, provider)}>
                          {forkBusy ? 'Forking…' : `Fork in ${provider === 'claude' ? 'Claude' : 'Codex'}`}
                        </button>
                        <button type="button" className="btn btn-secondary" disabled={forkBusy} onClick={() => void forkFromMessage(message, provider === 'claude' ? 'codex' : 'claude')}>
                          {`Open copy in ${provider === 'claude' ? 'Codex' : 'Claude'}`}
                        </button>
                      </div>
                    ) : null}
                  </div>
                ) : null}
              </article>
              );
            })}
            {forkError ? <div className="session-history-action-error" role="alert">{forkError}</div> : null}
            {!loading && !error && transcript?.messages.length === 0 ? <div className="usage-empty">This session has no normalized conversation messages.</div> : null}
          </div>
        )}
        {!detailsOpen && showJumpToLatest ? (
          <div className="session-history-jump-anchor">
            <button type="button" className="scroll-to-bottom" onClick={jumpToLatest} aria-label="Jump to latest message" title="Jump to latest message">
              <span aria-hidden>↓</span>
            </button>
          </div>
        ) : null}
      </div>
    </div>
  );
}

function formatDate(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
}
