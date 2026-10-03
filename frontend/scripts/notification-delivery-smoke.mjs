import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { build } from 'esbuild';
import puppeteer from 'puppeteer';
import { smoke, closeBrowser, closeServer } from './lib/smoke.mjs';

const t = smoke('notification-delivery');
const work = await mkdtemp(join(tmpdir(), 'sessions-notify-test-'));
let server;
let browser;
try {
  await build({ entryPoints: ['scripts/notification-delivery-fixture.tsx'], outdir: work,
    bundle: true, platform: 'browser', format: 'esm', entryNames: 'app',
    define: { 'import.meta.env.BASE_URL': '"/"' },
    external: ['/claude-icon.svg'],
    loader: { '.svg': 'dataurl', '.png': 'dataurl', '.woff2': 'dataurl' }, logLevel: 'silent' });
  await writeFile(join(work, 'index.html'), '<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/app.css"></head><body><div id="root"></div><script type="module" src="/app.js"></script></body></html>');
  server = createServer(async (req, res) => {
    const name = req.url === '/' ? 'index.html' : req.url.slice(1);
    if (!['index.html', 'app.js', 'app.css'].includes(name)) { res.writeHead(404); res.end(); return; }
    res.writeHead(200, { 'content-type': name.endsWith('.css') ? 'text/css' : name.endsWith('.js') ? 'text/javascript' : 'text/html' });
    res.end(await readFile(join(work, name)));
  });
  await t.bounded(new Promise((resolve) => server.listen(0, '127.0.0.1', resolve)), 'notification fixture to bind', 10_000);
  browser = await t.bounded(puppeteer.launch({ headless: true, args: ['--no-sandbox'] }), 'Chromium to launch', 60_000);
  const page = await browser.newPage();
  await page.setViewport({ width: 1024, height: 1100 });
  const errors = [];
  page.on('pageerror', (error) => errors.push(error.message));
  await t.bounded(page.goto(`http://127.0.0.1:${server.address().port}/`, { waitUntil: 'networkidle0' }), 'notification page to render', 15_000);
  await t.waitForFunction(page, () => document.querySelector('#live')?.textContent?.includes('nowhere to send push alerts'), 'enabled preferences without a destination to be explicit');
  const live = await page.$eval('#live', (node) => node.textContent);
  assert.match(live, /Mac mini/);
  assert.match(live, /HTTPS/);
  assert.match(live, /Native app notifications are separate/);
  const registered = await page.$eval('#registered', (node) => node.textContent);
  assert.match(registered, /Receiving device registered/);
  assert.match(registered, /not a delivery test/);
  const off = await page.$eval('#off', (node) => node.textContent);
  assert.match(off, /All push alerts are switched off/);
  assert.doesNotMatch(off, /Your preferences are on/);
  if (process.env.SESSIONS_NOTIFY_SCREENSHOT) await page.screenshot({ path: process.env.SESSIONS_NOTIFY_SCREENSHOT, fullPage: true });
  await page.click('button');
  await t.waitForFunction(page, () => document.querySelector('#live')?.textContent?.includes('This computer is unreachable'), 'unreachable selected machine to stay unknown, not become disabled');
  const failed = await page.$eval('#live', (node) => node.textContent);
  assert.match(failed, /No notification setting was changed/);
  assert.doesNotMatch(failed, /Receiving device registered|No receiving device registered/);
  const requests = await page.evaluate(() => window.__notificationRequests);
  assert.deepEqual(requests.map((request) => request.method), ['GET', 'GET']);
  assert.match(requests[0].url, /^http:\/\/mini\.invalid:8787\/api\/notify$/);
  assert.match(requests[1].url, /^http:\/\/book\.invalid:8787\/api\/notify$/);
  assert.deepEqual(errors, []);
  t.pass('Notification delivery smoke passed: preferences, destination, honest uncertainty, explicit host, no writes.');
} finally {
  t.release();
  await closeBrowser(browser);
  await closeServer(server);
  await rm(work, { recursive: true, force: true });
}
