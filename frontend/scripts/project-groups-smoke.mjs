import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtemp, readFile, rm, writeFile, mkdir } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { extname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { build } from 'esbuild';
import puppeteer from 'puppeteer';
import { smoke, closeBrowser, closeServer } from './lib/smoke.mjs';

const t = smoke('project-groups');
const work = await mkdtemp(join(tmpdir(), 'sessions-project-groups-'));
const publicDir = fileURLToPath(new URL('../public/', import.meta.url));
const screenshots = process.env.PROJECT_GROUPS_SCREENSHOTS;
let browser;
let server;

try {
  t.scenario('the real navigator bundles over a 100-agent fake daemon');
  await build({ entryPoints: [fileURLToPath(new URL('./project-groups-fixture.tsx', import.meta.url))],
    outdir: work, bundle: true, platform: 'browser', format: 'esm', entryNames: 'app', assetNames: 'asset-[hash]',
    define: { 'import.meta.env.BASE_URL': '"/"' }, external: ['/claude-icon.svg'],
    loader: { '.svg': 'dataurl', '.png': 'dataurl', '.woff2': 'dataurl' }, logLevel: 'silent' });
  await writeFile(join(work, 'index.html'), `<!doctype html><html><head><meta charset="utf-8">
    <meta name="viewport" content="width=device-width"><link rel="stylesheet" href="/app.css"></head>
    <body><div id="root"></div><script>
    const scope = new URLSearchParams(location.search).get('scope') || 'all-machines';
    localStorage.setItem('sessions:projects-machine-scope', scope);
    localStorage.setItem('sessions:navigator-grouping', 'project');
    </script><script type="module" src="/app.js"></script></body></html>`);
  server = createServer(async (request, response) => {
    const name = new URL(request.url, 'http://fixture').pathname.slice(1) || 'index.html';
    try {
      const body = await readFile(join(['openai-icon.svg', 'claude-icon.svg'].includes(name) ? publicDir : work, name));
      const types = { '.css': 'text/css', '.js': 'text/javascript', '.svg': 'image/svg+xml' };
      response.writeHead(200, { 'content-type': types[extname(name)] || 'text/html' });
      response.end(body);
    } catch { response.writeHead(404); response.end(); }
  });
  await t.bounded(new Promise((resolve) => server.listen(0, '127.0.0.1', resolve)), 'the static fixture to bind', 15_000);
  const address = server.address();
  if (!address || typeof address === 'string') throw new Error('fixture server did not bind');
  browser = await t.bounded(puppeteer.launch({ headless: true, args: ['--no-sandbox'] }), 'Chromium to launch', 60_000);
  const page = await browser.newPage();
  t.watch(page);
  const errors = [];
  page.on('pageerror', (error) => errors.push(error.message));
  page.setDefaultTimeout(15_000);
  if (screenshots) await mkdir(screenshots, { recursive: true });

  for (const viewport of [{ name: 'desktop', width: 1440, height: 960 }, { name: 'mobile', width: 390, height: 844 }]) {
    for (const scope of ['all-machines', 'fixture']) {
      for (const theme of ['dark', 'light']) {
        t.scenario(`${viewport.name} ${scope} ${theme}: bounded previews retain attention and selected rows`);
        await page.setViewport({ width: viewport.width, height: viewport.height, deviceScaleFactor: 1 });
        await page.goto(`http://127.0.0.1:${address.port}/?scope=${scope}&theme=${theme}`, { waitUntil: 'domcontentloaded' });
        const groups = scope === 'all-machines' ? '.agent-project' : '.inbox-project';
        const head = scope === 'all-machines' ? '.agent-project-disclosure' : '.inbox-project-head';
        await t.waitForFunction(page, (selector) => document.querySelectorAll(selector).length === 5, 'five project groups to render', { timeout: 15_000 }, groups);
        const initial = await page.evaluate((selector) => Array.from(document.querySelectorAll(selector), (group) => ({
          rows: group.querySelectorAll('.session-nav-row').length,
          name: group.querySelector('.agent-project-name, .session-group-disclosure')?.textContent?.trim(),
          count: group.querySelector('.agent-project-count, .inbox-project-head strong')?.textContent?.trim(),
          ids: Array.from(group.querySelectorAll('[data-session-id]'), (row) => row.getAttribute('data-session-id'))
        })), groups);
        assert.deepEqual(initial.map((group) => group.rows).sort(), [3, 3, 3, 3, scope === 'all-machines' ? 5 : 4]);
        const sessionGroup = initial.find((group) => group.name?.endsWith('Sessions'));
        assert.match(sessionGroup.count, scope === 'all-machines' ? /20$/ : /19$/, 'local count excludes the separate Pinned group');
        for (const index of [15, 16, 18, 19]) assert.ok(sessionGroup.ids.includes(`project-0-agent-${index}`));
        assert.ok(await page.$('[data-session-id="project-0-agent-17"]'), 'pinned conversation remains visible');
        const layout = await page.$eval('.session-navigator', (node) => ({ width: node.getBoundingClientRect().width, scroll: node.scrollWidth, client: node.clientWidth }));
        assert.ok(layout.width <= viewport.width && layout.scroll <= layout.client + 1, 'navigator must not overflow horizontally');
        if (screenshots) await page.screenshot({ path: join(screenshots, `${viewport.name}-${scope}-${theme}.png`) });

        t.scenario(`${viewport.name} ${scope} ${theme}: explicit reveal, fewer, keyboard collapse, no writes`);
        await page.$eval(`${groups} .project-preview-toggle`, (button) => button.click());
        await t.waitForFunction(page, (selector) => document.querySelector(selector)?.querySelectorAll('.session-nav-row').length >= 19, 'the selected project to reveal its remaining rows', { timeout: 15_000 }, groups);
        await page.$eval(`${groups} .project-preview-toggle`, (button) => button.click());
        await page.focus(`${groups} ${head}`);
        await page.keyboard.press('Enter');
        assert.equal(await page.$eval(`${groups} ${head}`, (button) => button.getAttribute('aria-expanded')), 'false');
        assert.equal(await page.$eval(groups, (group) => group.querySelectorAll('.session-nav-row').length), 0);
        await page.keyboard.press('Space');
        assert.equal(await page.$eval(`${groups} ${head}`, (button) => button.getAttribute('aria-expanded')), 'true');
        const daemon = await page.evaluate(() => ({
          writes: window.projectGroupDaemon.requests.filter((request) => request.method !== 'GET'),
          unhandled: window.projectGroupDaemon.unhandled
        }));
        assert.deepEqual(daemon.writes, [], 'disclosure must never write session state');
        assert.deepEqual(daemon.unhandled, [], 'all product requests must reach the fake daemon');
      }
    }
  }
  assert.deepEqual(errors, []);
  t.pass('project-groups smoke passed: 100 agents, desktop/mobile, local/all-machines, dark/light');
} finally {
  t.release();
  await closeBrowser(browser);
  await closeServer(server);
  await rm(work, { recursive: true, force: true });
}
