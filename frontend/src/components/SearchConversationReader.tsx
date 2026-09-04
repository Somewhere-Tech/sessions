import { useEffect, useRef, useState, type ReactNode } from 'react';
import { fetchServerHistoryTranscript, type HistoryTranscript } from '../api/sessionsd';
import {
  compactMachineName,
  compactPath,
  isPromptHistoryOnly,
  managedSourceSessionID,
  plural,
  relativeDate
} from '../lib/conversationBrowser';
import type { ServerConfig } from '../lib/servers';
import { operationLabel } from '../lib/searchFormatting';
import { ProviderBadge, normalizeProvider } from './ProviderBadge';
import type { SelectedConversation } from './SearchView';

type ReaderMode = 'around' | 'after' | 'user' | 'full' | 'range';
const READER_PAGE_SIZE = 500;

export function ConversationReader({
  selected,
  server,
  onBack,
  onResumeConversation,
  continuing,
  continuationError
}: {
  selected: SelectedConversation;
  server: ServerConfig | undefined;
  onBack: () => void;
  onResumeConversation: (
    serverId: string,
    providerSessionId: string,
    sourceSessionId?: string,
    historyId?: string
  ) => Promise<void>;
  continuing: boolean;
  continuationError: string | null;
}): JSX.Element {
  const [readerMode, setReaderMode] = useState<ReaderMode>('around');
  const [rangeStart, setRangeStart] = useState<number | null>(null);
  const [rangeEnd, setRangeEnd] = useState<number | null>(null);
  const [readerTranscript, setReaderTranscript] = useState(selected.transcript);
  const [readerLoading, setReaderLoading] = useState(selected.loading);
  const [readerError, setReaderError] = useState(selected.error);
  const [readerNextIndex, setReaderNextIndex] = useState(selected.transcript?.next_index ?? 0);
  const [readerHasMore, setReaderHasMore] = useState(false);
  const [readerLimit, setReaderLimit] = useState<number | null>(null);
  const anchorRef = useRef<HTMLElement | null>(null);
  const transcriptScrollRef = useRef<HTMLDivElement | null>(null);
  const readerAbort = useRef<AbortController | null>(null);
  const provider = normalizeProvider(readerTranscript?.session.tool ?? selected.tool);
  // A rollup-opened session has no anchor. The reader still needs a number to
  // page from, but nothing may be labelled "Match" on the strength of it.
  const anchored = selected.anchor !== null;
  const anchor = selected.anchor ?? 0;

  const visibleMessages = readerTranscript?.messages ?? [];

  useEffect(() => {
    setReaderTranscript(selected.transcript);
    setReaderLoading(selected.loading);
    setReaderError(selected.error);
    setReaderNextIndex(selected.transcript?.next_index ?? 0);
    setReaderHasMore(false);
  }, [selected.transcript, selected.loading, selected.error]);

  useEffect(() => () => readerAbort.current?.abort(), []);

  useEffect(() => {
    if (!readerTranscript) return;
    window.requestAnimationFrame(() => anchorRef.current?.scrollIntoView({ block: 'center', behavior: 'smooth' }));
  }, [readerTranscript, readerMode]);

  const loadReaderMode = (next: ReaderMode): void => {
    if (!server) return;
    if (next === 'range' && (rangeStart === null || rangeEnd === null)) return;
    setReaderMode(next);
    readerAbort.current?.abort();
    const controller = new AbortController();
    readerAbort.current = controller;
    setReaderLoading(true);
    setReaderError(null);
    let window: { start?: number; end?: number; role?: 'user' } | undefined;
    let limit: number | null = null;
    if (next === 'around') window = { start: Math.max(0, anchor - 2), end: anchor + 11 };
    if (next === 'after') window = { start: anchor, end: anchor + READER_PAGE_SIZE };
    if (next === 'user') window = { start: 0, end: READER_PAGE_SIZE, role: 'user' };
    if (next === 'full') window = undefined;
    if (next === 'range') {
      limit = Math.max(rangeStart as number, rangeEnd as number) + 1;
      const start = Math.min(rangeStart as number, rangeEnd as number);
      window = {
        start,
        end: Math.min(limit, start + READER_PAGE_SIZE)
      };
    }
    setReaderLimit(limit);
    void fetchServerHistoryTranscript(server, selected.sessionId, controller.signal, window)
      .then((transcript) => {
        if (!controller.signal.aborted) {
          const normalized = normalizeTranscriptIndexes(transcript);
          setReaderTranscript(normalized);
          const nextIndex = normalized.next_index ?? normalized.messages.length;
          setReaderNextIndex(nextIndex);
          setReaderHasMore(
            next !== 'around'
            && next !== 'full'
            && Boolean(normalized.has_more)
            && (limit === null || nextIndex < limit)
          );
          setReaderLoading(false);
        }
      })
      .catch((reason: unknown) => {
        if (!controller.signal.aborted) {
          setReaderError(reason instanceof Error ? reason.message : 'Could not load the transcript view');
          setReaderLoading(false);
        }
      });
  };

  const loadMore = (): void => {
    if (!server || !readerHasMore || readerLoading) return;
    const end = Math.min(readerNextIndex + READER_PAGE_SIZE, readerLimit ?? Number.MAX_SAFE_INTEGER);
    const controller = new AbortController();
    readerAbort.current?.abort();
    readerAbort.current = controller;
    setReaderLoading(true);
    setReaderError(null);
    void fetchServerHistoryTranscript(server, selected.sessionId, controller.signal, {
      start: readerNextIndex,
      end,
      role: readerMode === 'user' ? 'user' : undefined
    }).then((transcript) => {
      if (controller.signal.aborted) return;
      const normalized = normalizeTranscriptIndexes(transcript);
      setReaderTranscript((current) => current ? {
        ...normalized,
        messages: [...current.messages, ...normalized.messages]
      } : normalized);
      const nextIndex = normalized.next_index ?? end;
      setReaderNextIndex(nextIndex);
      setReaderHasMore(Boolean(normalized.has_more) && (readerLimit === null || nextIndex < readerLimit));
      setReaderLoading(false);
    }).catch((reason: unknown) => {
      if (!controller.signal.aborted) {
        setReaderError(reason instanceof Error ? reason.message : 'Could not load more of the transcript');
        setReaderLoading(false);
      }
    });
  };

  const chooseRangeStart = (index: number): void => {
    setRangeStart(index);
  };
  const chooseRangeEnd = (index: number): void => {
    setRangeEnd(index);
  };
  const jumpToLatest = (): void => {
    const scroll = transcriptScrollRef.current;
    if (!scroll) return;
    scroll.scrollTo({ top: scroll.scrollHeight, behavior: 'smooth' });
  };

  const providerSessionID = readerTranscript?.session.provider_session_id
    ?? selected.providerSessionId;
  const historyID = readerTranscript?.session.id ?? selected.sessionId;
  const promptHistoryOnly = isPromptHistoryOnly(selected.sessionId);
  const canResume = Boolean(historyID && provider);

  return (
    <div className="search-view search-conversation-view">
      <div className="search-shell search-reader-shell">
        <div className="search-reader-chrome">
          <button type="button" className="search-back" onClick={onBack}>← Back to results</button>
          <header className="search-conversation-heading">
            <div>
              <span className="search-conversation-kicker">
                {promptHistoryOnly ? 'Claude prompt history only' : 'Read-only transcript'}{selected.matchCount === null ? '' : ` · ${plural(selected.matchCount, 'message match', 'message matches')}`} · {anchored ? `opened at message ${anchor + 1}` : 'opened from the start'}
              </span>
              <h1>{readerTranscript?.session.name || selected.title || selected.sessionId.slice(0, 8)}</h1>
              <p>
                {provider ? <ProviderBadge provider={provider} /> : null}
                <span>{compactMachineName(server?.name ?? selected.serverName)}</span>
                {readerTranscript?.session.cwd ? <code>{compactPath(readerTranscript.session.cwd)}</code> : null}
              </p>
            </div>
            <div className="search-conversation-actions">
              <span>{promptHistoryOnly
                ? 'Sessions found the requests you sent Claude, but not the full conversation. You can reopen it only if Claude still has it.'
                : 'Viewing does not change this conversation. Continue lets you choose the agent, model, and amount of history before anything is sent.'}</span>
              {canResume ? (
                <button
                  type="button"
                  className="btn btn-primary"
                  disabled={continuing}
                  onClick={() => { void onResumeConversation(
                    selected.serverId,
                    providerSessionID || historyID,
                    managedSourceSessionID(selected.sessionId),
                    historyID
                  ); }}
                >
                  {continuing ? 'Opening details…' : 'Continue conversation…'}
                </button>
              ) : null}
            </div>
          </header>

          <div className="search-reader-toolbar">
            <div className="usage-segmented" role="tablist" aria-label="Transcript view">
              <ReaderButton active={readerMode === 'around'} onClick={() => loadReaderMode('around')}>Around match</ReaderButton>
              <ReaderButton active={readerMode === 'after'} onClick={() => loadReaderMode('after')}>Everything after</ReaderButton>
              <ReaderButton active={readerMode === 'user'} onClick={() => loadReaderMode('user')}>What you said</ReaderButton>
              <ReaderButton active={readerMode === 'full'} onClick={() => loadReaderMode('full')}>Full transcript</ReaderButton>
              <ReaderButton active={readerMode === 'range'} disabled={rangeStart === null || rangeEnd === null} onClick={() => loadReaderMode('range')}>
                Selected range
              </ReaderButton>
            </div>
            <div className="search-reader-position">
              <span>
                {readerMode === 'range' || rangeStart !== null || rangeEnd !== null
                  ? `${rangeStart === null ? 'Choose a start' : `Start ${rangeStart + 1}`} · ${rangeEnd === null ? 'choose an end' : `End ${rangeEnd + 1}`}`
                  : `${visibleMessages.length}${readerTranscript?.session.message_count ? ` of ${readerTranscript.session.message_count}` : ''} messages`}
              </span>
              {readerMode === 'full' && visibleMessages.length > 0 ? (
                <button type="button" onClick={jumpToLatest}>Jump to latest ↓</button>
              ) : null}
            </div>
          </div>
        </div>

        {continuationError ? <div className="search-errors">{continuationError}</div> : null}
        {readerError ? <div className="search-errors">{readerError}</div> : null}
        {readerLoading ? <div className="usage-empty">Loading this transcript view…</div> : null}
        {readerTranscript && !readerLoading ? (
          <div className="search-transcript" ref={transcriptScrollRef}>
            {visibleMessages.map((message) => {
              const isAnchor = anchored && message.index === anchor;
              return (
                <article
                  ref={isAnchor ? (node) => { anchorRef.current = node; } : undefined}
                  className={`search-transcript-message is-${message.role}${isAnchor ? ' is-match' : ''}`}
                  key={message.index}
                >
                  <header>
                    <span>
                      {isAnchor ? <span className="search-match-marker">Match</span> : null}
                      {message.role === 'user'
                        ? <span className="search-role is-user">{message.author ? `${message.author.name} · via Sessions` : 'You said'}</span>
                        : message.role === 'tool'
                          ? <span className="search-role is-tool">{operationLabel(message.kind)}</span>
                          : provider ? <ProviderBadge provider={provider} compact /> : <span className="search-role">Agent</span>}
                      <span className="search-message-index">#{message.index + 1}</span>
                    </span>
                    <span>
                      {message.role === 'user' ? (
                        <>
                          <button type="button" onClick={() => chooseRangeStart(message.index)}>Start range</button>
                          <button type="button" onClick={() => chooseRangeEnd(message.index)}>End range</button>
                        </>
                      ) : null}
                      <time>{message.timestamp ? relativeDate(message.timestamp) : ''}</time>
                    </span>
                  </header>
                  <p>{message.text}</p>
                </article>
              );
            })}
            {visibleMessages.length === 0 ? <div className="usage-empty">No messages in this view.</div> : null}
            {readerHasMore ? (
              <button type="button" className="search-load-more" disabled={readerLoading} onClick={loadMore}>
                {readerLoading ? 'Loading…' : `Load the next ${READER_PAGE_SIZE} message positions`}
              </button>
            ) : null}
            <div className="search-transcript-latest" aria-hidden />
          </div>
        ) : null}
      </div>
    </div>
  );
}

function ReaderButton({
  active,
  disabled = false,
  onClick,
  children
}: {
  active: boolean;
  disabled?: boolean;
  onClick: () => void;
  children: ReactNode;
}): JSX.Element {
  return <button type="button" disabled={disabled} className={active ? 'is-active' : ''} onClick={onClick}>{children}</button>;
}

export function normalizeTranscriptIndexes(transcript: HistoryTranscript): HistoryTranscript {
  return {
    ...transcript,
    messages: transcript.messages.map((message, index) => ({
      ...message,
      index: Number.isFinite(message.index) ? message.index : index,
      id: message.id || `legacy:${Number.isFinite(message.index) ? message.index : index}`
    }))
  };
}
