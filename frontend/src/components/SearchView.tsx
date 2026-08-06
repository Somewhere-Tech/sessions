import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import {
  fetchServerHistory,
  fetchServerHistoryTranscript,
  fetchServerResumableSessions,
  listServerSessions,
  planSmartSearch,
  searchServer,
  type HistoryTranscript,
  type ResumableSession,
  type SearchMatch,
  type SearchSessionHits,
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
import {
  compactMachineName,
  compactPath,
  isPromptHistoryOnly,
  managedSourceSessionID,
  plural,
  relativeDate,
  type BrowseFilters,
  type ConversationRow,
  type ResumeTarget
} from '../lib/conversationBrowser';
import { serverDisplayName, useServers, type ServerConfig } from '../lib/servers';
import { isTauri } from '../lib/tauriBridge';
import { ConversationBrowser } from './ConversationBrowser';
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

// The daemon's per-session rollup, tagged with the machine it came from. It
// counts every hit in the index, not the page of messages that came back, so
// it is the only thing on this screen that can answer "which session was that"
// for a session whose messages never reached the page.
interface RollupSession extends SearchSessionHits {
  serverId: string;
  serverName: string;
}

// What one machine said about the search itself. Every field is optional at
// the wire: a daemon older than the rollup sends none of them and each of
// these stays null, which is the signal to say nothing rather than to guess.
interface SearchMeta {
  serverId: string;
  serverName: string;
  rewrittenQuery: string | null;
  matchMode: string | null;
  totalHits: number | null;
  totalSessions: number | null;
  rollupPartial: boolean;
}

// One session's hits, summed over every Sessions run that continued the same
// conversation. Null when this daemon did not send a rollup at all — which is
// different from a rollup that says zero.
interface RollupSummary {
  hits: number;
  firstHitAt: string | null;
  lastHitAt: string | null;
  titleMatch: boolean;
}

type ResultRow =
  | { kind: 'group'; key: string; group: SearchConversationGroup; rollup: RollupSummary | null }
  | { kind: 'rollup'; key: string; entry: RollupSession };

interface SelectedConversation {
  token: number;
  key: string;
  serverId: string;
  serverName: string;
  sessionId: string;
  providerSessionId?: string;
  tool: string;
  title: string;
  // The message this view was opened at, or null when the session was opened
  // from a rollup and Search does not know where in it the hits are. A null
  // anchor must never be drawn as a match: message 1 is not the answer.
  anchor: number | null;
  matchCount: number | null;
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
  /**
   * Attach to a session that is already running. A live conversation cannot be
   * resumed — the daemon refuses it (session.ConversationLiveError) — so the
   * browse rows need somewhere real to send the user instead.
   */
  onOpenLiveSession?: (serverId: string, sessionId: string) => void;
}

export function SearchView({ onResumeConversation, onOpenLiveSession }: SearchViewProps): JSX.Element {
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
  const [rollup, setRollup] = useState<RollupSession[]>([]);
  const [metas, setMetas] = useState<SearchMeta[]>([]);
  const [errors, setErrors] = useState<string[]>([]);
  const [loading, setLoading] = useState(false);
  const [continuingKey, setContinuingKey] = useState<string | null>(null);
  const [continuationError, setContinuationError] = useState<string | null>(null);
  const [selected, setSelected] = useState<SelectedConversation | null>(null);
  const selectionToken = useRef(0);
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

  // Title enrichment below deliberately uses `effectiveQuery`, not the live
  // `query` input. The dependency list only ever carried `effectiveQuery`, so
  // reading `query` here matched titles against whatever the user had typed
  // since — highlighting and title hits for a search that was never run.
  // The results are for `effectiveQuery`; so is their enrichment.
  useEffect(() => {
    if (!effectiveQuery) {
      setResults([]);
      setRollup([]);
      setMetas([]);
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
            effectiveQuery
          );
          const historyMatches = enrichSearchResultsWithHistory(
            managedMatches,
            filterTitleSearchHistory(history, { speaker, tool, sessionName, cwd, since: dates.since, until: dates.until }),
            effectiveQuery
          );
          const matches = enrichSearchResultsWithResumable(
            historyMatches,
            filterTitleSearchResumable(resumable, { speaker, tool, sessionName, cwd, since: dates.since, until: dates.until }),
            effectiveQuery,
            serverDisplayName(server, true)
          );
          const serverName = serverDisplayName(server, true);
          return {
            matches: matches.map((match) => ({
              ...match,
              serverId: server.id,
              serverName
            })),
            // Absent fields stay absent. An older daemon sends matches and a
            // total and nothing else, and every null below is what keeps this
            // screen from inventing a rollup it was never given.
            sessions: (response.sessions ?? []).map((session) => ({ ...session, serverId: server.id, serverName })),
            meta: {
              serverId: server.id,
              serverName,
              rewrittenQuery: response.effective_query ?? null,
              matchMode: response.match_mode ?? null,
              totalHits: response.total_hits ?? null,
              totalSessions: response.total_sessions ?? null,
              rollupPartial: response.rollup_partial === true || response.partial === true
            } satisfies SearchMeta,
            error: null
          };
        } catch (reason) {
          return {
            matches: [] as Result[],
            sessions: [] as RollupSession[],
            meta: null,
            error: `${serverDisplayName(server, true)}: ${reason instanceof Error ? reason.message : 'unavailable'}`
          };
        }
      })).then((responses) => {
        if (controller.signal.aborted) return;
        setResults(responses.flatMap((response) => response.matches));
        setRollup(responses.flatMap((response) => response.sessions));
        setMetas(responses.flatMap((response) => response.meta ? [response.meta] : []));
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

  // The rollup is keyed by the daemon's own session id, and one conversation
  // can span several of those when it was continued across Sessions runs — so
  // a group claims every rollup row its runs produced, and their hits add up.
  const rows = useMemo<ResultRow[]>(() => {
    const byKey = new Map(rollup.map((entry) => [`${entry.serverId}:${entry.session_id}`, entry]));
    const claimed = new Set<string>();
    const groupRows = conversationGroups.map<ResultRow>((group) => {
      const entries = group.sourceSessionIds
        .map((sessionID) => byKey.get(`${group.primary.serverId}:${sessionID}`))
        .filter((entry): entry is RollupSession => Boolean(entry));
      for (const entry of entries) claimed.add(`${entry.serverId}:${entry.session_id}`);
      return { kind: 'group', key: group.key, group, rollup: summarizeRollup(entries) };
    });
    // Sessions the rollup counted but whose messages never reached the page.
    // These are the ones a message list silently loses, so they get a row.
    const orphans = rollup
      .filter((entry) => !claimed.has(`${entry.serverId}:${entry.session_id}`) && entry.hits > 0)
      .sort((left, right) => (right.score - left.score)
        || (timestampValue(right.last_hit_at ?? null) - timestampValue(left.last_hit_at ?? null)))
      .map<ResultRow>((entry) => ({ kind: 'rollup', key: `rollup:${entry.serverId}:${entry.session_id}`, entry }));
    return [...groupRows, ...orphans];
  }, [conversationGroups, rollup]);

  // Fleet totals are only reported when every machine that answered supplied
  // them. One old daemon in the fleet makes the sum a fiction, and a fiction
  // is worse here than saying nothing.
  const totals = useMemo(() => {
    if (metas.length === 0 || !metas.every((meta) => meta.totalSessions !== null && meta.totalHits !== null)) return null;
    return {
      sessions: metas.reduce((sum, meta) => sum + (meta.totalSessions ?? 0), 0),
      hits: metas.reduce((sum, meta) => sum + (meta.totalHits ?? 0), 0)
    };
  }, [metas]);

  // A count is a lower bound whenever any part of the fleet did not finish:
  // a truncated rollup, a machine that failed, or a machine that never replied.
  const countsArePartial = metas.some((meta) => meta.rollupPartial)
    || errors.length > 0
    || metas.length < servers.length;

  const queryNotice = useMemo(
    () => describeQueryRewrite(effectiveQuery, metas),
    [effectiveQuery, metas]
  );

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
    setRollup([]);
    setMetas([]);
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
    setRollup([]);
    setMetas([]);
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
      setContinuationError(reason instanceof Error ? reason.message : 'Could not resume this conversation');
      setContinuingKey(null);
    }
  };

  const openConversation = (
    target: Omit<SelectedConversation, 'token' | 'transcript' | 'loading' | 'error'>,
    messageId?: string
  ): void => {
    const server = servers.find((candidate) => candidate.id === target.serverId);
    if (!server) {
      setErrors([`The ${target.serverName} connection is no longer configured.`]);
      return;
    }
    transcriptAbort.current?.abort();
    const controller = new AbortController();
    transcriptAbort.current = controller;
    selectionToken.current += 1;
    const token = selectionToken.current;
    setSelected({ ...target, token, transcript: null, loading: true, error: null });
    const anchor = target.anchor;
    void fetchServerHistoryTranscript(server, target.sessionId, controller.signal, anchor === null
      ? { start: 0, end: 12 }
      : { start: Math.max(0, anchor - 2), end: anchor + 11, anchor, messageId })
      .then((transcript) => setSelected((current) => current?.token === token
        ? { ...current, transcript: normalizeTranscriptIndexes(transcript), loading: false }
        : current))
      .catch((reason: unknown) => setSelected((current) => current?.token === token ? {
        ...current,
        loading: false,
        error: reason instanceof Error ? reason.message : 'Could not load the conversation'
      } : current));
  };

  const viewConversation = (group: SearchConversationGroup, result: Result = group.primary): void => {
    openConversation({
      key: group.key,
      serverId: result.serverId,
      serverName: result.serverName,
      sessionId: result.session_id,
      providerSessionId: result.provider_session_id,
      tool: result.tool,
      title: group.title || result.name || result.session_id.slice(0, 8),
      anchor: result.message_index ?? 0,
      matchCount: group.matches.filter((match) => !match.title_match).length
    }, result.title_match ? undefined : result.message_id);
  };

  // The browse list under an empty query answers the other half of "which
  // conversation was that": the one the user cannot name a word from, only a
  // day and a folder. It reads the same filters this screen already carries.
  const browseFilters = useMemo<Omit<BrowseFilters, 'all'>>(() => ({
    tool,
    since: dates.since ? new Date(`${dates.since}T00:00:00`).getTime() : 0,
    until: dates.until ? new Date(`${dates.until}T23:59:59.999`).getTime() : 0,
    name: sessionName,
    cwd
  }), [tool, dates.since, dates.until, sessionName, cwd]);
  const browseFiltered = Boolean(tool || dates.since || dates.until || sessionName.trim() || cwd.trim());

  const viewBrowsedConversation = (row: ConversationRow): void => {
    openConversation({
      key: `browse:${row.key}`,
      serverId: row.serverId,
      serverName: row.serverName,
      sessionId: row.id,
      providerSessionId: row.providerSessionId,
      tool: row.tool,
      title: row.name || row.id.slice(0, 8),
      // Nothing was searched for, so there is no message to anchor on and no
      // match count to claim. Both stay null rather than pointing at message 1.
      anchor: null,
      matchCount: null
    });
  };

  const resumeBrowsedConversation = (row: ConversationRow, target: ResumeTarget): void => {
    void continueConversation(
      `browse:${row.key}`,
      row.serverId,
      target.providerSessionId,
      target.sourceSessionId,
      target.historyId
    );
  };

  // Opening a session Search only knows from the rollup: it has the session and
  // the hit count, but not where in the transcript the hits are, so it opens at
  // the beginning and says so rather than pointing at a message it guessed.
  const viewRollupSession = (entry: RollupSession, key: string): void => {
    openConversation({
      key,
      serverId: entry.serverId,
      serverName: entry.serverName,
      sessionId: entry.session_id,
      tool: entry.tool ?? '',
      title: entry.name || entry.session_id.slice(0, 8),
      anchor: null,
      matchCount: entry.hits
    });
  };

  if (selected) {
    return (
      <ConversationReader
        selected={selected}
        server={servers.find((candidate) => candidate.id === selected.serverId)}
        continuing={continuingKey === selected.key}
        continuationError={continuationError}
        onResumeConversation={(serverId, providerSessionId, sourceSessionId, historyId) => continueConversation(
          selected.key,
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
      : totals
        ? `${countsArePartial ? 'at least ' : ''}${plural(totals.sessions, 'conversation')} · ${plural(totals.hits, 'match', 'matches')}${totals.sessions > rows.length ? ` · ${rows.length} shown` : ''}`
        : plural(rows.length, 'conversation')
    : `${servers.length} machines ready`;
  const searchBusy = planning || loading;
  const showCountCaveat = countsArePartial
    && !loading
    && Boolean(totals || rows.some((row) => row.kind === 'rollup' || row.rollup));

  return (
    <div className="search-view">
      <div className="search-shell">
        <header className="search-heading">
          <h1>History</h1>
          <p>Every Claude and Codex conversation recorded on your machines. Browse the recent ones, or search for something that was said.</p>
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
          {/* Who spoke narrows a search and means nothing to a browse, so it is
              only offered once there is a query to apply it to. */}
          {hasSearch ? (
            <FilterGroup label="Search in">
              <FilterButton active={speaker === 'user'} onClick={() => setSpeaker('user')}>What you said</FilterButton>
              <FilterButton active={speaker === ''} onClick={() => setSpeaker('')}>Everything</FilterButton>
              <FilterButton active={speaker === 'assistant'} onClick={() => setSpeaker('assistant')}>Agent answers</FilterButton>
              <FilterButton active={speaker === 'tool'} onClick={() => setSpeaker('tool')}>Operations</FilterButton>
            </FilterGroup>
          ) : null}
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
        {/* Both notices are silent on a normal strict search: a screen that
            explains itself every time teaches people to stop reading it. */}
        {hasSearch && !loading && queryNotice ? (
          <div className={`search-notice${queryNotice.relaxed ? ' is-relaxed' : ''}`} role="status">
            <span aria-hidden>{queryNotice.relaxed ? '⌁' : '⇢'}</span>
            <span>
              {queryNotice.text}
              {queryNotice.query ? <> Results are for <code>{queryNotice.query}</code></> : null}
            </span>
          </div>
        ) : null}
        {showCountCaveat ? (
          <div className="search-notice" role="status">
            <span aria-hidden>±</span>
            <span>
              {errors.length > 0
                ? 'A machine did not answer, so these counts are lower bounds — there is more history than this.'
                : 'This count did not finish, so the numbers here are lower bounds — there is more history than this.'}
            </span>
          </div>
        ) : null}
        {searchBusy && orderedResults.length === 0 ? (
          <div className="search-loading-results" aria-hidden>
            <span /><span /><span />
          </div>
        ) : !hasSearch ? (
          // No query is not an empty screen. It is the case the CLI's
          // `sessions history` exists for: the conversation you cannot quote a
          // word from, only place in a folder and a day.
          <ConversationBrowser
            filters={browseFilters}
            filtered={browseFiltered}
            resumingKey={continuingKey}
            onOpen={viewBrowsedConversation}
            onResume={resumeBrowsedConversation}
            onAttach={onOpenLiveSession ? (row) => {
              if (row.liveSessionId) onOpenLiveSession(row.serverId, row.liveSessionId);
            } : undefined}
          />
        ) : rows.length === 0 && !loading ? (
          <div className="usage-empty">No matching conversations.</div>
        ) : (
          <div className="search-results">
            {rows.map((row) => row.kind === 'group' ? (
              <SearchConversationCard
                key={row.key}
                group={row.group}
                rollup={row.rollup}
                countsArePartial={countsArePartial}
                ranked={mode === 'ai' || mode === 'ranked'}
                onView={(result) => viewConversation(row.group, result)}
                onResume={normalizeProvider(row.group.primary.tool)
                  ? () => continueConversation(
                    row.key,
                    row.group.primary.serverId,
                    row.group.primary.provider_session_id || row.group.primary.session_id,
                    managedSourceSessionID(row.group.primary.session_id),
                    !row.group.primary.provider_session_id || isPromptHistoryOnly(row.group.primary.session_id)
                      ? row.group.primary.session_id
                      : undefined
                  )
                  : undefined}
                resumePending={continuingKey === row.key}
              />
            ) : (
              <SearchRollupCard
                key={row.key}
                entry={row.entry}
                countsArePartial={countsArePartial}
                onView={() => viewRollupSession(row.entry, row.key)}
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
  rollup,
  countsArePartial,
  ranked,
  onView,
  onResume,
  resumePending = false
}: {
  group: SearchConversationGroup;
  rollup: RollupSummary | null;
  countsArePartial: boolean;
  ranked: boolean;
  onView: (result: Result) => void;
  onResume?: () => Promise<void>;
  resumePending?: boolean;
}): JSX.Element {
  const result = group.primary;
  const provider = normalizeProvider(result.tool);
  const messageMatches = group.matches.filter((match) => !match.title_match);
  const titleMatched = group.matches.some((match) => match.title_match) || rollup?.titleMatch === true;
  const latest = group.matches.reduce<string | null>((current, match) => {
    if (!match.timestamp) return current;
    return !current || timestampValue(match.timestamp) > timestampValue(current) ? match.timestamp : current;
  }, null);
  const promptHistoryOnly = isPromptHistoryOnly(result.session_id);
  // The rollup counts the session; the page only carries what fitted. Taking
  // the larger of the two keeps the number a lower bound either way, and never
  // prints a total smaller than the matches listed right underneath it.
  const hits = rollup ? Math.max(rollup.hits, messageMatches.length) : null;
  const span = formatHitSpan(rollup?.firstHitAt ?? null, rollup?.lastHitAt ?? null);
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
              ? `${plural(messageMatches.length, 'retained user prompt')} · full transcript not locally readable`
              : hits !== null
              ? `${countsArePartial ? 'at least ' : ''}${plural(hits, 'matching message')}${hits > messageMatches.length
                ? ` · ${messageMatches.length > 0 ? `${messageMatches.length} shown here` : 'none shown here'}`
                : ''}`
              : messageMatches.length > 0
              ? plural(messageMatches.length, 'matching message')
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
            {span
              ? <time className="search-session-span" title={span.title}>{span.label}</time>
              : latest ? <time>{relativeDate(latest)}</time> : null}
            {ranked && !titleMatched ? <span>{rankedMatchLabel(result.score)}</span> : null}
          </span>
          <span className="search-result-actions">
            <button type="button" onClick={() => onView(result)}>{promptHistoryOnly ? 'View retained prompts' : 'Open conversation'} <span aria-hidden>→</span></button>
            {onResume ? (
              <button type="button" className="is-resume" disabled={resumePending} onClick={() => { void onResume(); }}>
                {resumePending ? 'Resuming…' : 'Resume conversation'}
              </button>
            ) : null}
          </span>
        </span>
      </span>
    </article>
  );
}

// A session the rollup counted whose messages never reached the returned page.
// Without this card the conversation is simply invisible — which is the exact
// failure the rollup exists to end.
function SearchRollupCard({
  entry,
  countsArePartial,
  onView
}: {
  entry: RollupSession;
  countsArePartial: boolean;
  onView: () => void;
}): JSX.Element {
  const provider = normalizeProvider(entry.tool ?? '');
  const span = formatHitSpan(entry.first_hit_at ?? null, entry.last_hit_at ?? null);
  const snippets = (entry.snippets ?? []).filter((snippet) => snippet.trim()).slice(0, 2);
  return (
    <article className={`search-result-card is-rollup-only${provider ? ` is-${provider}` : ''}`}>
      <span className="search-result-provider" aria-label={provider ? `${provider} conversation` : 'Saved conversation'}>
        <ParserIcon icon={provider === 'claude' ? '🟠' : provider === 'codex' ? '🟢' : '⬛'} size={24} />
      </span>
      <span className="search-result-body">
        <button type="button" className="search-result-main" onClick={onView}>
          <span className="search-result-source">
            <strong>{entry.name.trim() || 'Saved conversation'}</strong>
            {entry.title_match ? <span className="search-title-match">Title match</span> : null}
          </span>
          <span className="search-conversation-match-count">
            {`${countsArePartial ? 'at least ' : ''}${plural(entry.hits, 'matching message')} · none shown here`}
          </span>
        </button>
        {snippets.length > 0 ? (
          <span className="search-conversation-matches">
            {snippets.map((snippet, index) => (
              <span className="search-conversation-match is-static" key={index}>
                <span>In this chat</span>
                <span className="search-snippet"><SearchSnippet value={snippet} /></span>
              </span>
            ))}
          </span>
        ) : null}
        <span className="search-result-footer">
          <span className="search-result-location">
            {provider ? <ProviderBadge provider={provider} compact /> : null}
            <span>{compactMachineName(entry.serverName || entry.machine || '')}</span>
            {entry.cwd ? <code>{compactPath(entry.cwd)}</code> : null}
            {span ? <time className="search-session-span" title={span.title}>{span.label}</time> : null}
          </span>
          <span className="search-result-actions">
            <button type="button" onClick={onView}>Open conversation <span aria-hidden>→</span></button>
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
                ? 'Sessions found Claude’s prompt index, but not a full local transcript. Resume restores this exact conversation in its recorded workspace.'
                : 'Viewing is read-only. Resume opens this exact conversation in a new runtime.'}</span>
              {canResume ? (
                <button
                  type="button"
                  className="btn btn-primary"
                  disabled={continuing}
                  onClick={() => { void onResumeConversation(
                    selected.serverId,
                    providerSessionID || historyID,
                    managedSourceSessionID(selected.sessionId),
                    !providerSessionID || promptHistoryOnly ? historyID : undefined
                  ); }}
                >
                  {continuing ? 'Resuming…' : 'Resume conversation'}
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

// compactPath, compactMachineName, managedSourceSessionID, isPromptHistoryOnly,
// plural and relativeDate now live in lib/conversationBrowser.ts: the browse
// rows below the search box draw the same conversation row and must read a
// path, a machine, a date and a resume target identically.

// Adds one conversation's rollup rows together. Several Sessions runs can
// continue the same conversation, and the user asked about the conversation.
function summarizeRollup(entries: RollupSession[]): RollupSummary | null {
  if (entries.length === 0) return null;
  let firstHitAt: string | null = null;
  let lastHitAt: string | null = null;
  for (const entry of entries) {
    if (entry.first_hit_at && (!firstHitAt || timestampValue(entry.first_hit_at) < timestampValue(firstHitAt))) {
      firstHitAt = entry.first_hit_at;
    }
    if (entry.last_hit_at && (!lastHitAt || timestampValue(entry.last_hit_at) > timestampValue(lastHitAt))) {
      lastHitAt = entry.last_hit_at;
    }
  }
  return {
    hits: entries.reduce((sum, entry) => sum + entry.hits, 0),
    firstHitAt,
    lastHitAt,
    titleMatch: entries.some((entry) => entry.title_match === true)
  };
}

// What, if anything, to tell the user about the query that actually ran.
// Returns null for the ordinary case — a strict search of exactly what was
// typed — because a notice that appears every time is read no times.
function describeQueryRewrite(
  sentQuery: string,
  metas: SearchMeta[]
): { text: string; query: string | null; relaxed: boolean } | null {
  if (!sentQuery || metas.length === 0) return null;
  const rewritten = [...new Set(metas.map((meta) => meta.rewrittenQuery).filter((value): value is string => Boolean(value)))];
  const differing = rewritten.filter((value) => normalizeQueryText(value) !== normalizeQueryText(sentQuery));
  const shown = differing.length === 1 ? differing[0] : null;
  const broad = metas.filter((meta) => meta.matchMode === 'broad');
  if (broad.length > 0) {
    const scope = broad.length === metas.length || metas.length === 1
      ? ''
      : ` on ${broad.length} of ${metas.length} machines`;
    return {
      text: `Nothing matched that exact phrasing${scope}, so this is a widened search — expect results that are related rather than exact.`,
      query: shown,
      relaxed: true
    };
  }
  if (shown) {
    return {
      text: 'Search ran a trimmed version of what you typed.',
      query: shown,
      relaxed: false
    };
  }
  if (differing.length > 1) {
    return {
      text: 'Your machines read this query differently, so their results are not directly comparable.',
      query: null,
      relaxed: false
    };
  }
  return null;
}

function normalizeQueryText(value: string): string {
  return value.trim().replace(/\s+/g, ' ').toLocaleLowerCase();
}

// How long the subject was live in a conversation. Both ends come from the
// rollup, so this covers every hit and not only the ones on the page.
function formatHitSpan(first: string | null, last: string | null): { label: string; title: string } | null {
  const start = first && timestampValue(first) ? first : null;
  const end = last && timestampValue(last) ? last : null;
  if (!start && !end) return null;
  const startDay = start ? calendarDay(start) : '';
  const endDay = end ? calendarDay(end) : '';
  if (!start || !end || startDay === endDay) {
    const only = (end ?? start) as string;
    return { label: relativeDate(only), title: `Matched here on ${relativeDate(only)}` };
  }
  return {
    label: `${shortDate(start)} → ${shortDate(end)}`,
    title: `Matches here run from ${relativeDate(start)} to ${relativeDate(end)}`
  };
}

function calendarDay(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toDateString();
}

function shortDate(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
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

