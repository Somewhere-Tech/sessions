import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import {
  fetchServerHistory,
  fetchServerHistoryTranscript,
  fetchServerResumableSessions,
  listServerSessions,
  planSmartSearch,
  searchServer,
  type HistoryMessage,
  type HistoryTranscript,
  type ResumableSession,
  type SearchMatch,
  type SmartSearchPlan
} from '../api/sessionsd';
import {
  enrichSearchResultsWithHistory,
  enrichSearchResultsWithResumable,
  enrichSearchResultsWithSessions,
  groupSearchResults,
  type FleetSearchResult,
  type SearchConversationGroup
} from '../lib/searchConversations';
import { serverDisplayName, useServers, type ServerConfig } from '../lib/servers';
import { isTauri } from '../lib/tauriBridge';
import { ProviderBadge, normalizeProvider, type Provider } from './ProviderBadge';
import { ParserIcon } from './ParserIcon';
import type { SessionInfo } from '../types';

type SearchMode = 'ai' | 'ranked';
type Speaker = 'user' | '' | 'assistant' | 'tool';
type Tool = '' | Provider;
type SortMode = 'relevance' | 'timeline';
type DateRange = 'all' | 'today' | '7d' | '30d';
type ReaderMode = 'around' | 'after' | 'user' | 'full' | 'range';
const READER_PAGE_SIZE = 500;

type Result = FleetSearchResult;
interface SelectedConversation {
  result: Result;
  group: SearchConversationGroup;
  transcript: HistoryTranscript | null;
  loading: boolean;
  error: string | null;
}

interface SavedSearchState {
  query: string;
  mode: SearchMode;
  speaker: Speaker;
  tool: Tool;
  sort: SortMode;
  dateRange: DateRange;
  sessionName: string;
  cwd: string;
}

const SEARCH_STATE_KEY = 'sessions:search-state:v3';

function readSearchState(): SavedSearchState {
  try {
    const value = JSON.parse(window.localStorage.getItem(SEARCH_STATE_KEY) ?? '{}') as Partial<SavedSearchState>;
    const storedMode = typeof value.mode === 'string' ? value.mode : '';
    return {
      query: typeof value.query === 'string' ? value.query : '',
      mode: storedMode === 'ai' || storedMode === 'ranked' ? storedMode : storedMode ? 'ranked' : 'ai',
      speaker: value.speaker === '' || value.speaker === 'assistant' || value.speaker === 'tool' ? value.speaker : 'user',
      tool: value.tool === 'claude' || value.tool === 'codex' ? value.tool : '',
      sort: value.sort === 'timeline' ? 'timeline' : 'relevance',
      dateRange: value.dateRange === 'today' || value.dateRange === '7d' || value.dateRange === '30d' ? value.dateRange : 'all',
      sessionName: typeof value.sessionName === 'string' ? value.sessionName : '',
      cwd: typeof value.cwd === 'string' ? value.cwd : ''
    };
  } catch {
    return { query: '', mode: 'ai', speaker: 'user', tool: '', sort: 'relevance', dateRange: 'all', sessionName: '', cwd: '' };
  }
}

interface SearchViewProps {
  onResumeConversation: (
    serverId: string,
    providerSessionId: string,
    sourceSessionId?: string,
    historyId?: string
  ) => Promise<void>;
}

export function SearchView({ onResumeConversation }: SearchViewProps): JSX.Element {
  const initial = useMemo(readSearchState, []);
  const nativeClient = isTauri();
  const servers = useServers((state) => state.servers);
  const activeServerId = useServers((state) => state.activeId);
  const [query, setQuery] = useState(initial.query);
  const [submittedQuery, setSubmittedQuery] = useState('');
  const [mode, setMode] = useState<SearchMode>(nativeClient ? initial.mode : (initial.mode === 'ai' ? 'ranked' : initial.mode));
  const [speaker, setSpeaker] = useState<Speaker>(initial.speaker);
  const [tool, setTool] = useState<Tool>(initial.tool);
  const [sort, setSort] = useState<SortMode>(initial.sort);
  const [dateRange, setDateRange] = useState<DateRange>(initial.dateRange);
  const [sessionName, setSessionName] = useState(initial.sessionName);
  const [cwd, setCWD] = useState(initial.cwd);
  const [showMoreFilters, setShowMoreFilters] = useState(false);
  const [plan, setPlan] = useState<SmartSearchPlan | null>(null);
  const [planning, setPlanning] = useState(false);
  const [results, setResults] = useState<Result[]>([]);
  const [errors, setErrors] = useState<string[]>([]);
  const [loading, setLoading] = useState(false);
  const [continuingKey, setContinuingKey] = useState<string | null>(null);
  const [continuationError, setContinuationError] = useState<string | null>(null);
  const [selected, setSelected] = useState<SelectedConversation | null>(null);
  const planGeneration = useRef(0);
  const planAbort = useRef<AbortController | null>(null);
  const transcriptAbort = useRef<AbortController | null>(null);
  const previousActiveServerId = useRef(activeServerId);

  useEffect(() => {
    try {
      window.localStorage.setItem(SEARCH_STATE_KEY, JSON.stringify({
        query, mode, speaker, tool, sort, dateRange, sessionName, cwd
      }));
    } catch { /* storage is optional */ }
  }, [query, mode, speaker, tool, sort, dateRange, sessionName, cwd]);

  useEffect(() => () => {
    planAbort.current?.abort();
    transcriptAbort.current?.abort();
  }, []);

  useEffect(() => {
    if (previousActiveServerId.current === activeServerId) return;
    previousActiveServerId.current = activeServerId;
    planGeneration.current += 1;
    planAbort.current?.abort();
    setPlanning(false);
    setPlan(null);
  }, [activeServerId]);

  const effectiveQuery = mode === 'ai' ? (plan?.query.trim() ?? '') : submittedQuery;
  const dates = useMemo(() => dateFilters(dateRange), [dateRange]);

  useEffect(() => {
    if (!effectiveQuery) {
      setResults([]);
      setErrors([]);
      setLoading(false);
      return;
    }
    const controller = new AbortController();
    setLoading(true);
    const timer = window.setTimeout(() => {
      void Promise.all(servers.map(async (server) => {
        try {
          const [response, sessions, history, resumable] = await Promise.all([
            searchServer(server, {
            query: effectiveQuery,
            mode: 'ranked',
            role: speaker || undefined,
            tool: tool || undefined,
            name: sessionName.trim() || undefined,
            cwd: cwd.trim() || undefined,
            since: dates.since,
            until: dates.until,
            timeline: sort === 'timeline',
            limit: 250
            }, controller.signal),
            listServerSessions(server, controller.signal).catch(() => []),
            fetchServerHistory(server, controller.signal).catch(() => []),
            fetchServerResumableSessions(server, controller.signal).catch(() => [])
          ]);
          const managedMatches = enrichSearchResultsWithSessions(
            response.matches,
            filterTitleSearchSessions(sessions, { speaker, tool, sessionName, cwd, since: dates.since, until: dates.until }),
            query
          );
          const historyMatches = enrichSearchResultsWithHistory(
            managedMatches,
            filterTitleSearchHistory(history, { speaker, tool, sessionName, cwd, since: dates.since, until: dates.until }),
            query
          );
          const matches = enrichSearchResultsWithResumable(
            historyMatches,
            filterTitleSearchResumable(resumable, { speaker, tool, sessionName, cwd, since: dates.since, until: dates.until }),
            query,
            serverDisplayName(server, true)
          );
          return {
            matches: matches.map((match) => ({
              ...match,
              serverId: server.id,
              serverName: serverDisplayName(server, true)
            })),
            error: null
          };
        } catch (reason) {
          return {
            matches: [] as Result[],
            error: `${serverDisplayName(server, true)}: ${reason instanceof Error ? reason.message : 'unavailable'}`
          };
        }
      })).then((responses) => {
        if (controller.signal.aborted) return;
        setResults(responses.flatMap((response) => response.matches));
        setErrors(responses.flatMap((response) => response.error ? [response.error] : []));
        setLoading(false);
      });
    }, mode === 'ai' ? 0 : 180);
    return () => {
      window.clearTimeout(timer);
      controller.abort();
    };
  }, [effectiveQuery, mode, speaker, tool, sort, dateRange, dates.since, dates.until, sessionName, cwd, servers]);

  const orderedResults = useMemo(() => [...results].sort((left, right) => {
    if (sort === 'timeline') {
      return timestampValue(left.timestamp) - timestampValue(right.timestamp);
    }
    if (right.score !== left.score) return right.score - left.score;
    return timestampValue(right.timestamp) - timestampValue(left.timestamp);
  }), [results, sort]);

  const conversationGroups = useMemo(() => groupSearchResults(orderedResults), [orderedResults]);
  const updateQuery = (value: string): void => {
    setQuery(value);
    setSubmittedQuery('');
    if (mode === 'ai') {
      planGeneration.current += 1;
      planAbort.current?.abort();
      setPlanning(false);
      setPlan(null);
    }
  };

  const selectMode = (next: SearchMode): void => {
    planGeneration.current += 1;
    planAbort.current?.abort();
    setPlanning(false);
    setMode(next);
    setSubmittedQuery('');
    setPlan(null);
    setErrors([]);
  };

  const runAISearch = async (): Promise<void> => {
    const naturalQuery = query.trim();
    if (!naturalQuery || planning) return;
    const generation = planGeneration.current + 1;
    planGeneration.current = generation;
    planAbort.current?.abort();
    const controller = new AbortController();
    planAbort.current = controller;
    setPlanning(true);
    setPlan(null);
    setResults([]);
    setErrors([]);
    try {
      const nextPlan = await planSmartSearch(naturalQuery, controller.signal);
      if (planGeneration.current === generation) setPlan(nextPlan);
    } catch (reason) {
      if (planGeneration.current === generation && !controller.signal.aborted) {
        setErrors([reason instanceof Error ? reason.message : 'AI search planning failed']);
      }
    } finally {
      if (planGeneration.current === generation) setPlanning(false);
    }
  };

  const submitSearch = (): void => {
    if (mode === 'ai') {
      void runAISearch();
      return;
    }
    setResults([]);
    setErrors([]);
    setSubmittedQuery(query.trim());
  };

  const continueConversation = async (
    key: string,
    serverId: string,
    providerSessionId: string,
    sourceSessionId?: string,
    historyId?: string
  ): Promise<void> => {
    if (continuingKey) return;
    setContinuingKey(key);
    setContinuationError(null);
    try {
      await onResumeConversation(serverId, providerSessionId, sourceSessionId, historyId);
    } catch (reason) {
      setContinuationError(reason instanceof Error ? reason.message : 'Could not continue this conversation');
      setContinuingKey(null);
    }
  };

  const viewConversation = (group: SearchConversationGroup, result: Result = group.primary): void => {
    const server = servers.find((candidate) => candidate.id === result.serverId);
    if (!server) {
      setErrors([`The ${result.serverName} connection is no longer configured.`]);
      return;
    }
    transcriptAbort.current?.abort();
    const controller = new AbortController();
    transcriptAbort.current = controller;
    setSelected({ result, group, transcript: null, loading: true, error: null });
    const anchor = result.message_index ?? 0;
    void fetchServerHistoryTranscript(server, result.session_id, controller.signal, {
      start: Math.max(0, anchor - 2),
      end: anchor + 11,
      anchor,
      messageId: result.title_match ? undefined : result.message_id
    })
      .then((transcript) => setSelected((current) => current?.result === result
        ? { ...current, transcript: normalizeTranscriptIndexes(transcript), loading: false }
        : current))
      .catch((reason: unknown) => setSelected((current) => current?.result === result ? {
        ...current,
        loading: false,
        error: reason instanceof Error ? reason.message : 'Could not load the conversation'
      } : current));
  };

  if (selected) {
    return (
      <ConversationReader
        selected={selected}
        server={servers.find((candidate) => candidate.id === selected.result.serverId)}
        continuing={continuingKey === selected.group.key}
        continuationError={continuationError}
        onResumeConversation={(serverId, providerSessionId, sourceSessionId, historyId) => continueConversation(
          selected.group.key,
          serverId,
          providerSessionId,
          sourceSessionId,
          historyId
        )}
        onBack={() => {
          transcriptAbort.current?.abort();
          setSelected(null);
        }}
      />
    );
  }

  const hasSearch = mode === 'ai' ? Boolean(plan) : Boolean(submittedQuery);
  const resultSummary = hasSearch
    ? loading
      ? `Searching ${servers.length} machine${servers.length === 1 ? '' : 's'}…`
      : `${conversationGroups.length} conversation${conversationGroups.length === 1 ? '' : 's'}`
    : `${servers.length} machines ready`;
  const searchBusy = planning || loading;

  return (
    <div className="search-view">
      <div className="search-shell">
        <header className="search-heading">
          <h1>Search</h1>
          <p>Find something you or an agent said—then reopen the whole conversation around it.</p>
        </header>
        <form className="search-query-row" aria-busy={searchBusy} onSubmit={(event) => {
          event.preventDefault();
          submitSearch();
        }}>
          <span aria-hidden>⌕</span>
          <input
            autoFocus
            value={query}
            onChange={(event) => updateQuery(event.target.value)}
            placeholder={mode === 'ai'
              ? 'Where did I explain how the drafts rollout should work?'
              : 'Search a chat title, request, decision, or handoff…'}
          />
          <button type="submit" className="search-ai-submit" disabled={!query.trim() || searchBusy}>
            {planning ? 'Understanding…' : loading ? 'Searching…' : 'Search'}
          </button>
          {query && !searchBusy ? (
            <button type="button" aria-label="Clear search" onClick={() => updateQuery('')}>×</button>
          ) : null}
        </form>
        {searchBusy ? (
          <div className="search-progress" role="status" aria-live="polite">
            <span className="search-progress-track" aria-hidden><span /></span>
            <span>{planning ? 'Understanding what to look for…' : 'Searching titles and conversations on your machines…'}</span>
          </div>
        ) : null}

        <div className="search-mode-row">
          <div className="usage-segmented" role="tablist" aria-label="Search method">
            {(nativeClient ? ['ai', 'ranked'] as const : ['ranked'] as const).map((candidate) => (
              <button type="button" key={candidate} className={mode === candidate ? 'is-active' : ''} onClick={() => selectMode(candidate)}>
                {candidate === 'ai' ? 'Smart' : candidate === 'ranked' ? 'Keywords' : candidate}
              </button>
            ))}
          </div>
          {plan ? (
            <span className="search-plan">
              <ProviderBadge provider={plan.provider} compact /> planned <code>{plan.query}</code>
            </span>
          ) : null}
          <span className="search-result-count">{resultSummary}</span>
        </div>

        <section className="search-filter-panel" aria-label="Search filters">
          <FilterGroup label="Search in">
            <FilterButton active={speaker === 'user'} onClick={() => setSpeaker('user')}>What you said</FilterButton>
            <FilterButton active={speaker === ''} onClick={() => setSpeaker('')}>Everything</FilterButton>
            <FilterButton active={speaker === 'assistant'} onClick={() => setSpeaker('assistant')}>Agent answers</FilterButton>
            <FilterButton active={speaker === 'tool'} onClick={() => setSpeaker('tool')}>Operations</FilterButton>
          </FilterGroup>
          <FilterGroup label="Provider">
            <FilterButton active={tool === ''} onClick={() => setTool('')}>All</FilterButton>
            <FilterButton active={tool === 'claude'} onClick={() => setTool('claude')}><ProviderBadge provider="claude" compact /></FilterButton>
            <FilterButton active={tool === 'codex'} onClick={() => setTool('codex')}><ProviderBadge provider="codex" compact /></FilterButton>
          </FilterGroup>
          <button type="button" className="search-more-filters" onClick={() => setShowMoreFilters((value) => !value)}>
            {showMoreFilters ? 'Fewer filters' : 'More filters'}
          </button>
        </section>

        {showMoreFilters ? (
          <section className="search-advanced-filters">
            <label>
              <span>When</span>
              <select value={dateRange} onChange={(event) => setDateRange(event.target.value as DateRange)}>
                <option value="all">Any time</option>
                <option value="today">Today</option>
                <option value="7d">Last 7 days</option>
                <option value="30d">Last 30 days</option>
              </select>
            </label>
            <label>
              <span>Session name</span>
              <input value={sessionName} onChange={(event) => setSessionName(event.target.value)} placeholder="PM* or *builder*" />
            </label>
            <label>
              <span>Workspace</span>
              <input value={cwd} onChange={(event) => setCWD(event.target.value)} placeholder="~/somewhere/tech" />
            </label>
            <FilterGroup label="Order">
              <FilterButton active={sort === 'relevance'} onClick={() => setSort('relevance')}>Best matches</FilterButton>
              <FilterButton active={sort === 'timeline'} onClick={() => setSort('timeline')}>Timeline</FilterButton>
            </FilterGroup>
          </section>
        ) : null}

        {errors.length > 0 ? <div className="search-errors">{errors.join(' · ')}</div> : null}
        {continuationError ? <div className="search-errors">{continuationError}</div> : null}
        {searchBusy && orderedResults.length === 0 ? (
          <div className="search-loading-results" aria-hidden>
            <span /><span /><span />
          </div>
        ) : !hasSearch ? (
          <div className="search-welcome">
            <span>⌕</span>
            <h2>Start with what you remember</h2>
            <p>Smart Search sends only your question to the pre-authenticated Codex or Claude CLI. Transcripts stay local. Results open read-only at the exact matching message.</p>
          </div>
        ) : orderedResults.length === 0 && !loading ? (
          <div className="usage-empty">No matching conversations.</div>
        ) : (
          <div className="search-results">
            {conversationGroups.map((group) => (
              <SearchConversationCard
                key={group.key}
                group={group}
                ranked={mode === 'ai' || mode === 'ranked'}
                onView={(result) => viewConversation(group, result)}
                onResume={group.primary.provider_session_id
                  ? () => continueConversation(
                    group.key,
                    group.primary.serverId,
                    group.primary.provider_session_id as string,
                    managedSourceSessionID(group.primary),
                    isPromptHistoryOnly(group.primary) ? group.primary.session_id : undefined
                  )
                  : undefined}
                resumePending={continuingKey === group.key}
              />
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

function FilterGroup({ label, children }: { label: string; children: ReactNode }): JSX.Element {
  return <div className="search-filter-group"><span>{label}</span><div>{children}</div></div>;
}

function FilterButton({ active, onClick, children }: { active: boolean; onClick: () => void; children: ReactNode }): JSX.Element {
  return <button type="button" className={active ? 'is-active' : ''} onClick={onClick}>{children}</button>;
}

function SearchConversationCard({
  group,
  ranked,
  onView,
  onResume,
  resumePending = false
}: {
  group: SearchConversationGroup;
  ranked: boolean;
  onView: (result: Result) => void;
  onResume?: () => Promise<void>;
  resumePending?: boolean;
}): JSX.Element {
  const result = group.primary;
  const provider = normalizeProvider(result.tool);
  const messageMatches = group.matches.filter((match) => !match.title_match);
  const titleMatched = group.matches.some((match) => match.title_match);
  const latest = group.matches.reduce<string | null>((current, match) => {
    if (!match.timestamp) return current;
    return !current || timestampValue(match.timestamp) > timestampValue(current) ? match.timestamp : current;
  }, null);
  const promptHistoryOnly = isPromptHistoryOnly(result);
  return (
    <article className={`search-result-card${provider ? ` is-${provider}` : ''}`}>
      <span className="search-result-provider" aria-label={provider ? `${provider} conversation` : 'Saved conversation'}>
        <ParserIcon icon={provider === 'claude' ? '🟠' : provider === 'codex' ? '🟢' : '⬛'} size={24} />
      </span>
      <span className="search-result-body">
        <button type="button" className="search-result-main" onClick={() => onView(result)}>
          <span className="search-result-source">
            <strong>{group.title}</strong>
            {titleMatched ? <span className="search-title-match">Title match</span> : null}
          </span>
          <span className="search-conversation-match-count">
            {promptHistoryOnly
              ? `${messageMatches.length} retained user prompt${messageMatches.length === 1 ? '' : 's'} · full transcript not locally readable`
              : messageMatches.length > 0
              ? `${messageMatches.length} matching message${messageMatches.length === 1 ? '' : 's'}`
              : 'Named conversation'}
            {group.sourceSessionIds.length > 1 ? ` · continued across ${group.sourceSessionIds.length} Sessions runs` : ''}
          </span>
        </button>
        <span className="search-conversation-matches">
          {messageMatches.slice(0, 2).map((match) => (
            <button
              type="button"
              className="search-conversation-match"
              key={`${match.session_id}:${match.message_id || match.message_index}`}
              onClick={() => onView(match)}
            >
              <span>{searchSpeakerLabel(match, provider)}</span>
              <span className="search-snippet"><SearchSnippet value={match.snippet} /></span>
            </button>
          ))}
          {messageMatches.length > 2 ? (
            <span className="search-conversation-more">+ {messageMatches.length - 2} more matches</span>
          ) : null}
        </span>
        <span className="search-result-footer">
          <span className="search-result-location">
            {provider ? <ProviderBadge provider={provider} compact /> : null}
            <span>{compactMachineName(result.serverName || result.machine)}</span>
            {result.cwd ? <code>{compactPath(result.cwd)}</code> : null}
            {latest ? <time>{relativeDate(latest)}</time> : null}
            {ranked && !titleMatched ? <span>{rankedMatchLabel(result.score)}</span> : null}
          </span>
          <span className="search-result-actions">
            <button type="button" onClick={() => onView(result)}>{promptHistoryOnly ? 'View retained prompts' : 'Open conversation'} <span aria-hidden>→</span></button>
            {onResume ? (
              <button type="button" className="is-resume" disabled={resumePending} onClick={() => { void onResume(); }}>
                {resumePending ? 'Continuing…' : 'Continue conversation'}
              </button>
            ) : null}
          </span>
        </span>
      </span>
    </article>
  );
}

function searchSpeakerLabel(result: Result, provider: Provider | null): string {
  if (result.role === 'user') return 'You said';
  if (result.role === 'tool') return operationLabel(result.kind);
  if (provider === 'claude') return 'Claude said';
  if (provider === 'codex') return 'Codex said';
  return 'Agent said';
}

function SearchSnippet({ value }: { value: string }): JSX.Element {
  const parts = value.split(/(\[\[|\]\])/);
  let highlighted = false;
  return (
    <>
      {parts.map((part, index) => {
        if (part === '[[') {
          highlighted = true;
          return null;
        }
        if (part === ']]') {
          highlighted = false;
          return null;
        }
        return highlighted ? <mark key={index}>{part}</mark> : <span key={index}>{part}</span>;
      })}
    </>
  );
}

function operationLabel(kind?: SearchMatch['kind']): string {
  if (kind === 'delegation') return 'Delegation';
  if (kind === 'handoff') return 'Handoff';
  if (kind === 'automation') return 'Automation';
  if (kind === 'status') return 'Status';
  return 'Operation';
}

function ConversationReader({
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
  const provider = normalizeProvider(readerTranscript?.session.tool ?? selected.result.tool);
  const anchor = selected.result.message_index;

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
    void fetchServerHistoryTranscript(server, selected.result.session_id, controller.signal, window)
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
    void fetchServerHistoryTranscript(server, selected.result.session_id, controller.signal, {
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
    ?? selected.result.provider_session_id;
  const promptHistoryOnly = isPromptHistoryOnly(selected.result);
  const canResume = Boolean(providerSessionID && provider);

  return (
    <div className="search-view search-conversation-view">
      <div className="search-shell search-reader-shell">
        <div className="search-reader-chrome">
          <button type="button" className="search-back" onClick={onBack}>← Back to results</button>
          <header className="search-conversation-heading">
            <div>
              <span className="search-conversation-kicker">
                {promptHistoryOnly ? 'Claude prompt history only' : 'Read-only transcript'} · {selected.group.matches.filter((match) => !match.title_match).length} message match{selected.group.matches.filter((match) => !match.title_match).length === 1 ? '' : 'es'} · opened at message {anchor + 1}
              </span>
              <h1>{readerTranscript?.session.name || selected.group.title || selected.result.name || selected.result.session_id.slice(0, 8)}</h1>
              <p>
                {provider ? <ProviderBadge provider={provider} /> : null}
                <span>{compactMachineName(server?.name ?? selected.result.serverName)}</span>
                {readerTranscript?.session.cwd ? <code>{compactPath(readerTranscript.session.cwd)}</code> : null}
              </p>
            </div>
            <div className="search-conversation-actions">
              <span>{promptHistoryOnly
                ? 'Sessions found Claude’s prompt index, but not a full local transcript. Continue restores this exact conversation in its recorded workspace.'
                : 'Viewing is read-only. Continue opens this exact provider conversation in a new runtime.'}</span>
              {canResume ? (
                <button
                  type="button"
                  className="btn btn-primary"
                  disabled={continuing}
                  onClick={() => { void onResumeConversation(
                    selected.result.serverId,
                    providerSessionID as string,
                    managedSourceSessionID(selected.result),
                    promptHistoryOnly ? selected.result.session_id : undefined
                  ); }}
                >
                  {continuing ? 'Continuing…' : 'Continue conversation'}
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
              const isAnchor = message.index === anchor;
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

function normalizeTranscriptIndexes(transcript: HistoryTranscript): HistoryTranscript {
  return {
    ...transcript,
    messages: transcript.messages.map((message, index) => ({
      ...message,
      index: Number.isFinite(message.index) ? message.index : index,
      id: message.id || `legacy:${Number.isFinite(message.index) ? message.index : index}`
    }))
  };
}

function dateFilters(range: DateRange): { since?: string; until?: string } {
  if (range === 'all') return {};
  const now = new Date();
  if (range === 'today') {
    const today = localDate(now);
    return { since: today, until: today };
  }
  const days = range === '7d' ? 7 : 30;
  const since = new Date(now);
  since.setDate(since.getDate() - days + 1);
  return { since: localDate(since) };
}

function localDate(value: Date): string {
  const year = value.getFullYear();
  const month = String(value.getMonth() + 1).padStart(2, '0');
  const day = String(value.getDate()).padStart(2, '0');
  return `${year}-${month}-${day}`;
}

function timestampValue(value: string | null): number {
  if (!value) return 0;
  const parsed = new Date(value).getTime();
  return Number.isNaN(parsed) ? 0 : parsed;
}

function rankedMatchLabel(score: number): string {
  if (score >= 0.85) return 'Best match';
  if (score >= 0.5) return 'Strong match';
  return 'Related';
}

function compactPath(value: string): string {
  return value.replace(/^\/(Users|home)\/[^/]+/, '~');
}

function compactMachineName(value: string): string {
  const clean = value.trim().replace(/\.local$/i, '');
  if (/^mac[-\s]?mini(?:[-\s]?\d+)?$/i.test(clean)) return 'Mac mini';
  if (/^macbook(?:[-\s]?pro)?(?:[-\s]?\d+)?$/i.test(clean)) return 'MacBook';
  return clean.replace(/-/g, ' ') || 'Unknown computer';
}

function managedSourceSessionID(result: SearchMatch): string | undefined {
  return result.session_id.startsWith('provider:') ? undefined : result.session_id;
}

function isPromptHistoryOnly(result: SearchMatch): boolean {
  return result.session_id.startsWith('provider-history:');
}

function filterTitleSearchSessions(
  sessions: SessionInfo[],
  filters: {
    speaker: Speaker;
    tool: Tool;
    sessionName: string;
    cwd: string;
    since?: string;
    until?: string;
  }
): SessionInfo[] {
  if (filters.speaker === 'assistant' || filters.speaker === 'tool') return [];
  const since = filters.since ? new Date(`${filters.since}T00:00:00`).getTime() : null;
  const until = filters.until ? new Date(`${filters.until}T23:59:59.999`).getTime() : null;
  return sessions.filter((session) => {
    const provider = session.tool === 'claude-code' ? 'claude' : session.tool === 'codex' ? 'codex' : 'shell';
    if (filters.tool && provider !== filters.tool) return false;
    if (filters.cwd.trim() && !session.cwd.toLocaleLowerCase().includes(filters.cwd.trim().toLocaleLowerCase())) return false;
    if (filters.sessionName.trim()) {
      const pattern = filters.sessionName.trim()
        .replace(/[.+?^${}()|[\]\\]/g, '\\$&')
        .replaceAll('*', '.*');
      const names = [session.claudeCustomTitle, session.name, session.claudeAiTitle, session.description].filter(Boolean).join(' ');
      if (!new RegExp(pattern, 'i').test(names)) return false;
    }
    const activity = session.exitedAt ?? session.lastDataAt ?? session.createdAt;
    if (since !== null && activity < since) return false;
    if (until !== null && activity > until) return false;
    return true;
  });
}

function filterTitleSearchHistory(
  sessions: import('../api/sessionsd').HistorySession[],
  filters: {
    speaker: Speaker;
    tool: Tool;
    sessionName: string;
    cwd: string;
    since?: string;
    until?: string;
  }
): import('../api/sessionsd').HistorySession[] {
  if (filters.speaker === 'assistant' || filters.speaker === 'tool') return [];
  const since = filters.since ? new Date(`${filters.since}T00:00:00`).getTime() : null;
  const until = filters.until ? new Date(`${filters.until}T23:59:59.999`).getTime() : null;
  return sessions.filter((session) => {
    if (filters.tool && session.tool !== filters.tool) return false;
    if (filters.cwd.trim() && !session.cwd.toLocaleLowerCase().includes(filters.cwd.trim().toLocaleLowerCase())) return false;
    if (filters.sessionName.trim()) {
      const pattern = filters.sessionName.trim()
        .replace(/[.+?^${}()|[\]\\]/g, '\\$&')
        .replaceAll('*', '.*');
      if (!new RegExp(pattern, 'i').test(session.name)) return false;
    }
    if (since !== null && session.last_activity_at < since) return false;
    if (until !== null && session.last_activity_at > until) return false;
    return true;
  });
}

function filterTitleSearchResumable(
  sessions: ResumableSession[],
  filters: {
    speaker: Speaker;
    tool: Tool;
    sessionName: string;
    cwd: string;
    since?: string;
    until?: string;
  }
): ResumableSession[] {
  if (filters.speaker === 'assistant' || filters.speaker === 'tool') return [];
  const since = filters.since ? new Date(`${filters.since}T00:00:00`).getTime() : null;
  const until = filters.until ? new Date(`${filters.until}T23:59:59.999`).getTime() : null;
  return sessions.filter((session) => {
    if (filters.tool && session.tool !== filters.tool) return false;
    if (filters.cwd.trim() && !session.cwd.toLocaleLowerCase().includes(filters.cwd.trim().toLocaleLowerCase())) return false;
    if (filters.sessionName.trim()) {
      const pattern = filters.sessionName.trim()
        .replace(/[.+?^${}()|[\]\\]/g, '\\$&')
        .replaceAll('*', '.*');
      const names = [session.title, session.firstUserMessage].filter(Boolean).join(' ');
      if (!new RegExp(pattern, 'i').test(names)) return false;
    }
    if (since !== null && session.modifiedAt < since) return false;
    if (until !== null && session.modifiedAt > until) return false;
    return true;
  });
}

function relativeDate(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
}
