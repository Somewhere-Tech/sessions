import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { build } from 'esbuild';
import puppeteer from 'puppeteer';
import { closeBrowser, closeServer } from './lib/smoke.mjs';

const work = await mkdtemp(join(tmpdir(), 'sessions-launcher-'));
let server;
let browser;
try {
  await build({ entryPoints: ['scripts/launcher-fixture.tsx'], bundle: true, outdir: work,
    entryNames: 'app', format: 'esm', platform: 'browser', jsx: 'automatic', external: ['/claude-icon.svg'],
    define: { 'import.meta.env.BASE_URL': '"/"' },
    loader: { '.svg': 'dataurl', '.png': 'dataurl' }, logLevel: 'silent' });
  await writeFile(join(work, 'index.html'), '<!doctype html><html><head><meta name="viewport" content="width=device-width, initial-scale=1"><link rel="stylesheet" href="/app.css"></head><body><div id="root" style="height:100dvh"></div><script type="module" src="/app.js"></script></body></html>');
  server = createServer(async (req, res) => {
    const name = req.url === '/' ? 'index.html' : req.url.slice(1);
    if (!['index.html', 'app.js', 'app.css', 'claude-icon.svg', 'openai-icon.svg'].includes(name)) {
      res.writeHead(404); res.end(); return;
    }
    try {
      const file = await readFile(join(name.endsWith('.svg') ? 'public' : work, name));
      res.writeHead(200, { 'Content-Type': name.endsWith('.js') ? 'text/javascript' : name.endsWith('.css') ? 'text/css' : name.endsWith('.svg') ? 'image/svg+xml' : 'text/html' });
      res.end(file);
    } catch { res.writeHead(404); res.end(); }
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  browser = await puppeteer.launch({ headless: true, args: ['--no-sandbox'] });
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', (error) => errors.push(error.message));
  for (const [width, height] of [[1280, 900], [390, 844], [375, 667]]) {
    await page.setViewport({ width, height });
    await page.goto(`http://127.0.0.1:${server.address().port}`, { waitUntil: 'networkidle0' });
    await page.waitForFunction(() => !document.querySelector('[aria-label="Start session"]').disabled);
    assert.equal(await page.$eval('textarea', (el) => el === document.activeElement), false, 'opening the launcher must not summon the phone keyboard');
    const bounds = await page.evaluate(() => ({
      overflow: document.documentElement.scrollWidth > innerWidth,
      controls: [...document.querySelectorAll('.launcher-setup select, .launcher-setup button')].map((el) => {
        const r = el.getBoundingClientRect(); return { x: r.x, right: r.right, height: r.height };
      })
    }));
    assert.equal(bounds.overflow, false, `no horizontal overflow at ${width}px`);
    assert.ok(bounds.controls.every((r) => r.x >= 0 && r.right <= width && r.height >= 42), `readable touch-sized choices at ${width}px`);
    await page.select('[aria-label="Agent"]', 'codex');
    await page.waitForSelector('.model-picker-trigger:not(:disabled)');
    await page.click('.model-picker-trigger');
    await page.waitForSelector('[role="option"]');
    await page.evaluate(() => [...document.querySelectorAll('[role="option"]')].find((el) => el.textContent.includes('Astra')).click());
    assert.match(await page.$eval('.model-picker-trigger', (el) => el.textContent), /Astra/);
    await page.select('[aria-label="Computer"]', 'mini');
    await page.waitForFunction(() => document.querySelector('.is-workspace strong').textContent === 'platform');
    if (width <= 390) {
      const startBottom = await page.$eval('[aria-label="Start session"]', (el) => el.getBoundingClientRect().bottom);
      assert.ok(startBottom <= height, `Start stays visible before opening the keyboard at ${width}px`);
    }
    if (process.env.LAUNCHER_SCREENSHOT_DIR) {
      await page.screenshot({ path: join(process.env.LAUNCHER_SCREENSHOT_DIR, `launcher-${width}.png`), fullPage: true });
    }
    await page.click('[aria-label="Start session"]');
    await page.waitForFunction(() => window.launcherDaemon.created.length === 1);
    const request = await page.evaluate(() => window.launcherDaemon.requests.find((r) => r.method === 'POST' && r.path === '/api/sessions'));
    assert.equal(request.body.cwd, '/Users/example/Projects/platform');
    assert.ok(request.body.args.includes('gpt-6-astra'), 'selected model reaches session creation');
  }
  assert.deepEqual(errors, [], 'no React/browser errors');
  console.log('launcher layout: real controls pass at desktop, 390px and 375px');
} finally {
  await closeBrowser(browser);
  if (server) await closeServer(server);
  await rm(work, { recursive: true, force: true });
}
