import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { build } from 'esbuild';
import puppeteer from 'puppeteer';
import { closeBrowser, closeServer } from './lib/smoke.mjs';

// The account-first Accounts page in a real browser: one row per provable
// account, usage meters that fit a phone, and the same page in light and dark.
// Set ACCOUNT_SCREENSHOT_DIR to keep screenshots of each layout.
const work = await mkdtemp(join(tmpdir(), 'sessions-account-usage-'));
let server, browser;
try {
  await build({ entryPoints: ['scripts/account-usage-fixture.tsx'], bundle: true, outdir: work,
    entryNames: 'app', format: 'esm', platform: 'browser', jsx: 'automatic', external: ['/claude-icon.svg', '/openai-icon.svg'],
    define: { 'import.meta.env.BASE_URL': '"/"' }, loader: { '.svg': 'dataurl', '.png': 'dataurl' }, logLevel: 'silent' });
  await writeFile(join(work, 'index.html'), '<!doctype html><meta name="viewport" content="width=device-width, initial-scale=1"><link rel="stylesheet" href="/app.css"><div id="root"></div><script type="module" src="/app.js"></script>');
  server = createServer(async (req, res) => {
    const path = req.url.split('?')[0];
    const name = path === '/' ? 'index.html' : path.slice(1);
    const icon = ['claude-icon.svg', 'openai-icon.svg'].includes(name);
    if (!icon && !['index.html', 'app.js', 'app.css'].includes(name)) { res.writeHead(404); res.end(); return; }
    res.writeHead(200, { 'Content-Type': icon ? 'image/svg+xml' : name.endsWith('.js') ? 'text/javascript' : name.endsWith('.css') ? 'text/css' : 'text/html' });
    res.end(await readFile(icon ? join('public', name) : join(work, name)));
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  browser = await puppeteer.launch({ headless: true });
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', (error) => errors.push(error.message));
  for (const [theme, width] of [['dark', 1280], ['dark', 375], ['light', 1280], ['light', 375]]) {
    await page.setViewport({ width, height: 900 });
    await page.goto(`http://127.0.0.1:${server.address().port}/?theme=${theme}`, { waitUntil: 'networkidle0' });
    await page.waitForFunction(() => document.querySelectorAll('[aria-label="Computers for Team plan"] > li').length === 2);
    await page.waitForFunction(() => document.querySelector('[aria-label="Usage for Side project"]'));
    const rows = await page.$$eval('.accounts-group > .accounts-identity > strong', (nodes) => nodes.map((node) => node.textContent));
    // One Team plan row for both computers; email-only accounts stay apart.
    assert.deepEqual(rows.filter((title) => title === 'Team plan'), ['Team plan']);
    assert.ok(rows.includes('Personal') && rows.includes('Side project'), `rows: ${rows}`);
    const meters = await page.$$eval('[aria-label="Usage for Team plan"] [role="meter"]', (nodes) => nodes.map((node) => node.getAttribute('aria-valuenow')));
    assert.deepEqual(meters, ['93', '41', '5'], 'the fresher Mac mini reading, every limit separately');
    // The meter's fill is visible against its track in both themes.
    const fill = await page.$eval('.account-usage-meter > span', (span) => {
      const channels = (value) => value.match(/[\d.]+/g).slice(0, 3).map(Number);
      const luminance = (rgb) => {
        const [r, g, b] = rgb.map((channel) => { const c = channel / 255; return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4; });
        return 0.2126 * r + 0.7152 * g + 0.0722 * b;
      };
      const [light, dark] = [luminance(channels(getComputedStyle(span).backgroundColor)), luminance(channels(getComputedStyle(span.parentElement).backgroundColor))].sort((a, b) => b - a);
      return (light + 0.05) / (dark + 0.05);
    });
    assert.ok(fill >= 1.5, `meter fill contrast ${fill.toFixed(2)}:1 in ${theme} mode`);
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, `no overflow at ${width}px in ${theme}`);
    if (process.env.ACCOUNT_SCREENSHOT_DIR) await page.screenshot({ path: join(process.env.ACCOUNT_SCREENSHOT_DIR, `account-usage-${theme}-${width}.png`), fullPage: true });
  }
  assert.deepEqual(errors, []);
  console.log('account usage: one row per provable account, fresher reading, separate limits, no overflow in dark and light at desktop and 375px');
} finally {
  await closeBrowser(browser);
  await closeServer(server);
  await rm(work, { recursive: true, force: true });
}
