import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { build } from 'esbuild';
import puppeteer from 'puppeteer';
import { closeBrowser, closeServer } from './lib/smoke.mjs';

const work = await mkdtemp(join(tmpdir(), 'sessions-account-ui-'));
let server, browser;
try {
  await build({ entryPoints: ['scripts/account-login-fixture.tsx'], bundle: true, outdir: work,
    entryNames: 'app', format: 'esm', platform: 'browser', jsx: 'automatic', external: ['/claude-icon.svg'],
    define: { 'import.meta.env.BASE_URL': '"/"' }, loader: { '.svg': 'dataurl', '.png': 'dataurl' }, logLevel: 'silent' });
  await writeFile(join(work, 'index.html'), '<!doctype html><meta name="viewport" content="width=device-width, initial-scale=1"><link rel="stylesheet" href="/app.css"><div id="root"></div><script type="module" src="/app.js"></script>');
  server = createServer(async (req, res) => {
    const name = req.url === '/' ? 'index.html' : req.url.slice(1);
    if (!['index.html', 'app.js', 'app.css'].includes(name)) { res.writeHead(404); res.end(); return; }
    res.writeHead(200, { 'Content-Type': name.endsWith('.js') ? 'text/javascript' : name.endsWith('.css') ? 'text/css' : 'text/html' });
    res.end(await readFile(join(work, name)));
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  browser = await puppeteer.launch({ headless: true });
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', (error) => errors.push(error.message));
  const click = (label) => page.evaluate((text) => [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === text).click(), label);
  for (const width of [1280, 375]) {
    await page.setViewport({ width, height: 900 });
    await page.goto(`http://127.0.0.1:${server.address().port}`, { waitUntil: 'networkidle0' });
    await click('Add account');
    assert.equal(await page.$('[aria-label="Account name"]'), null);
    await page.type('[aria-label="Account label"]', 'Work');
    await click('Continue');
    await page.waitForSelector('[aria-label="Claude confirmation code"]');
    assert.equal(await page.evaluate(() => window.accountDaemon.created.length), 0);
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, `no overflow at ${width}px`);
    if (process.env.ACCOUNT_SCREENSHOT_DIR) await page.screenshot({ path: join(process.env.ACCOUNT_SCREENSHOT_DIR, `account-login-${width}.png`), fullPage: true });
    await page.type('[aria-label="Claude confirmation code"]', 'fixture-code');
    await click('Connect account');
    await page.waitForFunction(() => document.body.textContent.includes('Account connected'));
    assert.match(await page.$eval('.accounts-signing-in', (el) => el.textContent), /second@example.test/);
    await click('Done');
    assert.equal(await page.$('.accounts-signing-in'), null);
    await click('Add account');
    await page.select('[aria-label="Provider"]', 'codex');
    await click('Continue');
    await page.waitForSelector('.account-device-code');
    assert.equal(await page.$eval('.account-device-code', (el) => el.textContent), 'TEST-CODE');
    await click('Cancel sign-in');
    await page.waitForFunction(() => !document.querySelector('.account-device-code'));
  }
  assert.deepEqual(errors, []);
  console.log('account login: real controls pass at desktop and 375px, with no agent sessions created');
} finally {
  await closeBrowser(browser);
  await closeServer(server);
  await rm(work, { recursive: true, force: true });
}
