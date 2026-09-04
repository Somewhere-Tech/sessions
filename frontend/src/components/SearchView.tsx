import { lazy, Suspense, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import {
  fetchServerHistory,
  fetchServerHistoryTranscript,
  fetchServerResumableSessions,
  listServerSessions,
  planSmartSearch,
  searchServer,
  type HistoryTranscript,
  type ResumableSession,
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
  managedSourceSessionID,
  plural,
  type BrowseFilters,
  type ConversationRow,
  type ResumeTarget
} from '../lib/conversationBrowser';
import { serverDisplayName, useServers } from '../lib/servers';
import { isTauri } from '../lib/tauriBridge';
import { ConversationBrowser } from './ConversationBrowser';
import { ProviderBadge, normalizeProvider, type Provider } from './ProviderBadge';
import { normalizeTranscriptIndexes } from '../lib/searchTranscript';
import { SearchConversationCard, SearchRollupCard } from './SearchResultCards';
import type { SessionInfo } from '../types';

const ConversationReader = lazy(() => import('./SearchConversationReader').then((module) => ({ default: module.ConversationReader })));

type SearchMode = 'ai' | 'ranked';
type Speaker = 'user' | '' | 'assistant' | 'tool';
type Tool = '' | Provider;
type SortMode = 'relevance' | 'timeline';
type DateRange = 'all' | 'today' | '7d' | '30d';
export type Result = FleetSearchResult;

// The daemon's per-session rollup, tagged with the machine it came from. It
// counts every hit in the index, not the page of messages that came back, so
// it is the only thing on this screen that can answer "which session was that"
// for a session whose messages never reached the page.
export interface RollupSession extends SearchSessionHits {
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
export interface RollupSummary {
  hits: number;
  firstHitAt: string | null;
  lastHitAt: string | null;
  titleMatch: boolean;
}

type ResultRow =
  | { kind: 'group'; key: string; group: SearchConversationGroup; rollup: RollupSummary | null }
  | { kind: 'rollup'; key: string; entry: RollupSession };

export interface SelectedConversation {
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
      setContinuationError(reason instanceof Error ? reason.message : 'Could not open the continuation details');
    } finally {
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
    return <Suspense fallback={null}>
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
      </Suspense>;
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
                    row.group.primary.session_id
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

// Shared browse helpers live in lib/conversationBrowser.ts so the rows below
// the search box and the history browser resolve conversations identically.

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
