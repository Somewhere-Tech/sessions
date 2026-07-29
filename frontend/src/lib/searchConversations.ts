import type { HistorySession, ResumableSession, SearchMatch } from '../api/sessionsd';
import { providerConversationId } from './sessionStatus';
import type { SessionInfo } from '../types';

export interface FleetSearchResult extends SearchMatch {
  serverId: string;
  serverName: string;
}

export interface SearchConversationGroup {
  key: string;
  title: string;
  primary: FleetSearchResult;
  matches: FleetSearchResult[];
  sourceSessionIds: string[];
}

export function enrichSearchResultsWithSessions(
  matches: SearchMatch[],
  sessions: SessionInfo[],
  titleQuery = ''
): SearchMatch[] {
  const sessionByRuntime = new Map(sessions.map((session) => [session.id, session]));
  const sessionsByProvider = new Map<string, SessionInfo[]>();
  for (const session of sessions) {
    const providerID = providerConversationId(session);
    if (!providerID) continue;
    const values = sessionsByProvider.get(providerID) ?? [];
    values.push(session);
    sessionsByProvider.set(providerID, values);
  }

  const enriched = matches.map((match) => {
    const runtime = sessionByRuntime.get(match.session_id);
    const providerID = match.provider_session_id?.trim() || (runtime ? providerConversationId(runtime) : null);
    const title = bestSearchTitle(providerID ? sessionsByProvider.get(providerID) ?? [] : runtime ? [runtime] : []);
    return {
      ...match,
      ...(providerID ? { provider_session_id: providerID } : {}),
      ...(title ? { name: title } : {})
    };
  });

  const normalizedQuery = normalizeTitleQuery(titleQuery);
  if (!normalizedQuery) return enriched;

  const titleMatches: SearchMatch[] = [];
  const seenConversations = new Set<string>();
  const orderedSessions = [...sessions].sort((left, right) => sessionActivity(right) - sessionActivity(left));
  for (const session of orderedSessions) {
    const providerID = providerConversationId(session);
    const title = bestSearchTitle(providerID ? sessionsByProvider.get(providerID) ?? [session] : [session]);
    if (!providerID || !title) continue;
    const normalizedTitle = normalizeTitleQuery(title);
    if (!titleMatchesQuery(normalizedTitle, normalizedQuery)) continue;
    const tool = searchTool(session);
    const conversationKey = `${tool}:${providerID}`;
    if (seenConversations.has(conversationKey)) continue;
    seenConversations.add(conversationKey);
    const highlightedTitle = highlightTitle(title, normalizedQuery);
    titleMatches.push({
      session_id: session.id,
      provider_session_id: providerID,
      title_match: true,
      name: title,
      tool,
      role: 'user',
      timestamp: new Date(sessionActivity(session)).toISOString(),
      message_index: 0,
      message_id: `title:${providerID}`,
      snippet: highlightedTitle,
      match_start: 0,
      match_end: title.length,
      score: titleScore(normalizedTitle, normalizedQuery),
      cwd: session.cwd,
      machine: ''
    });
  }

  return [...titleMatches, ...enriched];
}

export function enrichSearchResultsWithHistory(
  matches: SearchMatch[],
  history: HistorySession[],
  titleQuery = ''
): SearchMatch[] {
  const byRuntime = new Map(history.map((session) => [session.id, session]));
  const byProvider = new Map<string, HistorySession>();
  for (const session of history) {
    if (!session.provider_session_id) continue;
    const current = byProvider.get(session.provider_session_id);
    if (!current || session.last_activity_at > current.last_activity_at) {
      byProvider.set(session.provider_session_id, session);
    }
  }
  const enriched = matches.map((match) => {
    const source = byRuntime.get(match.session_id)
      ?? (match.provider_session_id ? byProvider.get(match.provider_session_id) : undefined);
    return source ? {
      ...match,
      name: source.name || match.name,
      provider_session_id: source.provider_session_id || match.provider_session_id,
      machine: source.machine || match.machine
    } : match;
  });
  const normalizedQuery = normalizeTitleQuery(titleQuery);
  if (!normalizedQuery) return enriched;
  const titleMatches: SearchMatch[] = [];
  const seen = new Set<string>();
  for (const session of [...history].sort((left, right) => right.last_activity_at - left.last_activity_at)) {
    const normalizedTitle = normalizeTitleQuery(session.name);
    if (!titleMatchesQuery(normalizedTitle, normalizedQuery)) continue;
    const identity = `${session.tool}:${session.provider_session_id || session.id}`;
    if (seen.has(identity)) continue;
    seen.add(identity);
    titleMatches.push({
      session_id: session.id,
      provider_session_id: session.provider_session_id,
      title_match: true,
      name: session.name,
      tool: session.tool,
      role: 'user',
      timestamp: new Date(session.last_activity_at).toISOString(),
      message_index: 0,
      message_id: `title:${session.provider_session_id || session.id}`,
      snippet: highlightTitle(session.name, normalizedQuery),
      match_start: 0,
      match_end: session.name.length,
      score: titleScore(normalizedTitle, normalizedQuery),
      cwd: session.cwd,
      machine: session.machine
    });
  }
  return [...titleMatches, ...enriched];
}

export function enrichSearchResultsWithResumable(
  matches: SearchMatch[],
  resumable: ResumableSession[],
  titleQuery = '',
  machine = ''
): SearchMatch[] {
  return enrichSearchResultsWithHistory(matches, resumable.map((session) => ({
    id: session.historyId || `provider:${session.tool}:${session.sessionId}`,
    name: session.title?.trim() || session.firstUserMessage.trim() || session.sessionId,
    tool: session.tool,
    provider_session_id: session.sessionId,
    cwd: session.cwd,
    machine,
    created_at: session.modifiedAt,
    last_activity_at: session.modifiedAt,
    message_count: 0,
    conversation_available: !session.promptHistoryOnly,
    external: true,
    prompt_history_only: session.promptHistoryOnly
  })), titleQuery);
}

export function groupSearchResults(orderedResults: FleetSearchResult[]): SearchConversationGroup[] {
  const groups = new Map<string, {
    matches: FleetSearchResult[];
    sourceSessionIds: Set<string>;
    messageKeys: Set<string>;
  }>();

  for (const result of orderedResults) {
    const conversationID = result.provider_session_id?.trim() || result.session_id;
    const key = `${result.serverId}:${result.tool}:${conversationID}`;
    let group = groups.get(key);
    if (!group) {
      group = { matches: [], sourceSessionIds: new Set(), messageKeys: new Set() };
      groups.set(key, group);
    }
    group.sourceSessionIds.add(result.session_id);
    const messageKey = result.message_id
      ? `${result.role}:${result.message_id}`
      : `${result.session_id}:${result.message_index}:${result.snippet}`;
    if (group.messageKeys.has(messageKey)) continue;
    group.messageKeys.add(messageKey);
    group.matches.push(result);
  }

  return [...groups.entries()].map(([key, group]) => ({
    key,
    title: conversationTitle(group.matches),
    primary: group.matches[0],
    matches: group.matches,
    sourceSessionIds: [...group.sourceSessionIds]
  }));
}

export function plainSearchSnippet(value: string): string {
  return value
    .replaceAll('[[', '')
    .replaceAll(']]', '')
    .replace(/^…\s*/, '')
    .replace(/\s*…$/, '')
    .replace(/\s+/g, ' ')
    .trim();
}

function conversationTitle(matches: FleetSearchResult[]): string {
  // A search hit is evidence inside a conversation, not the conversation's
  // identity. Keep the headline stable as the user changes query: provider
  // title first, then the deterministic local title supplied by history.
  // Prompt-index-only records use their first request as that local title.
  // Never replace it with whichever later prompt happened to match.
  const named = matches
    .map((match) => match.name.trim())
    .find((name) => name && !isGenericHistoryName(name));
  if (named) return shortenTitle(named);

  const said = matches.find((match) => match.role === 'user');
  const excerpt = plainSearchSnippet((said ?? matches[0])?.snippet ?? '');
  if (excerpt) return shortenTitle(excerpt);

  const fallback = matches[0]?.name.trim();
  if (fallback) return shortenTitle(fallback);
  return 'Saved conversation';
}

export function searchSessionTitle(session: SessionInfo): string {
  const candidates = [
    session.claudeCustomTitle,
    isGenericHistoryName(session.name ?? '') ? '' : session.name,
    session.claudeAiTitle,
    session.description
  ];
  const selected = candidates.find((value) => value?.trim());
  return selected ? shortenTitle(selected) : '';
}

function bestSearchTitle(sessions: SessionInfo[]): string {
  return [...sessions]
    .sort((left, right) => sessionActivity(right) - sessionActivity(left))
    .map(searchSessionTitle)
    .find(Boolean) ?? '';
}

function isGenericHistoryName(value: string): boolean {
  return /^(?:claude|codex|shell)\s*[-—]\s+/i.test(value)
    || /^[0-9a-f]{8}(?:-[0-9a-f-]+)?$/i.test(value);
}

function searchTool(session: SessionInfo): SearchMatch['tool'] {
  if (session.tool === 'claude-code') return 'claude';
  if (session.tool === 'codex') return 'codex';
  return 'shell';
}

function sessionActivity(session: SessionInfo): number {
  return session.exitedAt ?? session.lastDataAt ?? session.createdAt ?? 0;
}

function normalizeTitleQuery(value: string): string {
  return value
    .trim()
    .replace(/^["']|["']$/g, '')
    .replace(/\s+/g, ' ')
    .toLocaleLowerCase();
}

function highlightTitle(title: string, query: string): string {
  const start = title.toLocaleLowerCase().indexOf(query);
  if (start < 0) return title;
  return `${title.slice(0, start)}[[${title.slice(start, start + query.length)}]]${title.slice(start + query.length)}`;
}

function titleScore(title: string, query: string): number {
  if (title === query) return 1_000_000;
  if (title.startsWith(query)) return 900_000;
  if (title.includes(query)) return 800_000;
  return 700_000 + Math.max(0, 10_000 - editDistance(title, query) * 1_000);
}

function titleMatchesQuery(title: string, query: string): boolean {
  if (title.includes(query)) return true;
  const queryTokens = titleTokens(query);
  const titleValues = titleTokens(title);
  if (queryTokens.length === 0 || titleValues.length === 0) return false;
  return queryTokens.every((queryToken) => titleValues.some((titleToken) => fuzzyTitleTokenMatch(titleToken, queryToken)));
}

function titleTokens(value: string): string[] {
  return value.split(/[^\p{L}\p{N}]+/u).filter(Boolean);
}

function fuzzyTitleTokenMatch(titleToken: string, queryToken: string): boolean {
  if (titleToken.includes(queryToken) || queryToken.includes(titleToken)) return true;
  if (Math.min(titleToken.length, queryToken.length) < 5) return false;
  const sharedPrefix = commonPrefixLength(titleToken, queryToken);
  const shorter = Math.min(titleToken.length, queryToken.length);
  if (sharedPrefix >= 5 && sharedPrefix / shorter >= 0.6) return true;
  const allowedEdits = Math.min(3, Math.max(1, Math.ceil(shorter * 0.34)));
  return editDistance(titleToken, queryToken, allowedEdits) <= allowedEdits;
}

function commonPrefixLength(left: string, right: string): number {
  const limit = Math.min(left.length, right.length);
  let index = 0;
  while (index < limit && left[index] === right[index]) index += 1;
  return index;
}

function editDistance(left: string, right: string, stopAfter = Number.MAX_SAFE_INTEGER): number {
  if (Math.abs(left.length - right.length) > stopAfter) return stopAfter + 1;
  let previous = Array.from({ length: right.length + 1 }, (_, index) => index);
  for (let leftIndex = 1; leftIndex <= left.length; leftIndex += 1) {
    const current = [leftIndex];
    for (let rightIndex = 1; rightIndex <= right.length; rightIndex += 1) {
      const substitution = previous[rightIndex - 1]! + (left[leftIndex - 1] === right[rightIndex - 1] ? 0 : 1);
      const value = Math.min(
        current[rightIndex - 1]! + 1,
        previous[rightIndex]! + 1,
        substitution
      );
      current.push(value);
    }
    previous = current;
  }
  return previous[right.length]!;
}

function shortenTitle(value: string): string {
  const clean = value.replace(/\s+/g, ' ').trim();
  const runes = [...clean];
  if (runes.length <= 92) return clean;
  return `${runes.slice(0, 91).join('').trim()}…`;
}
