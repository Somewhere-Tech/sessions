import { useCallback, useEffect, useRef, useState } from 'react';
import { fetchServerHistoryTranscript, type HistoryTranscript } from '../api/sessionsd';
import { getActiveServer, serverDisplayName, useServers } from '../lib/servers';
import { resolvedSessionLabel } from '../lib/tabLabels';
import { useSessions } from '../store/sessions';
import type { SessionInfo } from '../types';
import { ProviderBadge, normalizeProvider } from './ProviderBadge';
import { SessionDetails } from './SessionDetails';
import { canContinueSession, continuationSession, endedAtLabel, endedSummary } from '../lib/sessionStatus';
import { sessionMode, sessionModeName, sessionModeShort } from '../lib/sessionMode';
import { SessionPopOutButton } from './SessionPopOutButton';
import { ContinueElsewhereButton } from './ContinueElsewhereButton';
import { ConversationForkButton } from './ConversationForkButton';

const INITIAL_PREVIEW_MESSAGES = 60;
const MAX_PREVIEW_MESSAGES = 400;

interface Props {
  session: SessionInfo;
  onResume?: (session: SessionInfo) => void | Promise<void>;
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
  const label = resolvedSessionLabel(session);
  const supportsConversation = session.tool !== 'terminal';
  const [detailsOpen, setDetailsOpen] = useState(!supportsConversation);
  const [transcript, setTranscript] = useState<HistoryTranscript | null>(null);
  const [loading, setLoading] = useState(supportsConversation);
  const [error, setError] = useState<string | null>(null);
  const [archiveBusy, setArchiveBusy] = useState(false);
  const [archiveError, setArchiveError] = useState<string | null>(null);
  const [resumeBusy, setResumeBusy] = useState(false);
  const [resumeError, setResumeError] = useState<string | null>(null);
  const [previewMessages, setPreviewMessages] = useState(INITIAL_PREVIEW_MESSAGES);
  const [showJumpToLatest, setShowJumpToLatest] = useState(false);
  const [forkPoint, setForkPoint] = useState<number | null>(null);
  const [forkMode, setForkMode] = useState(false);
  const [forkBusy, setForkBusy] = useState(false);
  const [forkError, setForkError] = useState<string | null>(null);
  const historyBodyRef = useRef<HTMLDivElement>(null);
  const previousHistoryHeight = useRef(0);
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
    if (previewMessages === INITIAL_PREVIEW_MESSAGES) setTranscript(null);
    void fetchServerHistoryTranscript(getActiveServer(), session.id, controller.signal, {
      preview: true,
      previewLimit: previewMessages
    })
      .then((value) => {
        if (!controller.signal.aborted) {
          // Older compatible daemons ignore the additive `limit` query and
          // return their full 400-message preview. Bound it again in the
          // client so a newer viewer stays responsive during version skew.
          setTranscript(value.messages.length > previewMessages
            ? { ...value, messages: value.messages.slice(-previewMessages), truncated: true }
            : value);
          if (previousHistoryHeight.current > 0) {
            window.requestAnimationFrame(() => {
              const element = historyBodyRef.current;
              if (element) element.scrollTop += element.scrollHeight - previousHistoryHeight.current;
              previousHistoryHeight.current = 0;
            });
          }
        }
      })
      .catch((reason: unknown) => {
        if (!controller.signal.aborted) setError(reason instanceof Error ? reason.message : 'Could not load the conversation.');
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [activeServerId, session.id, supportsConversation, previewMessages]);

  const resumeExactConversation = async (): Promise<void> => {
    if (!onResume || resumeBusy) return;
    setResumeBusy(true);
    setResumeError(null);
    try {
      await onResume(session);
    } catch (reason) {
      setResumeError(reason instanceof Error ? reason.message : 'Could not resume this conversation.');
    } finally {
      setResumeBusy(false);
    }
  };

  const showEarlierMessages = (): void => {
    const element = historyBodyRef.current;
    previousHistoryHeight.current = element?.scrollHeight ?? 0;
    setPreviewMessages((current) => Math.min(MAX_PREVIEW_MESSAGES, current + INITIAL_PREVIEW_MESSAGES));
  };

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
      setForkMode(false);
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
          <span className="session-parent-breadcrumb">{parent ? `${resolvedSessionLabel(parent)} / ${session.displayParentSessionId !== undefined ? 'grouped session' : 'child session'}` : 'Manager session'} · saved history</span>
          <div className="session-active-title-row"><h1>{label}</h1><span className={`session-live-pill ${continuationIsLive ? 'is-completed' : 'is-finished'}`}>{lifecycleLabel}</span><span className={`session-runtime-badge${sessionMode(session) === 'terminal' && session.tool !== 'claude-code' ? ' is-terminal' : ''}`} title={sessionModeName(session)}>{sessionModeShort(session)}</span></div>
          <div className="session-active-meta">
            {provider ? <ProviderBadge provider={provider} compact /> : <span className="provider-badge is-shell is-compact">⌘ Shell</span>}
            <span>{session.profile || 'Default profile'}</span><span>Saved on {serverDisplayName(getActiveServer(), true)}</span><span title={session.cwd}>{session.cwd}</span>
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
        {onFork && provider ? (
          <ConversationForkButton
            active={forkMode}
            onClick={() => {
              setForkError(null);
              setForkPoint(null);
              const next = !forkMode;
              setForkMode(next);
              if (next) setDetailsOpen(false);
            }}
          />
        ) : null}
        <button
          type="button"
          className={`details-inspector-button${detailsOpen ? ' is-active' : ''}`}
          onClick={() => {
            if (!detailsOpen) setForkMode(false);
            setDetailsOpen((current) => !current);
          }}
        >
          {detailsOpen ? 'Close details' : 'Details'}
        </button>
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
                  <button type="button" className="btn btn-primary" disabled={resumeBusy} onClick={() => void resumeExactConversation()}>{resumeBusy ? 'Resuming…' : 'Resume conversation'} {!resumeBusy ? <span aria-hidden>→</span> : null}</button>
                ) : hasContinuation ? (
                  <span>The continuation is recorded on another machine.</span>
                ) : (
                  <span>{supportsConversation
                    ? 'Sessions could not find a readable conversation history for this runtime.'
                    : 'Shell sessions do not have an agent conversation to resume.'}</span>
                )}
                {canContinueSession(session) ? <ContinueElsewhereButton sessionId={session.id} label={label} /> : null}
                <button type="button" className="btn btn-secondary" disabled={archiveBusy} onClick={() => void archiveFromInbox()}>{archiveBusy ? 'Archiving…' : 'Archive from list'}</button>
              </div>
            </div>
            {archiveError ? <div className="session-history-action-error" role="alert">{archiveError}</div> : null}
            {resumeError ? <div className="session-history-action-error" role="alert">{resumeError}</div> : null}
            {forkMode ? (
              <div className="conversation-fork-mode-note" role="status">
                <span>Choose a message to branch from.</span>
                <small>The original conversation stays unchanged.</small>
              </div>
            ) : null}
            {loading ? <div className="usage-empty">Loading the conversation…</div> : null}
            {error ? <div className="search-errors">{error}</div> : null}
            {transcript?.truncated ? (
              <div className="search-preview-notice session-history-preview-control">
                <span>Showing the latest {transcript.messages.length} messages so this conversation opens quickly.</span>
                {previewMessages < MAX_PREVIEW_MESSAGES ? (
                  <button type="button" className="btn btn-ghost" disabled={loading} onClick={showEarlierMessages}>{loading ? 'Loading…' : 'Show earlier messages'}</button>
                ) : <span>Use Search to find anything earlier.</span>}
              </div>
            ) : null}
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
                {forkMode && onFork && provider ? (
                  <div className="session-history-message-actions">
                    <button
                      type="button"
                      className="remote-message-fork-trigger"
                      disabled={forkBusy}
                      onClick={() => {
                        setForkError(null);
                        setForkPoint((current) => current === message.index ? null : message.index);
                      }}
                    >
                      Fork here
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
