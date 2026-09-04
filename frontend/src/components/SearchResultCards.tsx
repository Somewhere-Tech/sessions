import {
  compactMachineName,
  compactPath,
  isPromptHistoryOnly,
  plural,
  relativeDate
} from '../lib/conversationBrowser';
import type { SearchConversationGroup } from '../lib/searchConversations';
import { formatHitSpan, operationLabel, rankedMatchLabel } from '../lib/searchFormatting';
import { ParserIcon } from './ParserIcon';
import { ProviderBadge, normalizeProvider, type Provider } from './ProviderBadge';
import type { Result, RollupSession, RollupSummary } from './SearchView';

function timestampValue(value: string | null): number {
  if (!value) return 0;
  const parsed = new Date(value).getTime();
  return Number.isNaN(parsed) ? 0 : parsed;
}

export function SearchConversationCard({
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
            {group.sourceSessionIds.length > 1 ? ` · opened ${group.sourceSessionIds.length} times in Sessions` : ''}
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
                {resumePending ? 'Opening details…' : 'Continue conversation…'}
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
export function SearchRollupCard({
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
