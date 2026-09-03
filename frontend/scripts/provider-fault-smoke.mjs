import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { extname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { build } from 'esbuild';
import puppeteer from 'puppeteer';
import { smoke, closeBrowser, closeServer } from './lib/smoke.mjs';

const t = smoke('provider-fault');
const work = await mkdtemp(join(tmpdir(), 'sessions-provider-fault-'));
const publicDir = fileURLToPath(new URL('../public/', import.meta.url));
let browser;
let server;

try {
  await build({
    entryPoints: [fileURLToPath(new URL('./provider-fault-fixture.tsx', import.meta.url))],
    outdir: work,
    bundle: true,
    platform: 'browser',
    format: 'esm',
    define: { 'import.meta.env.BASE_URL': '"/"' },
    entryNames: 'app',
    assetNames: 'asset-[hash]',
    external: ['/claude-icon.svg'],
    loader: { '.svg': 'dataurl', '.png': 'dataurl', '.woff2': 'dataurl' },
    logLevel: 'silent'
  });
  await writeFile(join(work, 'index.html'), `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<link rel="stylesheet" href="/app.css"><style>
.provider-fault-fixture{display:grid;grid-template-columns:360px minmax(0,1fr);grid-template-rows:520px 420px;height:940px;background:#0a0a0a}
#fault-navigator{grid-row:1/3;display:flex;min-height:0}#retrying-view,#pty-view{display:flex;min-width:0;min-height:0}
#retrying-view .session-view,#pty-view .session-view{flex:1}
</style></head><body><div id="root"></div>
<script>localStorage.setItem('sessions:servers',JSON.stringify([{id:'fixture',name:'Fixture Mac',host:'127.0.0.1',port:8787,isDefault:true}]));localStorage.setItem('sessions:active-server','fixture');localStorage.setItem('sessions:navigator-machine-scope','fixture');</script>
<script type="module" src="/app.js"></script></body></html>`);

  server = createServer(async (request, response) => {
    const name = request.url === '/' ? 'index.html' : request.url.slice(1);
    try {
      const source = name === 'openai-icon.svg' || name === 'claude-icon.svg'
        ? join(publicDir, name)
        : join(work, name);
      const body = await readFile(source);
      const type = extname(name) === '.css' ? 'text/css'
        : extname(name) === '.js' ? 'text/javascript'
          : extname(name) === '.svg' ? 'image/svg+xml' : 'text/html';
      response.writeHead(200, { 'content-type': type });
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
  const page = await browser.newPage();
  t.watch(page);
  page.setDefaultTimeout(15_000);
  await page.setViewport({ width: 1440, height: 960, deviceScaleFactor: 1 });
  const pageErrors = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  await page.goto(`http://127.0.0.1:${address.port}`, { waitUntil: 'domcontentloaded' });

  t.scenario('fault labels, grouped trouble and the fleet notice render');
  await t.waitForSelector(page, '#fault-navigator .inbox-provider-banner', 'the provider outage banner to render');
  const navigator = await page.$eval('#fault-navigator', (root) => ({
    banner: root.querySelector('.inbox-provider-banner')?.textContent?.replace(/\s+/g, ' ').trim() ?? '',
    group: root.querySelector('.inbox-provider-trouble .session-tree-group-head')?.textContent?.replace(/\s+/g, ' ').trim() ?? '',
    labels: Array.from(root.querySelectorAll('.inbox-provider-trouble .inbox-needs-why'), (node) => node.textContent?.trim() ?? '')
  }));
  assert.match(navigator.banner, /^Codex is having trouble — 3 sessions since /);
  assert.equal(navigator.group, 'Provider trouble4');
  assert.deepEqual(new Set(navigator.labels), new Set(['Codex unavailable', 'Rate limited', 'Claude unavailable']));

  t.scenario('the Rich card counts down and retry reaches the contract route');
  await t.waitForSelector(page, '#retrying-view .session-remote-pane .provider-control-card.is-provider-fault', 'the Rich provider fault card to render');
  const firstCountdown = await page.$eval('#retrying-view .session-remote-pane .provider-control-card-hint', (node) => node.textContent ?? '');
  assert.match(firstCountdown, /^Retrying in \d+s \(attempt 2 of 5\)$/);
  assert.deepEqual(await page.$$eval('#retrying-view .session-remote-pane .provider-control-card-action', (buttons) => buttons.map((button) => button.textContent?.trim())), ['Retry now', 'Stop retrying']);
  await new Promise((resolve) => setTimeout(resolve, 1_150));
  const secondCountdown = await page.$eval('#retrying-view .session-remote-pane .provider-control-card-hint', (node) => node.textContent ?? '');
  assert.ok(Number(/(\d+)s/.exec(secondCountdown)?.[1]) < Number(/(\d+)s/.exec(firstCountdown)?.[1]));
  await page.$$eval('#retrying-view .session-remote-pane .provider-control-card-action', (buttons) => {
    buttons.find((button) => button.textContent?.trim() === 'Retry now')?.click();
  });
  await t.waitForFunction(page, () => window.__providerFaultFixture?.daemon?.retried?.includes('codex-retrying'), 'Retry now to hit /api/sessions/:id/retry');

  t.scenario('fault and retry history stay system UI, and Terminal shows the card');
  await t.waitForSelector(page, '#retrying-view .remote-bubble-error', 'provider_fault to render as an error bubble');
  assert.match(await page.$eval('#retrying-view .remote-bubble-error', (node) => node.textContent ?? ''), /⚠Codex API unavailable/);
  assert.equal(await page.$eval('#retrying-view .remote-provider-retry', (node) => node.textContent?.trim()), 'Retrying (2 of 5) …');
  await t.waitForSelector(page, '#pty-view .terminal-provider-fault .provider-control-card', 'the PTY terminal fault card to render');
  assert.match(await page.$eval('#pty-view .terminal-provider-fault', (node) => node.textContent ?? ''), /Send your message again when the provider is back/);
  await page.evaluate(() => window.__providerFaultFixture.clearFaults());
  await t.waitForFunction(page, () => !document.querySelector('.inbox-provider-banner') && !document.querySelector('.inbox-provider-trouble'), 'the fleet notice and provider trouble group to clear with the daemon faults');
  assert.deepEqual(pageErrors, []);
  t.pass('provider fault smoke passed');
} finally {
  t.release();
  await closeBrowser(browser, 3_000);
  await closeServer(server, 3_000);
  await rm(work, { recursive: true, force: true });
}
