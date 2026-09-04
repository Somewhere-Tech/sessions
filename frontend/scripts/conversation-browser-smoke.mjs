// The app must be able to answer "which conversation was that" for every
// conversation on the machine, not only the ones Sessions started — asserted by
// rendering the real SearchView, not by reading its source.
//
// Each scenario below is a way this screen can send someone to a dead end:
//
//   1. showing only Sessions' own sessions, so the conversation you had in a
//      terminal three folders away is simply not there;
//   2. offering Resume on a conversation the daemon will refuse because it is
//      live right now (session.ConversationLiveError);
//   3. hiding a conversation that only survives in Sessions' own copy, or
//      offering a button on one that is genuinely gone;
//   4. ordering by the Sessions record's activity stamp, so a shutdown sweep
//      floats long-dead conversations above yesterday's work;
//   5. printing counts as totals while a machine in the fleet never answered;
//   6. previewing by resuming — creating a runtime just to look.
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

const t = smoke('conversation-browser');
const work = await mkdtemp(join(tmpdir(), 'sessions-conversation-browser-'));
const publicDir = fileURLToPath(new URL('../public/', import.meta.url));
const screenshot = process.env.CONVERSATION_BROWSER_SCREENSHOT || join(work, 'conversation-browser.png');
let browser;
let server;


try {
  await build({
    entryPoints: [fileURLToPath(new URL('./conversation-browser-fixture.tsx', import.meta.url))],
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
localStorage.setItem('sessions:servers', JSON.stringify([
  {id:'fixture',name:'Fixture Mac',host:'127.0.0.1',port:8787,isDefault:true},
  {id:'studio',name:'Studio',host:'127.0.0.1',port:8788}
]));
localStorage.setItem('sessions:active-server','fixture');
localStorage.removeItem('sessions:search-state:v3');
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
  await page.setViewport({ width: 1440, height: 1400, deviceScaleFactor: 1 });
  const pageErrors = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  t.watch(page);
  await page.goto(`http://127.0.0.1:${address.port}`, { waitUntil: 'domcontentloaded' });

  // ── 1. Browsing is the default state of the surface ─────────────────────
  // No query typed, nothing clicked: the conversations are already there.
  t.scenario('browsing is the default state: conversations are listed with nothing typed');
  await t.waitForSelector(
    page,
    '#surface-search .conversation-browser .search-result-card',
    'the browser to list conversations with no query typed and nothing clicked'
  );

  const shot = async (name) => {
    if (!process.env.CONVERSATION_BROWSER_SCREENSHOT) return;
    await page.screenshot({ path: screenshot.replace(/\.png$/, `-${name}.png`), fullPage: true, captureBeyondViewport: false });
  };

  const readScreen = () => page.evaluate(() => {
    const text = (node) => (node?.textContent ?? '').replace(/\s+/g, ' ').trim();
    const rows = [];
    for (const card of document.querySelectorAll('#surface-search .conversation-browser .search-result-card')) {
      rows.push({
        title: text(card.querySelector('.search-result-source strong')),
        badges: Array.from(card.querySelectorAll('.search-title-match, .conversation-row-tag'), (node) => text(node)),
        meta: text(card.querySelector('.search-conversation-match-count')),
        location: text(card.querySelector('.search-result-location')),
        path: text(card.querySelector('.search-result-footer code')),
        actions: Array.from(card.querySelectorAll('.search-result-actions button'), (button) => text(button)),
        note: text(card.querySelector('.conversation-row-note')),
        preview: Array.from(card.querySelectorAll('.conversation-preview-line'), (line) => text(line)),
        previewNote: text(card.querySelector('.conversation-preview-note'))
      });
    }
    return {
      rows,
      order: rows.map((row) => row.title),
      notices: Array.from(document.querySelectorAll('#surface-search .search-notice'), (notice) => text(notice)),
      foot: text(document.querySelector('#surface-search .conversation-browser-foot')),
      body: text(document.querySelector('#surface-search'))
    };
  });

  const rowByTitle = (screen, title) => screen.rows.find((row) => row.title === title);

  const first = await readScreen();
  await shot('browse');

  // Conversations Sessions never started are listed alongside the ones it did.
  assert.ok(rowByTitle(first, 'Release notes for 0.2.16'), 'a conversation started outside Sessions must be browsable');
  assert.match(rowByTitle(first, 'Release notes for 0.2.16').location, /Started outside Sessions/);

  // ── 2. A row you can recognise a week later ─────────────────────────────
  const recognisable = rowByTitle(first, 'Hardening sweep notes');
  assert.ok(recognisable, 'the Sessions-copy conversation must be listed');
  assert.match(recognisable.meta, /\d+:\d\d/, 'a row must say when it was last spoken in');
  assert.match(recognisable.meta, /ago/, 'and how long ago that was');
  assert.match(recognisable.meta, /233 messages/, 'and how big it is');
  assert.equal(recognisable.path, '~/somewhere/tech/hardening', 'and which folder it happened in');
  assert.match(recognisable.location, /Claude/, 'and which provider it belongs to');

  // ── 3. Ordering is by the conversation, not by the record ───────────────
  // 'Old drafts thread' has the most recent last_activity_at on the machine
  // (a shutdown sweep touched it an hour ago) and the oldest transcript. If it
  // leads the list, the browse is sorted on housekeeping.
  assert.notEqual(first.order[0], 'Old drafts thread', 'a record touched by housekeeping must not outrank a real conversation');
  assert.equal(first.order[0], 'Cutover rehearsal', 'the most recently spoken-in conversation leads');
  assert.ok(
    first.order.indexOf('Old drafts thread') > first.order.indexOf('Release notes for 0.2.16'),
    'ordering must follow conversation_updated_at, not last_activity_at'
  );

  // ── 4. Live conversations offer attach, never resume ────────────────────
  const liveByID = rowByTitle(first, 'Cutover rehearsal');
  assert.deepEqual(liveByID.badges, ['Live now']);
  assert.ok(!liveByID.actions.includes('Resume conversation'), 'a live conversation must not offer resume; the daemon refuses it');
  assert.ok(liveByID.actions.includes('Open the live session'), 'it must offer the thing that does work');
  assert.match(liveByID.note, /running right now/);

  // Bound through the provider UUID only — the id comparison alone misses it.
  const liveByProvider = rowByTitle(first, 'Quota calculator with Codex');
  assert.deepEqual(liveByProvider.badges, ['Live now'], 'a conversation held by a differently-named session is still live');
  assert.ok(!liveByProvider.actions.includes('Resume conversation'), 'and it must not offer resume either');

  // An exited session holding the same provider UUID is not a live binding.
  const native = rowByTitle(first, 'Release notes for 0.2.16');
  assert.ok(native.actions.includes('Resume conversation'), 'an ended runtime must not suppress resume');
  assert.equal(first.rows.filter((row) => row.title === 'Release notes for 0.2.16').length, 1,
    'copies of one provider conversation must share one card');
  assert.ok(native.badges.includes('2 copies'));
  assert.match(native.note, /Why there are copies/);
  assert.match(native.note, /Resume uses the newest/);

  // ── 5. Moved conversations point at the machine that has them ───────────
  const moved = rowByTitle(first, 'Fleet migration plan');
  assert.ok(!moved.actions.includes('Resume conversation'), 'a conversation that moved must not offer a resume that forks it');
  assert.match(moved.note, /continued on studio\.tail-scale\.ts/i);
  assert.match(moved.note, /fork/, 'and says what resuming here would do instead');

  // ── 6. Honest partials ──────────────────────────────────────────────────
  const partialNotice = first.notices.find((notice) => notice.includes('did not answer'));
  assert.ok(partialNotice, 'a machine that did not answer must be named, not omitted');
  assert.match(partialNotice, /more history than this/);
  assert.match(first.foot, /at least/, 'counts are lower bounds while a machine is missing');
  const unreadableNotice = first.notices.find((notice) => notice.includes('could not be read'));
  assert.ok(unreadableNotice, 'rows the daemon could not read must be counted out loud');

  // ── 7. The default cut is visible, and reversible ───────────────────────
  // 26 conversations survive the default filters; twenty are shown.
  assert.equal(first.rows.length, 20, 'a browse shows a page, not three hundred rows');
  assert.match(first.foot, /at least 26 conversations · showing the 20 most recent/);
  assert.match(first.foot, /4 hidden as empty, shell-only, or unrecoverable/);
  assert.ok(!first.order.includes('Deleted experiment'), 'an unrecoverable conversation is not in the default list');
  assert.ok(!first.order.includes('build logs'), 'a shell lane is not a conversation');
  assert.ok(!first.order.includes('Never used'), 'an empty shell has nothing to recognise');

  await page.$$eval('#surface-search .conversation-browser-foot-actions button', (buttons) => {
    buttons.find((button) => button.textContent.includes('Show 6 more'))?.click();
  });
  await t.waitForFunction(
    page,
    () => document.querySelectorAll('#surface-search .conversation-browser .search-result-card').length === 26,
    '"Show 6 more" to extend the page from 20 rows to all 26 that survive the default filters'
  );

  await page.$$eval('#surface-search .conversation-browser-foot-actions button', (buttons) => {
    buttons.find((button) => button.textContent.includes('Show everything'))?.click();
  });
  await t.waitForFunction(
    page,
    () => Boolean(
      Array.from(document.querySelectorAll('#surface-search .search-result-source strong'))
        .find((node) => node.textContent.trim() === 'Deleted experiment')
    ),
    '"Show everything" to reveal the unrecoverable conversations the default cut hides'
  );
  const everything = await readScreen();
  await shot('everything');

  // ── 8. Gone means gone, and says so ─────────────────────────────────────
  const gone = rowByTitle(everything, 'Deleted experiment');
  assert.deepEqual(gone.actions, [], 'a conversation nothing holds must offer no action at all');
  assert.match(gone.note, /Neither the provider nor Sessions still holds this conversation/);

  const torn = rowByTitle(everything, 'Torn transcript');
  assert.deepEqual(torn.actions, [], 'an unreadable conversation must not offer a preview that 404s');
  assert.match(torn.note, /permission denied reading the Codex rollout file/, 'it must say what actually failed');

  await page.$$eval('#surface-search .conversation-browser-foot-actions button', (buttons) => {
    buttons.find((button) => button.textContent.includes('Show conversations only'))?.click();
  });
  await t.waitForFunction(
    page,
    () => document.querySelectorAll('#surface-search .conversation-browser .search-result-card').length === 20,
    '"Show conversations only" to restore the default 20-row cut'
  );

  // ── 9. Preview reads; it does not resume ────────────────────────────────
  await page.$$eval('#surface-search .conversation-browser .search-result-card', (cards) => {
    const card = cards.find((node) => node.textContent.includes('Hardening sweep notes'));
    Array.from(card.querySelectorAll('.search-result-actions button'))
      .find((button) => button.textContent.trim() === 'Preview')?.click();
  });
  t.scenario('preview reads the conversation tail; it must not resume or attach');
  await t.waitForSelector(page, '#surface-search .conversation-preview-line', 'Preview to render the tail of the conversation inline');
  const previewed = await readScreen();
  await shot('preview');
  const preview = rowByTitle(previewed, 'Hardening sweep notes').preview;
  assert.equal(preview.length, 4, 'a preview is the tail of the conversation, not the whole thing');
  assert.match(preview[preview.length - 1], /Ship it after the smoke suite passes/, 'and it ends at the last thing said');
  assert.ok(
    !preview.some((line) => line.includes('Read(frontend/src/App.tsx)')),
    'tool traffic is not what a person reads a conversation back for'
  );

  const afterPreview = await page.evaluate(() => ({
    actions: window.__actions ?? [],
    writes: (window.__fetchLog ?? []).filter((entry) => !entry.startsWith('GET ')),
    preview: (window.__fetchLog ?? []).filter((entry) => entry.includes('/preview'))
  }));
  assert.deepEqual(afterPreview.writes, [], 'previewing must not create anything');
  assert.deepEqual(afterPreview.actions, [], 'previewing must not resume or attach');
  assert.ok(afterPreview.preview.length > 0, 'the preview must come from the tail-bounded preview route');

  // ── 10. Resume carries the handle that actually works ───────────────────
  const clickResume = (title) => page.$$eval('#surface-search .conversation-browser .search-result-card', (cards, wanted) => {
    const card = cards.find((node) => node.textContent.includes(wanted));
    Array.from(card.querySelectorAll('.search-result-actions button'))
      .find((button) => button.textContent.trim().startsWith('Resume'))?.click();
  }, title);

  // A conversation that survives only in Sessions' own copy: no provider
  // handle, so it must be addressed by its history id or it cannot come back.
  t.scenario('resume carries the handle that actually works for each kind of conversation');
  await clickResume('Hardening sweep notes');
  await t.waitForFunction(page, () => (window.__actions ?? []).length === 1, 'Resume on the Sessions-copy conversation to dispatch exactly one action');
  const [sessionsCopyResume] = await page.evaluate(() => window.__actions);
  assert.equal(sessionsCopyResume, 'resume fixture provider=sessions-copy source=sessions-copy history=sessions-copy');

  await page.reload({ waitUntil: 'domcontentloaded' });
  await t.waitForSelector(page, '#surface-search .conversation-browser .search-result-card', 'the browser to relist conversations after a reload');
  await clickResume('Release notes for 0.2.16');
  await t.waitForFunction(page, () => (window.__actions ?? []).length === 1, 'Resume on the provider-native conversation to dispatch exactly one action');
  const [nativeResume] = await page.evaluate(() => window.__actions);
  assert.equal(
    nativeResume,
    'resume fixture provider=uuid-native source=native history=-',
    'a copied provider conversation resumes from its newest Sessions runtime'
  );

  // Attaching goes to the live session, not to a second runtime on top of it.
  await page.reload({ waitUntil: 'domcontentloaded' });
  await t.waitForSelector(page, '#surface-search .conversation-browser .search-result-card', 'the browser to relist conversations after a reload');
  await page.$$eval('#surface-search .conversation-browser .search-result-card', (cards) => {
    const card = cards.find((node) => node.textContent.includes('Quota calculator with Codex'));
    Array.from(card.querySelectorAll('.search-result-actions button'))
      .find((button) => button.textContent.trim() === 'Open the live session')?.click();
  });
  await t.waitForFunction(page, () => (window.__actions ?? []).length === 1, '"Open the live session" to dispatch exactly one attach action');
  const [attach] = await page.evaluate(() => window.__actions);
  assert.equal(attach, 'attach fixture lane-99', 'attach must target the session that holds the conversation');

  // ── 11. Opening a browsed conversation is read-only, and honest about it ─
  t.scenario('opening a browsed conversation is read-only and calls nothing a match');
  await page.reload({ waitUntil: 'domcontentloaded' });
  await t.waitForSelector(page, '#surface-search .conversation-browser .search-result-card', 'the browser to relist conversations after a reload');
  await page.$$eval('#surface-search .conversation-browser .search-result-card', (cards) => {
    const card = cards.find((node) => node.textContent.includes('Hardening sweep notes'));
    Array.from(card.querySelectorAll('.search-result-actions button'))
      .find((button) => button.textContent.trim().startsWith('Open conversation'))?.click();
  });
  await t.waitForSelector(page, '#surface-search .search-conversation-view', 'the browsed conversation to open into the transcript reader');
  await t.waitForFunction(page, () => document.querySelectorAll('#surface-search .search-transcript-message').length > 0, 'the transcript to load at least one message');
  const reader = await page.evaluate(() => ({
    kicker: (document.querySelector('#surface-search .search-conversation-kicker')?.textContent ?? '').replace(/\s+/g, ' ').trim(),
    title: (document.querySelector('#surface-search .search-conversation-heading h1')?.textContent ?? '').trim(),
    markers: document.querySelectorAll('#surface-search .search-match-marker').length
  }));
  assert.equal(reader.title, 'Hardening sweep notes');
  assert.match(reader.kicker, /opened from the start/);
  assert.doesNotMatch(reader.kicker, /match/i, 'nothing was searched for, so nothing may be called a match');
  assert.equal(reader.markers, 0);

  // ── 12. Filters narrow the browse, and an empty answer stays honest ─────
  t.scenario('filters narrow the browse, and an empty answer still says what exists behind it');
  await page.$eval('#surface-search .search-back', (element) => element.click());
  await t.waitForSelector(page, '#surface-search .conversation-browser', 'the back control to return to the conversation browser');
  await page.$$eval('#surface-search .search-filter-group button', (buttons) => {
    buttons.find((button) => button.textContent.includes('Codex'))?.click();
  });
  await t.waitForFunction(
    page,
    () => document.querySelectorAll('#surface-search .conversation-browser .search-result-card').length === 2,
    'the Codex provider filter to narrow the browse to its two conversations'
  );
  const codexOnly = await readScreen();
  assert.deepEqual(codexOnly.order, ['Quota calculator with Codex', 'Release notes for 0.2.16']);

  await page.$eval('#surface-search .search-more-filters', (element) => element.click());
  await t.waitForSelector(page, '#surface-search .search-advanced-filters input[placeholder^="~/"]', 'the advanced filter panel to expose its workspace input');
  await page.type('#surface-search .search-advanced-filters input[placeholder^="~/"]', '/no/such/folder');
  // A workspace nothing was recorded under must empty the list. If the filter
  // is not applied at all this wait is what fails.
  await t.waitForFunction(
    page,
    () => Boolean(document.querySelector('#surface-search .conversation-browser .usage-empty')),
    'a workspace nothing was recorded under to empty the list — if this times out the filter is not being applied at all',
    { timeout: 5_000 }
  );
  const empty = await readScreen();
  await shot('empty');
  assert.match(empty.body, /No conversation matches these filters/);
  assert.match(empty.body, /30 conversations are recorded/, 'an empty answer must still say what exists behind the filters');

  assert.doesNotMatch(first.body, /undefined|NaN|\[object Object\]/, 'missing fields must not reach the screen');
  assert.deepEqual(pageErrors, []);
  if (process.env.CONVERSATION_BROWSER_SCREENSHOT) {
    await page.screenshot({ path: screenshot, fullPage: true, captureBeyondViewport: false });
  }
  t.pass(`conversation-browser smoke passed${process.env.CONVERSATION_BROWSER_SCREENSHOT ? `: ${screenshot}` : ''}`);
} finally {
  t.release();
  // Both of these are deadline-bounded so a wedged browser or an unreleased
  // socket fails instead of hanging — and the deadline timers are unref'd, so
  // a clean shutdown exits immediately instead of paying the deadline. The
  // straightforward `Promise.race([close(), delay(3_000)])` this replaces was
  // costing three seconds on every passing run of every suite that used it.
  await closeBrowser(browser, 3_000);
  await closeServer(server, 3_000);
  if (!process.env.CONVERSATION_BROWSER_SCREENSHOT) await rm(work, { recursive: true, force: true });
}
