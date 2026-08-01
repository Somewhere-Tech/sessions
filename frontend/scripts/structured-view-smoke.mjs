import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { extname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { build } from 'esbuild';
import puppeteer from 'puppeteer';

const work = await mkdtemp(join(tmpdir(), 'sessions-structured-view-'));
const publicDir = fileURLToPath(new URL('../public/', import.meta.url));
const screenshot = process.env.STRUCTURED_VIEW_SCREENSHOT || join(work, 'structured-view.png');
let browser;
let server;

const delay = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));

async function withTimeout(promise, label, milliseconds = 10_000) {
  let timer;
  try {
    return await Promise.race([
      promise,
      new Promise((_, reject) => {
        timer = setTimeout(() => reject(new Error(`${label} timed out after ${milliseconds}ms`)), milliseconds);
      })
    ]);
  } finally {
    clearTimeout(timer);
  }
}

try {
  await build({
    entryPoints: [fileURLToPath(new URL('./structured-view-fixture.tsx', import.meta.url))],
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
<script>localStorage.setItem('sessions:servers', JSON.stringify([{id:'fixture',name:'Fixture',host:'127.0.0.1',port:8787,isDefault:true}]));localStorage.setItem('sessions:active-server','fixture');</script>
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
  page.setDefaultTimeout(10_000);
  page.setDefaultNavigationTimeout(10_000);
  await page.setViewport({ width: 1440, height: 960, deviceScaleFactor: 1 });
  const pageErrors = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  await page.goto(`http://127.0.0.1:${address.port}`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('.remote-bubble-plan');
  await page.$eval('.remote-bubble-tools-toggle', (element) => element.click());
  await page.$eval('.remote-bubble-tool', (element) => element.click());

  const report = await page.evaluate(() => ({
    planSteps: document.querySelectorAll('.remote-bubble-plan-step').length,
    activityItems: document.querySelectorAll('.remote-bubble-tool-row').length,
    activitySummary: document.querySelector('.remote-bubble-tools-toggle')?.textContent?.replace(/\s+/g, ' ').trim() ?? '',
    activityLabels: Array.from(document.querySelectorAll('.remote-bubble-tool-input')).map((element) => element.textContent?.trim() ?? ''),
    activityListRadius: getComputedStyle(document.querySelector('.remote-bubble-tools-list')).borderRadius,
    reasoning: document.querySelector('.remote-bubble-reasoning')?.textContent ?? '',
    updates: document.querySelector('.remote-bubble-updates')?.textContent ?? '',
    attachControl: document.querySelector('.input-attach')?.textContent ?? '',
    quickKeyCount: document.querySelectorAll('.qk-btn').length,
    timestampCount: document.querySelectorAll('.remote-message-meta time').length,
    messageAuthor: document.querySelector('.remote-message-author')?.textContent ?? '',
    userCardRadius: getComputedStyle(document.querySelector('.remote-bubble-user')).borderRadius,
    assistantBackground: getComputedStyle(document.querySelector('.remote-bubble-assistant')).backgroundColor,
    runState: document.querySelector('.sidebar-run-state')?.textContent ?? '',
    horizontalOverflow: document.documentElement.scrollWidth - document.documentElement.clientWidth
  }));
  assert.equal(report.planSteps, 3);
  assert.equal(report.activityItems, 2);
  assert.match(report.activitySummary, /Used 2 tools/);
  assert.doesNotMatch(report.activitySummary, /Activity/);
  assert.match(report.activityLabels[0] ?? '', /go test/);
  assert.doesNotMatch(report.activityLabels[0] ?? '', /^Command\b/);
  assert.notEqual(report.activityListRadius, '0px');
  assert.match(report.reasoning, /Reasoning summary/);
  assert.match(report.updates, /progress update/);
  assert.match(report.attachControl, /Attach/);
  assert.equal(report.quickKeyCount, 0);
  assert.equal(report.timestampCount, 2);
  assert.match(report.messageAuthor, /PM Claude · via Sessions/);
  assert.notEqual(report.userCardRadius, '0px');
  assert.match(report.assistantBackground, /rgba\(0, 0, 0, 0\)/);
  assert.match(report.runState, /Working|Ready|\d/);
  assert.equal(report.horizontalOverflow, 0);
  assert.deepEqual(pageErrors, []);
  if (process.env.STRUCTURED_VIEW_SCREENSHOT) {
    await withTimeout(
      page.screenshot({ path: screenshot, captureBeyondViewport: false }),
      'screenshot'
    );
  }
  process.stdout.write(`structured-view smoke passed${process.env.STRUCTURED_VIEW_SCREENSHOT ? `: ${screenshot}` : ''}\n`);
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
  if (!process.env.STRUCTURED_VIEW_SCREENSHOT) await rm(work, { recursive: true, force: true });
}
