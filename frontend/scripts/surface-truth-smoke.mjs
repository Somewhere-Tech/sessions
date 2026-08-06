// One screen, one truth — asserted by rendering, not by reading source.
//
// Every other status test in this repo either imports lib/sessionStatus.ts
// and checks the classifier in isolation, or greps a component's source for a
// string. Neither can see two mounted components disagree, which is the bug
// this suite exists for. Here the real SessionNavigator, FleetView, HomeView
// and GridView are mounted together over one fixture list (no daemon: the
// fixture replaces fetch and WebSocket), and the assertions read what a user
// would actually see.
//
// Harness: the same esbuild → static server → puppeteer path
// scripts/structured-view-smoke.mjs already uses. No new dependency.
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { extname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { build } from 'esbuild';
import puppeteer from 'puppeteer';

const work = await mkdtemp(join(tmpdir(), 'sessions-surface-truth-'));
const publicDir = fileURLToPath(new URL('../public/', import.meta.url));
const screenshot = process.env.SURFACE_TRUTH_SCREENSHOT || join(work, 'surface-truth.png');
let browser;
let server;

const delay = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));

// Every state word the navigator, Fleet, Home and the grid can print for the
// fixture list. Keyed by session name so a surface's own ordering does not
// matter — only what it says about each session.
const EXPECTED_LABELS = {
  'Ended while asking': 'Ended',
  'Waiting on you': 'Needs you',
  'Missing one MCP server': 'Limited',
  'Running the build': 'Working',
  'Finished its turn': 'Finished'
};

try {
  await build({
    entryPoints: [fileURLToPath(new URL('./surface-truth-fixture.tsx', import.meta.url))],
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
localStorage.setItem('sessions:navigator-machine-scope','fixture');
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
  await page.setViewport({ width: 1440, height: 1000, deviceScaleFactor: 1 });
  const pageErrors = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  await page.goto(`http://127.0.0.1:${address.port}`, { waitUntil: 'domcontentloaded' });

  await page.waitForSelector('#surface-navigator .session-nav-row');
  await page.waitForSelector('#surface-home .home-session-list button');
  await page.waitForSelector('#surface-grid .grid-cell');
  await page.waitForSelector('#surface-fleet .fleet-session-row');

  // The navigator collapses Ended by default and Fleet hides history behind a
  // toggle. Both are user-reachable; open them so every surface is showing the
  // whole fixture list at the same moment.
  await page.$eval(
    '#surface-navigator div.session-tree-group-head > button',
    (element) => element.click()
  );
  await page.$eval('#surface-fleet .fleet-history-toggle input', (element) => element.click());
  await page.waitForFunction(
    () => document.querySelectorAll('#surface-fleet .fleet-session-row').length === 5
      && document.querySelectorAll('#surface-navigator .session-nav-row').length === 5
  );

  const readSurfaces = () => page.evaluate(() => {
    const text = (node) => (node?.textContent ?? '').replace(/\s+/g, ' ').trim();
    const pairs = (nodes, name, state) => Object.fromEntries(
      Array.from(nodes, (node) => [name(node), state(node)])
    );
    return {
      navigator: pairs(
        document.querySelectorAll('#surface-navigator .session-nav-row'),
        (row) => text(row.querySelector('.session-nav-title')),
        (row) => row.querySelector('.session-nav-status')?.getAttribute('title') ?? ''
      ),
      fleet: pairs(
        document.querySelectorAll('#surface-fleet .fleet-session-row'),
        (row) => text(row.querySelector('.fleet-session-name')),
        (row) => text(row.querySelector('.fleet-session-state'))
      ),
      home: pairs(
        document.querySelectorAll('#surface-home .home-session-list button'),
        (row) => text(row.querySelector('strong')),
        (row) => row.getAttribute('title') ?? ''
      ),
      grid: pairs(
        document.querySelectorAll('#surface-grid .grid-cell'),
        (cell) => text(cell.querySelector('.grid-cell-name')),
        // "Working · 5s", "Ended · 0" — the state word is the first field.
        (cell) => text(cell.querySelector('.grid-cell-status')).split(' · ')[0]
      ),
      counts: {
        // The navigator's filter badge and Home's tile are two numbers on one
        // screen that must describe the same set of sessions.
        navigatorNeedsYou: Number(
          (Array.from(document.querySelectorAll('#surface-navigator .session-filter-row button'))
            .map((button) => text(button))
            .find((label) => label.startsWith('Needs you')) ?? '')
            .replace('Needs you', '').trim() || 0
        ),
        homeNeedsYou: Number(text(
          Array.from(document.querySelectorAll('#surface-home .home-stat-grid button'))
            .find((button) => text(button).startsWith('Needs you'))
            ?.querySelector('strong')
        ))
      }
    };
  });

  const surfaces = await readSurfaces();

  // ── An exited session that is still carrying idleReason 'needs-input' ────
  // This is the record that read "Ended" in the navigator and "Needs you" in
  // Fleet while both were visible.
  for (const [surface, labels] of Object.entries(surfaces)) {
    if (surface === 'counts') continue;
    assert.equal(
      labels['Ended while asking'],
      'Ended',
      `${surface} must call an exited needs-input session "Ended"`
    );
  }

  // ── One fixture list, one status word per session, on every surface ──────
  for (const [surface, labels] of Object.entries(surfaces)) {
    if (surface === 'counts') continue;
    assert.deepEqual(
      labels,
      EXPECTED_LABELS,
      `${surface} disagreed with the shared status vocabulary`
    );
  }

  // ── A degraded session is not a question ────────────────────────────────
  // "Limited" everywhere, and counted by nobody's "needs you" number. Both
  // badges must also agree with each other: two counts on one screen may not
  // describe different sets.
  assert.equal(surfaces.counts.navigatorNeedsYou, 1);
  assert.equal(surfaces.counts.homeNeedsYou, 1);
  assert.equal(surfaces.counts.navigatorNeedsYou, surfaces.counts.homeNeedsYou);

  // The badge is only honest if the filter behind it selects the same set.
  await page.$$eval('#surface-navigator .session-filter-row button', (buttons) => {
    buttons.find((button) => (button.textContent ?? '').startsWith('Needs you'))?.click();
  });
  await page.waitForFunction(
    () => document.querySelectorAll('#surface-navigator .session-nav-row').length === 1
  );
  const needsRows = await page.$$eval(
    '#surface-navigator .session-nav-row .session-nav-title',
    (nodes) => nodes.map((node) => node.textContent?.trim() ?? '')
  );
  assert.deepEqual(needsRows, ['Waiting on you'], 'the needs-you filter must select the badge’s set');
  await page.$$eval('#surface-navigator .session-filter-row button', (buttons) => {
    buttons.find((button) => (button.textContent ?? '').trim() === 'All')?.click();
  });

  // ── A failed end must stay an open decision ─────────────────────────────
  // The fixture daemon refuses the DELETE. The confirmation must stay up, say
  // what the daemon said, and offer the retry — silently closing would tell
  // the user a runtime had stopped while it is still running.
  await page.$eval(
    '#surface-navigator .session-nav-row[data-session-id="busy-runtime"] .session-row-action-trigger',
    (element) => element.click()
  );
  await page.waitForSelector('[data-session-action-menu="busy-runtime"]');
  await page.$$eval('[data-session-action-menu="busy-runtime"] button', (buttons) => {
    buttons.find((button) => (button.textContent ?? '').startsWith('End session'))?.click();
  });
  await page.waitForSelector('.session-end-sheet [role="dialog"]');
  await page.$eval('.session-end-sheet .session-end-actions .btn-primary', (element) => element.click());
  await page.waitForSelector('.session-end-sheet .session-end-error');

  const endState = await page.evaluate(() => {
    const sheet = document.querySelector('.session-end-sheet');
    const error = sheet?.querySelector('.session-end-error') ?? null;
    const rect = error?.getBoundingClientRect();
    const topmost = rect
      ? document.elementFromPoint(rect.left + rect.width / 2, rect.top + rect.height / 2)
      : null;
    return {
      dialogOpen: Boolean(sheet?.querySelector('[role="dialog"]')),
      title: sheet?.querySelector('h2')?.textContent?.replace(/\s+/g, ' ').trim() ?? '',
      message: error?.textContent?.replace(/\s+/g, ' ').trim() ?? '',
      // Rendering the message somewhere behind the modal scrim is the same as
      // not rendering it, so check the user can actually see this element.
      messageOnTop: Boolean(error && topmost && (error === topmost || error.contains(topmost))),
      confirmLabel: sheet?.querySelector('.session-end-actions .btn-primary')?.textContent?.trim() ?? '',
      confirmEnabled: !sheet?.querySelector('.session-end-actions .btn-primary')?.disabled,
      keepRunning: Array.from(sheet?.querySelectorAll('.session-end-actions button') ?? [])
        .some((button) => (button.textContent ?? '').trim() === 'Keep running'),
      // The row is still live in the navigator behind the sheet.
      rowStillLive: document.querySelector('#surface-navigator .session-nav-row[data-session-id="busy-runtime"]')
        ?.className.includes('is-working') ?? false
    };
  });

  assert.equal(endState.dialogOpen, true, 'a failed end must leave the confirmation open');
  assert.match(endState.title, /Running the build/);
  assert.match(endState.message, /503/, 'the daemon’s own reply must reach the user');
  assert.match(endState.message, /runner for this session is not responding/);
  assert.equal(endState.messageOnTop, true, 'the failure message must be visible, not behind the sheet');
  assert.equal(endState.confirmLabel, 'Try again', 'a failed end must offer a retry');
  assert.equal(endState.confirmEnabled, true);
  assert.equal(endState.keepRunning, true, 'the user must still be able to leave it running');
  assert.equal(endState.rowStillLive, true, 'the session must not be shown as ended');

  assert.deepEqual(pageErrors, []);
  if (process.env.SURFACE_TRUTH_SCREENSHOT) {
    await page.screenshot({ path: screenshot, fullPage: true, captureBeyondViewport: false });
  }
  process.stdout.write(`surface-truth smoke passed${process.env.SURFACE_TRUTH_SCREENSHOT ? `: ${screenshot}` : ''}\n`);
} finally {
  if (browser) {
    const browserProcess = browser.process();
    await Promise.race([browser.close().catch(() => {}), delay(3_000)]);
    if (browserProcess && browserProcess.exitCode === null && browserProcess.signalCode === null) {
      browserProcess.kill('SIGKILL');
    }
  }
  if (server) {
    server.closeAllConnections?.();
    await Promise.race([new Promise((resolve) => server.close(resolve)), delay(3_000)]);
  }
  if (!process.env.SURFACE_TRUTH_SCREENSHOT) await rm(work, { recursive: true, force: true });
}
