// Delegated work stays reachable at every width the app supports.
//
// Below 1180px the Lanes panel overlays the conversation as an absolutely
// positioned grid child. It used to keep its desktop grid-column: 2 after the
// grid dropped to one column, so its containing block was a zero-width track
// at the right edge: the Lanes button toggled and nothing appeared (0px on a
// phone, ~1px on a tablet). This drives the real App and product CSS at a
// phone, a tablet and a desktop width and measures what a person can reach.
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { extname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { build } from 'esbuild';
import puppeteer from 'puppeteer';
import { smoke, closeBrowser, closeServer } from './lib/smoke.mjs';

const t = smoke('subagents-panel');
const work = await mkdtemp(join(tmpdir(), 'sessions-subagents-panel-'));
const publicDir = fileURLToPath(new URL('../public/', import.meta.url));
// Optional private evidence: a directory for one small screenshot per width.
const screenshots = process.env.SUBAGENTS_PANEL_SCREENSHOTS;
const VIEWPORTS = [
  { label: 'phone', width: 390, height: 844, deviceScaleFactor: 1, isMobile: true, hasTouch: true },
  { label: 'tablet', width: 1024, height: 768, deviceScaleFactor: 1 },
  { label: 'desktop', width: 1440, height: 900, deviceScaleFactor: 1 }
];
const TYPES = { '.css': 'text/css', '.js': 'text/javascript', '.svg': 'image/svg+xml', '.png': 'image/png', '.ico': 'image/x-icon' };
let browser;
let server;

// Everything a person needs from the open panel, measured in the page.
function measurePanel() {
  const viewport = document.documentElement.clientWidth;
  const box = (element) => {
    const rect = element.getBoundingClientRect();
    return { left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom, width: rect.width, height: rect.height };
  };
  const reachable = (element) => {
    element.scrollIntoView({ block: 'center', inline: 'nearest' });
    const rect = element.getBoundingClientRect();
    const hit = document.elementFromPoint(rect.left + rect.width / 2, rect.top + rect.height / 2);
    return Boolean(hit && (hit === element || element.contains(hit)));
  };
  const slot = document.querySelector('.subagents-panel-slot');
  const panel = document.querySelector('.subagents-panel');
  const actions = Array.from(document.querySelectorAll('.subagent-card-actions button'))
    .filter((button) => ['Open', 'Hand back'].includes(button.textContent.trim()))
    .map((button) => ({ label: button.textContent.trim(), box: box(button), reachable: reachable(button) }));
  const note = Array.from(document.querySelectorAll('.subagent-card .session-start-note'))
    .find((element) => element.textContent.includes('First request may not have arrived'));
  if (note) note.scrollIntoView({ block: 'center' });
  return {
    viewport,
    documentOverflow: document.documentElement.scrollWidth - viewport,
    slot: slot && box(slot),
    panelOverflow: panel ? panel.scrollWidth - panel.clientWidth : null,
    actions,
    note: note && {
      text: note.textContent.trim(),
      clipped: note.scrollWidth - note.clientWidth,
      box: box(note),
      card: box(note.closest('.subagent-card'))
    }
  };
}

function measureComposer() {
  const composer = document.querySelector('.session-view textarea');
  if (!composer) return null;
  composer.scrollIntoView({ block: 'center' });
  const rect = composer.getBoundingClientRect();
  const hit = document.elementFromPoint(rect.left + rect.width / 2, rect.top + rect.height / 2);
  return {
    width: rect.width,
    inViewport: rect.left >= 0 && rect.right <= document.documentElement.clientWidth + 0.5 && rect.width > 0,
    reachable: hit === composer || composer.contains(hit),
    panelGone: !document.querySelector('.subagents-panel-slot')
  };
}

try {
  await build({
    entryPoints: [fileURLToPath(new URL('./subagents-panel-fixture.tsx', import.meta.url))],
    outdir: work,
    bundle: true,
    platform: 'browser',
    format: 'esm',
    define: { 'import.meta.env.BASE_URL': '"/"', 'import.meta.env.DEV': 'false' },
    entryNames: 'app',
    assetNames: 'asset-[hash]',
    // The stylesheet's absolute public asset is served from public/.
    external: ['/claude-icon.svg'],
    loader: { '.svg': 'dataurl', '.png': 'dataurl', '.woff2': 'dataurl' },
    logLevel: 'silent'
  });
  await writeFile(join(work, 'index.html'), `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="icon" href="data:,"><link rel="stylesheet" href="/app.css"></head><body><div id="root"></div>
<script>localStorage.setItem('sessions:servers',JSON.stringify([{id:'fixture',name:'Fixture Mac',host:'127.0.0.1',port:8787,isDefault:true}]));localStorage.setItem('sessions:active-server','fixture');localStorage.setItem('sessions:projects-machine-scope','fixture');</script>
<script type="module" src="/app.js"></script></body></html>`);

  server = createServer(async (request, response) => {
    const name = new URL(request.url, 'http://fixture').pathname.slice(1) || 'index.html';
    try {
      const body = await readFile(name.startsWith('app.') || name === 'index.html' ? join(work, name) : join(publicDir, name));
      response.writeHead(200, { 'content-type': TYPES[extname(name)] ?? 'text/html' });
      response.end(body);
    } catch {
      response.writeHead(404);
      response.end();
    }
  });
  await t.bounded(new Promise((resolve) => server.listen(0, '127.0.0.1', resolve)), 'the fixture server to bind', 15_000);
  const address = server.address();
  if (!address || typeof address === 'string') throw new Error('fixture server did not bind');
  browser = await t.bounded(puppeteer.launch({ headless: true, args: ['--no-sandbox'] }), 'Chromium to launch', 60_000);

  for (const viewport of VIEWPORTS) {
    const context = await browser.createBrowserContext();
    const page = await context.newPage();
    t.watch(page);
    page.setDefaultTimeout(15_000);
    const problems = [];
    page.on('pageerror', (error) => problems.push(`page error: ${error.message}`));
    page.on('console', (message) => { if (message.type() === 'error') problems.push(`console error: ${message.text()}`); });
    page.on('response', (response) => { if (response.status() >= 400) problems.push(`HTTP ${response.status()} ${response.url()}`); });
    await page.setViewport(viewport);
    await page.goto(`http://127.0.0.1:${address.port}`, { waitUntil: 'domcontentloaded' });

    t.scenario(`${viewport.label} ${viewport.width}px: Lanes opens a visible, usable panel`);
    await t.waitForSelector(page, '[data-session-id="lead"]', 'the manager row to render');
    await page.click('[data-session-id="lead"]');
    await t.waitForSelector(page, '.subagents-panel-trigger', 'the Lanes button on the manager');
    await page.click('.subagents-panel-trigger');
    await t.waitForSelector(page, '.subagents-panel .subagent-card', 'the lane cards to render');
    // TeamEvidence, which carries the start note, loads lazily after the cards.
    await t.waitForSelector(page, '.subagent-card .session-start-note', 'the lane start note to render');
    const panel = await page.evaluate(measurePanel);
    const where = `${viewport.label} ${viewport.width}px: ${JSON.stringify(panel.slot)}`;
    assert.ok(panel.slot, `${where}: the panel slot is missing`);
    assert.ok(panel.slot.width >= Math.min(330, viewport.width - 24), `${where}: the panel is too narrow to use`);
    assert.ok(panel.slot.left >= -0.5 && panel.slot.right <= panel.viewport + 0.5, `${where}: the panel is outside the viewport`);
    if (viewport.width <= 720) assert.ok(Math.abs(panel.slot.width - panel.viewport) <= 1, `${where}: a phone panel takes the full width`);
    assert.ok(panel.documentOverflow <= 0, `${where}: the page scrolls sideways by ${panel.documentOverflow}px`);
    assert.ok(panel.panelOverflow <= 1, `${where}: the panel scrolls sideways by ${panel.panelOverflow}px`);
    assert.deepEqual(panel.actions.map((action) => action.label), ['Open', 'Hand back', 'Open', 'Hand back'], where);
    for (const action of panel.actions) {
      assert.ok(action.reachable, `${where}: ${action.label} is covered or off screen at ${JSON.stringify(action.box)}`);
      assert.ok(action.box.left >= panel.slot.left - 0.5 && action.box.right <= panel.slot.right + 0.5, `${where}: ${action.label} spills out of the panel`);
    }

    t.scenario(`${viewport.label} ${viewport.width}px: the partial-delivery next action is readable in full`);
    assert.ok(panel.note, `${where}: the unconfirmed first request has no note`);
    assert.match(panel.note.text, /did not resend it/);
    assert.match(panel.note.text, /check the conversation first/);
    assert.ok(panel.note.clipped <= 1, `${where}: the note is cut off by ${panel.note.clipped}px: "${panel.note.text}"`);
    assert.ok(panel.note.box.right <= panel.note.card.right + 0.5, `${where}: the note runs past its card`);
    if (screenshots) await page.screenshot({ path: join(screenshots, `lanes-${viewport.label}-${viewport.width}.jpg`), type: 'jpeg', quality: 60 });

    t.scenario(`${viewport.label} ${viewport.width}px: closing the panel returns a usable composer`);
    await page.click('[aria-label="Close lanes"]');
    await t.waitForFunction(page, () => !document.querySelector('.subagents-panel-slot'), 'the panel to close');
    const composer = await page.evaluate(measureComposer);
    assert.ok(composer?.inViewport && composer.reachable && composer.panelGone, `${viewport.label}: composer after close ${JSON.stringify(composer)}`);
    await page.click('.session-view textarea');
    await page.keyboard.type('draft');
    assert.equal(await page.$eval('.session-view textarea', (node) => node.value), 'draft');

    t.scenario(`${viewport.label} ${viewport.width}px: Open reaches the lane and nothing was written`);
    await page.click('.subagents-panel-trigger');
    await t.waitForSelector(page, '.subagents-panel .subagent-card-actions button', 'the panel to reopen');
    await page.evaluate(() => {
      const card = Array.from(document.querySelectorAll('.subagent-card')).find((node) => node.textContent.includes('Review the narrow-screen lanes fix'));
      Array.from(card.querySelectorAll('button')).find((button) => button.textContent.trim() === 'Open').click();
    });
    // Opened sessions stay mounted; the one on screen is the one with a layout box.
    await t.waitForFunction(page, () => Array.from(document.querySelectorAll('.session-active-header'))
      .some((header) => header.getClientRects().length > 0 && header.textContent.includes('Review the narrow-screen lanes fix')), 'Open to show the reviewer lane');
    const writes = await page.evaluate(() => window.__subagentsPanelFixture.daemon.requests
      .filter((request) => request.method !== 'GET').map((request) => `${request.method} ${request.path}`));
    assert.deepEqual(writes, [], `${viewport.label}: viewing lanes must not write`);
    assert.deepEqual(problems, [], `${viewport.label}: page problems`);
    await context.close();
  }
  t.pass('subagents panel smoke passed');
} finally {
  t.release();
  await closeBrowser(browser, 3_000);
  await closeServer(server, 3_000);
  await rm(work, { recursive: true, force: true });
}
