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
  // WCAG contrast of a button's text against its own fill.
  const contrastOf = (label) => page.evaluate((text) => {
    const button = [...document.querySelectorAll('button')].find((candidate) => candidate.textContent.trim() === text);
    const channels = (value) => value.match(/[\d.]+/g).slice(0, 3).map(Number);
    const luminance = (rgb) => {
      const [r, g, b] = rgb.map((channel) => { const c = channel / 255; return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4; });
      return 0.2126 * r + 0.7152 * g + 0.0722 * b;
    };
    const style = getComputedStyle(button);
    const [light, dark] = [luminance(channels(style.color)), luminance(channels(style.backgroundColor))].sort((a, b) => b - a);
    return (light + 0.05) / (dark + 0.05);
  }, label);
  const click = (label) => page.evaluate((text) => [...document.querySelectorAll('button')].find((button) => (button.getAttribute('aria-label') || button.textContent.trim()) === text).click(), label);
  for (const [theme, width] of [['dark', 1280], ['dark', 375], ['light', 1280], ['light', 375]]) {
    await page.setViewport({ width, height: 900 });
    await page.goto(`http://127.0.0.1:${server.address().port}/?theme=${theme}`, { waitUntil: 'networkidle0' });
    await click('Add account');
    assert.equal(await page.$('[aria-label="Account name"]'), null);
    assert.equal(await page.$('[aria-label="Account label"]'), null);
    await click('Claude');
    const contrast = await contrastOf('Sign in to Claude');
    assert.ok(contrast >= 4.5, `Sign-in contrast ${contrast.toFixed(2)}:1 in ${theme} mode`);
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, `no picker overflow at ${width}px`);
    if (process.env.ACCOUNT_SCREENSHOT_DIR) await page.screenshot({ path: join(process.env.ACCOUNT_SCREENSHOT_DIR, `account-picker-${theme}-${width}.png`), fullPage: true });
    await click('Sign in to Claude');
    await page.waitForSelector('[aria-label="Claude confirmation code"]');
    assert.equal(await page.evaluate(() => window.accountDaemon.created.length), 0);
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, `no overflow at ${width}px`);
    assert.equal(await page.$('.account-picker'), null, 'the picker closes while its sign-in is open');
    if (process.env.ACCOUNT_SCREENSHOT_DIR) await page.screenshot({ path: join(process.env.ACCOUNT_SCREENSHOT_DIR, `account-login-${theme}-${width}.png`), fullPage: true });
    await page.type('[aria-label="Claude confirmation code"]', 'fixture-code');
    await click('Connect account');
    await page.waitForFunction(() => document.body.textContent.includes('Account connected'));
    assert.match(await page.$eval('.accounts-signing-in', (el) => el.textContent), /second@example.test/);
    await click('Done');
    assert.equal(await page.$('.accounts-signing-in'), null);
    // Rename in place: the daemon keeps the nickname, the row shows it once.
    await click('Rename');
    await page.waitForSelector('.accounts-rename input');
    await page.$eval('.accounts-rename input', (input) => input.select());
    await page.type('.accounts-rename input', 'Team plan\n');
    await page.waitForFunction(() => [...document.querySelectorAll('.accounts-identity strong')].some((el) => el.textContent === 'Team plan'));
    assert.equal(await page.evaluate(() => window.accountDaemon.machines[0].profiles[0].label), 'Team plan');
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, `no overflow after rename at ${width}px`);
    if (process.env.ACCOUNT_SCREENSHOT_DIR) await page.screenshot({ path: join(process.env.ACCOUNT_SCREENSHOT_DIR, `account-list-${theme}-${width}.png`), fullPage: true });
    await click('Add account');
    await click('ChatGPT');
    assert.match(await page.$eval('.account-device-help', (el) => el.textContent), /device-code sign-in/);
    await click('Sign in to ChatGPT');
    await page.waitForSelector('.account-device-code');
    assert.equal(await page.$eval('.account-device-code', (el) => el.textContent), 'TEST-CODE');
    await click('Cancel sign-in');
    await page.waitForFunction(() => !document.querySelector('.account-device-code'));
  }
  assert.deepEqual(errors, []);
  console.log('account login: add, sign-in, and rename pass in dark and light at desktop and 375px, with no agent sessions created');
} finally {
  await closeBrowser(browser);
  await closeServer(server);
  await rm(work, { recursive: true, force: true });
}
