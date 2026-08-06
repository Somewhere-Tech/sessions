// Search must say which session, not only which message — asserted by
// rendering the real SearchView, not by reading its source.
//
// The daemon now returns a per-session rollup whose counts cover the whole
// index, the expression it actually ran, and whether it had to relax that
// expression. Each scenario below is a way this screen can lie to someone
// looking for a conversation they lost:
//
//   1. counting only the messages that fitted on the page, and dropping the
//      sessions that did not fit at all;
//   2. showing relaxed results as if they answered what was typed;
//   3. printing a truncated count as a complete one;
//   4. assuming a daemon that predates all of the above sends any of it.
//
// Harness: the esbuild → static server → puppeteer path from
// scripts/surface-truth-smoke.mjs. No new dependency.
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { extname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { build } from 'esbuild';
import puppeteer from 'puppeteer';
import { smoke, closeBrowser, closeServer } from './lib/smoke.mjs';

const t = smoke('search-rollup');
const work = await mkdtemp(join(tmpdir(), 'sessions-search-rollup-'));
const publicDir = fileURLToPath(new URL('../public/', import.meta.url));
const screenshot = process.env.SEARCH_ROLLUP_SCREENSHOT || join(work, 'search-rollup.png');
let browser;
let server;


try {
  await build({
    entryPoints: [fileURLToPath(new URL('./search-rollup-fixture.tsx', import.meta.url))],
    outdir: work,
    bundle: true,
    platform: 'browser',
    format: 'esm',
    define: { 'import.meta.env.BASE_URL': '"/"' },
    entryNames: 'app',
    assetNames: 'asset-[hash]',
    external: ['/claude-icon.svg'],
    loader: { '.svg': 'dataurl', '.png': 'dataurl' },
    logLevel: 'silent'
  });
  await writeFile(join(work, 'index.html'), `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<link rel="stylesheet" href="/app.css"></head><body><div id="root"></div>
<script>
localStorage.setItem('sessions:servers', JSON.stringify([{id:'fixture',name:'Fixture Mac',host:'127.0.0.1',port:8787,isDefault:true}]));
localStorage.setItem('sessions:active-server','fixture');
</script>
<script type="module" src="/app.js"></script></body></html>`);

  server = createServer(async (request, response) => {
    const name = request.url === '/' ? 'index.html' : request.url.slice(1);
    try {
      const source = name === 'openai-icon.svg' || name === 'claude-icon.svg'
        ? join(publicDir, name)
        : join(work, name);
      const body = await readFile(source);
      const type = extname(name) === '.css'
        ? 'text/css'
        : extname(name) === '.js'
          ? 'text/javascript'
          : extname(name) === '.svg'
            ? 'image/svg+xml'
            : extname(name) === '.png'
              ? 'image/png'
              : 'text/html';
      response.writeHead(200, { 'content-type': type });
      response.end(body);
    } catch {
      response.writeHead(404);
      response.end();
    }
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const address = server.address();
  if (!address || typeof address === 'string') throw new Error('fixture server did not bind');

  browser = await puppeteer.launch({ headless: true, args: ['--no-sandbox'] });
  const page = await browser.newPage();
  page.setDefaultTimeout(15_000);
  page.setDefaultNavigationTimeout(15_000);
  await page.setViewport({ width: 1440, height: 1200, deviceScaleFactor: 1 });
  const pageErrors = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  t.watch(page);
  await page.goto(`http://127.0.0.1:${address.port}`, { waitUntil: 'domcontentloaded' });
  t.scenario('the search surface mounts');
  await t.waitForSelector(page, '#surface-search .search-query-row input', 'the search box to mount');

  // Type it and press Search, the way a person does.
  const search = async (text) => {
    const input = await page.$('#surface-search .search-query-row input');
    await input.click({ clickCount: 3 });
    await page.keyboard.type(text);
    await page.click('#surface-search .search-ai-submit');
    // The view goes busy synchronously on submit and leaves busy in the same
    // commit that publishes the results, so this brackets exactly one search.
    // Both halves are named: "never went busy" means the submit never fired,
    // "never left busy" means the fixture daemon never answered. Those are
    // different bugs and used to produce the same message.
    await t.waitForFunction(
      page,
      () => Boolean(document.querySelector('#surface-search .search-progress')),
      `the search for "${text}" to enter its busy state after submit`
    );
    await t.waitForFunction(
      page,
      () => !document.querySelector('#surface-search .search-progress'),
      `the search for "${text}" to leave its busy state with results published`
    );
  };

  // Screenshots are opt-in; each scenario writes its own so the states can be
  // looked at side by side rather than only the last one.
  const shot = async (name) => {
    if (!process.env.SEARCH_ROLLUP_SCREENSHOT) return;
    await page.screenshot({ path: screenshot.replace(/\.png$/, `-${name}.png`), fullPage: true, captureBeyondViewport: false });
  };

  const readScreen = () => page.evaluate(() => {
    const text = (node) => (node?.textContent ?? '').replace(/\s+/g, ' ').trim();
    const cards = {};
    for (const card of document.querySelectorAll('#surface-search .search-result-card')) {
      const span = card.querySelector('.search-session-span');
      cards[text(card.querySelector('.search-result-source strong'))] = {
        count: text(card.querySelector('.search-conversation-match-count')),
        span: text(span),
        spanTitle: span?.getAttribute('title') ?? '',
        rollupOnly: card.className.includes('is-rollup-only'),
        actions: Array.from(card.querySelectorAll('.search-result-actions button'), (button) => text(button))
      };
    }
    return {
      summary: text(document.querySelector('#surface-search .search-result-count')),
      notices: Array.from(document.querySelectorAll('#surface-search .search-notice'), (notice) => ({
        relaxed: notice.className.includes('is-relaxed'),
        text: text(notice),
        code: text(notice.querySelector('code'))
      })),
      cards,
      body: text(document.querySelector('#surface-search'))
    };
  });

  // ── 1. A current daemon: the session rollup leads ───────────────────────
  t.scenario('a current daemon: the session rollup leads and counts the whole index');
  await search('rollout');
  const current = await readScreen();
  await shot('current');
  assert.deepEqual(
    Object.keys(current.cards).sort(),
    ['Drafts rollout plan', 'Mobile staged release', 'Rollout retro with Codex'],
    'every session the rollup counted must have a row, including the one with no message on the page'
  );

  // The page carried two of this session's messages. The session has nine
  // hits. Printing "2" here is how a user concludes a conversation only
  // mentioned the subject twice and stops looking.
  assert.equal(
    current.cards['Drafts rollout plan']?.count,
    '9 matching messages · 2 shown here',
    'a session card must count the session, and say how much of it reached the page'
  );
  // Nothing was hidden for this one, so there is nothing to explain.
  assert.equal(current.cards['Mobile staged release']?.count, '1 matching message');

  // The session with five hits and no messages on the page. Without the
  // rollup this conversation does not appear on the screen at all.
  const lost = current.cards['Rollout retro with Codex'];
  assert.ok(lost, 'a session the rollup counted must be listed even with no message on the page');
  assert.equal(lost.count, '5 matching messages · none shown here');
  assert.equal(lost.rollupOnly, true);
  assert.deepEqual(lost.actions, ['Open conversation →'], 'it must be openable');

  // How long the subject was live in the session, from the rollup's own
  // bracket rather than from whichever hit happened to reach the page.
  assert.match(
    current.cards['Drafts rollout plan'].span,
    /^\w{3} \d+ → \w{3} \d+$/,
    'a session whose hits span days must show that span'
  );
  assert.match(current.cards['Drafts rollout plan'].spanTitle, /run from .+ to /);
  assert.doesNotMatch(current.cards['Mobile staged release'].span, /→/, 'a single-day session must not fake a span');

  // The reach claim: 26 conversations and 288 matches exist behind the three
  // rows on screen, and the screen says so instead of implying three.
  assert.equal(current.summary, '26 conversations · 288 matches · 3 shown');

  // A strict search of exactly what was typed explains nothing.
  assert.deepEqual(current.notices, [], 'a strict search must not add chrome');

  // ── Opening a rollup-only session ───────────────────────────────────────
  // Search knows the session and the count, not where in it the hits are. It
  // must not point at message 1 and call it the match.
  await page.$$eval('#surface-search .search-result-card.is-rollup-only .search-result-actions button', (buttons) => {
    buttons[0]?.click();
  });
  await t.waitForSelector(page, '#surface-search .search-conversation-view', 'the rollup-only session to open into the transcript reader');
  await t.waitForFunction(
    page,
    () => document.querySelectorAll('#surface-search .search-transcript-message').length > 0,
    'the transcript to load at least one message from the start of the conversation'
  );
  const reader = await page.evaluate(() => ({
    kicker: (document.querySelector('#surface-search .search-conversation-kicker')?.textContent ?? '').replace(/\s+/g, ' ').trim(),
    title: (document.querySelector('#surface-search .search-conversation-heading h1')?.textContent ?? '').trim(),
    markers: document.querySelectorAll('#surface-search .search-match-marker').length,
    matched: document.querySelectorAll('#surface-search .search-transcript-message.is-match').length,
    transcriptRequests: (window.__fetchLog ?? []).filter((entry) => entry.startsWith('/api/history/'))
  }));
  assert.equal(reader.title, 'Rollout retro with Codex');
  assert.match(reader.kicker, /5 message matches/);
  assert.match(reader.kicker, /opened from the start/);
  assert.equal(reader.markers, 0, 'a session opened without an anchor must not label a message "Match"');
  assert.equal(reader.matched, 0);
  assert.ok(
    reader.transcriptRequests.some((entry) => entry.includes('sess-lost') && entry.includes('start=0') && !entry.includes('message_id')),
    'the transcript must be requested from the start, not around a guessed anchor'
  );
  await page.$eval('#surface-search .search-back', (element) => element.click());
  await t.waitForSelector(page, '#surface-search .search-results', 'the back control to return to the result list');

  // ── 2. The daemon relaxed the query ─────────────────────────────────────
  t.scenario('a daemon that relaxed the query says so, once, with the expression it ran');
  await search('how should the drafts rollout work');
  const relaxed = await readScreen();
  await shot('relaxed');
  assert.equal(relaxed.notices.length, 1, 'a widened search must say so, once');
  assert.equal(relaxed.notices[0].relaxed, true);
  assert.match(relaxed.notices[0].text, /exact phrasing/);
  assert.match(relaxed.notices[0].text, /widened/);
  assert.equal(
    relaxed.notices[0].code,
    'drafts OR rollout',
    'the expression that actually ran must be visible'
  );

  // ── 3. The rollup did not finish ────────────────────────────────────────
  t.scenario('a truncated rollup reads as a lower bound, not a complete count');
  await search('partial');
  const partial = await readScreen();
  await shot('partial');
  assert.equal(
    partial.summary,
    'at least 12 conversations · 31 matches · 1 shown',
    'a truncated rollup must not read as a complete count'
  );
  assert.equal(partial.cards['Index rebuild']?.count, 'at least 6 matching messages · 1 shown here');
  assert.equal(partial.notices.length, 1);
  assert.match(partial.notices[0].text, /lower bounds/);
  assert.equal(partial.notices[0].relaxed, false);

  // ── 4. A daemon older than the rollup ───────────────────────────────────
  // matches and total, nothing else. The screen must fall back to what it can
  // see, and must not print a count it was never given.
  t.scenario('a daemon older than the rollup renders matches without inventing rollup language');
  await search('legacy');
  const legacy = await readScreen();
  assert.deepEqual(Object.keys(legacy.cards), ['Legacy daemon chat'], 'an older daemon still renders its matches');
  assert.equal(legacy.cards['Legacy daemon chat']?.count, '2 matching messages');
  assert.equal(legacy.summary, '1 conversation');
  assert.deepEqual(legacy.notices, []);
  assert.doesNotMatch(legacy.body, /at least|shown here|none shown/, 'no rollup language without a rollup');
  assert.doesNotMatch(legacy.body, /undefined|NaN|\[object Object\]/, 'missing fields must not reach the screen');

  assert.deepEqual(pageErrors, []);
  if (process.env.SEARCH_ROLLUP_SCREENSHOT) {
    await page.screenshot({ path: screenshot, fullPage: true, captureBeyondViewport: false });
  }
  t.pass(`search-rollup smoke passed${process.env.SEARCH_ROLLUP_SCREENSHOT ? `: ${screenshot}` : ''}`);
} finally {
  t.release();
  // Both of these are deadline-bounded so a wedged browser or an unreleased
  // socket fails instead of hanging — and the deadline timers are unref'd, so
  // a clean shutdown exits immediately instead of paying the deadline. The
  // straightforward `Promise.race([close(), delay(3_000)])` this replaces was
  // costing three seconds on every passing run of every suite that used it.
  await closeBrowser(browser, 3_000);
  await closeServer(server, 3_000);
  if (!process.env.SEARCH_ROLLUP_SCREENSHOT) await rm(work, { recursive: true, force: true });
}
