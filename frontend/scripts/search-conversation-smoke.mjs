import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { build } from 'esbuild';
import puppeteer from 'puppeteer';

const source = async (path) => readFile(new URL(`../${path}`, import.meta.url), 'utf8');
const [searchView, searchAPI, app, styles] = await Promise.all([
  source('src/components/SearchView.tsx'),
  source('src/api/sessionsd.ts'),
  source('src/App.tsx'),
  source('src/styles/globals.css')
]);

assert.match(searchView, /groupSearchResults\(orderedResults\)/);
assert.match(searchView, />What you said<\/FilterButton>/);
assert.match(searchView, /'You said'/);
assert.match(searchView, /via Sessions/);
assert.doesNotMatch(searchView, /Your request/);
assert.doesNotMatch(searchView, /\['ai', 'ranked', 'exact', 'regex'\]/);
assert.doesNotMatch(searchView, /Enter a Go regular expression/);
assert.match(searchView, /Resume conversation/);
assert.match(searchView, /next === 'full'\) window = undefined/);
assert.match(searchView, /Jump to latest ↓/);
assert.match(searchView, /search-transcript-latest/);
assert.match(searchAPI, /provider_session_id\?: string/);
assert.match(app, /<SearchView[\s\S]*onResumeConversation=/);
// Continuing from Search goes through the shared adopt-then-repair helper —
// the same one ResumeDialog uses — so both entry points give the user the
// same answer about whether the history annotations finished.
assert.match(app, /adoptConversationWithRepair\(providerSessionId, sourceSessionId, historyId\)/);
assert.match(app, /setAdoptionNotice\(adoptionWarning\(adopted\)\)/);
assert.doesNotMatch(app, /console\.warn\('Sessions resumed the conversation/);
assert.match(app, /result\.transcriptRecovery[\s\S]*\? 'remote'[\s\S]*: 'terminal'/);
assert.doesNotMatch(app, /onResumeConversation=\{\(serverId,[\s\S]{0,240}setDialogOpen/);

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

  const browser = await puppeteer.launch({ headless: true });
  try {
    const page = await browser.newPage();
    for (const width of [960, 460]) {
      await page.setViewport({ width, height: 720 });
      await page.setContent(`
        <style>${styles}</style>
        <main class="search-view" style="width:${width}px;box-sizing:border-box">
          <section class="search-results">
            <article id="card" class="search-result-card is-claude">
              <span class="search-result-provider">●</span>
              <span class="search-result-body">
                <button class="search-result-main">
                  <span class="search-result-source"><strong>Lakebuild product direction with a useful human title</strong><span class="search-title-match">Title match</span></span>
                  <span class="search-conversation-match-count">3 matches in this conversation · continued across 2 Sessions runs</span>
                </button>
                <span class="search-conversation-matches">
                  <button class="search-conversation-match"><span>You said</span><span class="search-snippet">A long matching message that should remain inside the available conversation card width.</span></button>
                </span>
                <span class="search-result-footer"><span class="search-result-location">Mac mini · ~/somewhere/tech</span><span class="search-result-actions"><button>Open conversation →</button><button class="is-resume">Resume conversation</button></span></span>
              </span>
            </article>
          </section>
        </main>
      `);
      const geometry = await page.$eval('#card', (card) => ({
        overflow: card.scrollWidth - card.clientWidth,
        width: card.getBoundingClientRect().width
      }));
      assert.ok(geometry.overflow <= 0, `search card overflowed by ${geometry.overflow}px at ${width}px`);
      assert.ok(geometry.width <= width, `search card exceeded viewport at ${width}px`);
    }

    await page.setViewport({ width: 760, height: 640 });
    await page.setContent(`
      <style>${styles}</style>
      <main class="search-view search-conversation-view" style="width:760px;height:640px;box-sizing:border-box">
        <section class="search-shell search-reader-shell">
          <div class="search-reader-chrome">
            <button class="search-back">← Back to results</button>
            <header class="search-conversation-heading">
              <div><h1>Lakebuild</h1><p>Mac mini</p></div>
              <div class="search-conversation-actions"><button class="btn">Resume conversation</button></div>
            </header>
            <div class="search-reader-toolbar"><span>Full transcript</span><span class="search-reader-position"><span>1635 messages</span><button>Jump to latest ↓</button></span></div>
          </div>
          <div id="transcript" class="search-transcript">
            <article id="message" class="search-transcript-message is-assistant">
              <header><span>Claude #1635</span></header>
              <p>${'unbroken-provider-output-'.repeat(90)}</p>
            </article>
          </div>
        </section>
      </main>
    `);
    const readerGeometry = await page.evaluate(() => {
      const transcript = document.querySelector('#transcript');
      const message = document.querySelector('#message');
      const chrome = document.querySelector('.search-reader-chrome');
      return {
        transcriptOverflowX: transcript.scrollWidth - transcript.clientWidth,
        messageOverflow: message.scrollWidth - message.clientWidth,
        transcriptBottom: transcript.getBoundingClientRect().bottom,
        chromeTop: chrome.getBoundingClientRect().top,
        viewportHeight: window.innerHeight
      };
    });
    assert.ok(readerGeometry.transcriptOverflowX <= 0, `transcript overflowed horizontally by ${readerGeometry.transcriptOverflowX}px`);
    assert.ok(readerGeometry.messageOverflow <= 0, `message overflowed horizontally by ${readerGeometry.messageOverflow}px`);
    assert.ok(readerGeometry.transcriptBottom <= readerGeometry.viewportHeight, 'reader should keep its transcript inside the window');
    assert.ok(readerGeometry.chromeTop >= 0, 'reader controls should remain inside the visible window');
  } finally {
    await browser.close();
  }
} finally {
  await rm(scratch, { recursive: true, force: true });
}

console.log('search conversation smoke: ok');
