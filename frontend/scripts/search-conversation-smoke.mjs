// How search results become conversations: title adoption, provider identity,
// and grouping runs of the same chat into one card.
//
// This suite used to open with twenty-one `assert.match(<file contents>, /…/)`
// assertions over SearchView/App and close with two hand-copied-HTML CSS
// geometry scenarios. Both are gone: the first ran no product code, and the
// second measured markup pasted into page.setContent rather than anything
// SearchView renders. What is left bundles lib/searchConversations.ts and calls
// it, which is the part that was always real. Search's user-visible behaviour
// — type a query, get the conversation and the sentence back — is covered by
// tests/capability/search.test.tsx against a mounted SearchView.
import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { build } from 'esbuild';

const scratch = await mkdtemp(join(tmpdir(), 'sessions-search-smoke-'));
const output = join(scratch, 'search-conversations.mjs');
try {
  await build({
    entryPoints: ['src/lib/searchConversations.ts'],
    bundle: true,
    format: 'esm',
    outfile: output,
    platform: 'node',
    target: 'node20',
    logLevel: 'silent'
  });
  const {
    enrichSearchResultsWithHistory,
    enrichSearchResultsWithResumable,
    enrichSearchResultsWithSessions,
    groupSearchResults
  } = await import(`${pathToFileURL(output).href}?v=${Date.now()}`);
  const base = {
    name: 'Lakebuild product direction',
    tool: 'claude',
    role: 'user',
    timestamp: '2026-07-13T12:00:00Z',
    snippet: 'We should try [[Lakebuild]] later.',
    match_start: 14,
    match_end: 23,
    score: 1,
    cwd: '/Users/example/somewhere/tech',
    machine: 'Mac mini',
    serverId: 'mini',
    serverName: 'Mac mini'
  };
  const enriched = enrichSearchResultsWithSessions([
    { ...base, session_id: 'runtime-a', message_index: 8, message_id: 'message-a' },
    { ...base, session_id: 'runtime-b', provider_session_id: 'claude-conversation', message_index: 8, message_id: 'message-a' },
    { ...base, session_id: 'runtime-b', provider_session_id: 'claude-conversation', message_index: 19, message_id: 'message-b', snippet: 'The [[Lakebuild]] launcher should stay local.' },
    { ...base, session_id: 'runtime-c', provider_session_id: 'other-conversation', message_index: 2, message_id: 'message-c' }
  ], [
    {
      id: 'runtime-a',
      tool: 'claude-code',
      claudeSessionId: 'claude-conversation',
      name: 'Claude - somewhere web',
      claudeCustomTitle: 'BOLO'
    }
  ], 'bolo');
  assert.equal(enriched[0].title_match, true, 'a known chat title should become the strongest search result');
  assert.equal(enriched[0].name, 'BOLO');
  assert.equal(enriched[1].provider_session_id, 'claude-conversation', 'older hosts should hydrate provider identity from their session list');
  assert.equal(enriched[1].name, 'BOLO', 'the retained provider title should replace a generic history label');
  const groups = groupSearchResults(enriched.map((match) => ({
    ...match,
    serverId: match.serverId ?? 'mini',
    serverName: match.serverName ?? 'Mac mini'
  })));
  assert.equal(groups.length, 2, 'provider conversation identity should define cards');
  assert.equal(groups[0].matches.length, 3, 'title and unique messages should share one conversation card');
  assert.deepEqual(groups[0].sourceSessionIds, ['runtime-a', 'runtime-b']);
  assert.equal(groups[0].title, 'BOLO');

  const providerNative = enrichSearchResultsWithHistory([], [{
    id: 'provider:claude:native-bolo',
    name: 'BOLO',
    tool: 'claude',
    provider_session_id: 'native-bolo',
    cwd: '/Users/example/bolo',
    machine: 'Mac mini',
    created_at: Date.parse('2026-06-17T10:00:00Z'),
    last_activity_at: Date.parse('2026-06-17T10:00:00Z'),
    message_count: 20,
    conversation_available: true,
    external: true
  }], 'bolo');
  assert.equal(providerNative.length, 1, 'a provider-native title should be searchable without a Sessions runtime record');
  assert.equal(providerNative[0].name, 'BOLO');
  assert.equal(providerNative[0].score, 1_000_000);

  const fuzzyProviderTitle = enrichSearchResultsWithResumable([], [{
    sessionId: 'native-lakebuild',
    tool: 'claude',
    title: 'Lakebuild',
    historyId: 'provider:claude:native-lakebuild',
    cwd: '/Users/example/somewhere/tech/web',
    modifiedAt: Date.parse('2026-07-13T12:00:00Z'),
    firstUserMessage: 'Wait, who is sending the request?',
    sizeBytes: 1024
  }], 'lakebed', 'Mac mini');
  assert.equal(fuzzyProviderTitle.length, 1, 'a close remembered spelling should find a provider-native title');
  assert.equal(fuzzyProviderTitle[0].name, 'Lakebuild');
  assert.equal(fuzzyProviderTitle[0].title_match, true);

  const archivedGroups = groupSearchResults([{
    ...base,
    session_id: 'provider-history:claude:old-bolo',
    provider_session_id: 'old-bolo',
    name: 'what was the last thing we worked on',
    message_index: 12,
    message_id: 'prompt-history:old-bolo:12',
    snippet: 'okay we are building the [[BOLO]] app',
    serverId: 'mini',
    serverName: 'Mac mini'
  }]);
  assert.equal(
    archivedGroups[0].title,
    'what was the last thing we worked on',
    'prompt-only recovery cards should retain one stable first-request title instead of adopting the matching excerpt'
  );

} finally {
  await rm(scratch, { recursive: true, force: true });
}

console.log('search conversation smoke: ok');
