import assert from 'node:assert/strict';
import fs from 'node:fs';
import { rm } from 'node:fs/promises';
import http from 'node:http';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import puppeteer from 'puppeteer';
import { smoke, stableDistSnapshot, closeBrowser, closeServer } from './lib/smoke.mjs';

const t = smoke('window-scope');
const frontendDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const appSource = fs.readFileSync(path.join(frontendDir, 'src', 'App.tsx'), 'utf8');
// The suites below serve the real build. They serve a private snapshot of it
// rather than frontend/dist itself: `vite build` empties dist before rewriting
// it, so any concurrent build on the machine used to yank the app out from
// under a running suite — dist missing at start-up, or every asset 404ing
// mid-run, which reads exactly like a slow machine. See stableDistSnapshot.
const distDir = await stableDistSnapshot('window-scope');

const sessions = [
  {
    id: 'codex-1', cmd: 'codex', args: [], cwd: '/tmp/codex', cols: 120, rows: 40,
    createdAt: 1, pid: 101, tool: 'codex', working: false, lastDataAt: 1,
    lastUserMessageAt: null, exited: false, exitCode: null, exitSignal: null, exitedAt: null
  },
  {
    id: 'claude-1', cmd: 'claude', args: [], cwd: '/tmp/claude', cols: 120, rows: 40,
    createdAt: 2, pid: 102, tool: 'claude-code', working: false, lastDataAt: 2,
    lastUserMessageAt: null, exited: false, exitCode: null, exitSignal: null, exitedAt: null
  },
  {
    id: 'shell-1', cmd: 'zsh', args: [], cwd: '/tmp/shell', cols: 120, rows: 40,
    createdAt: 3, pid: 103, tool: 'terminal', working: false, lastDataAt: 3,
    lastUserMessageAt: null, exited: false, exitCode: null, exitSignal: null, exitedAt: null
  },
  {
    id: 'finished-parent', cmd: 'claude', args: [], cwd: '/tmp/parent', cols: 120, rows: 40,
    createdAt: 4, pid: 104, tool: 'claude-code', working: false, lastDataAt: 4,
    lastUserMessageAt: 4, exited: true, exitCode: 0, exitSignal: null, exitedAt: 4,
    conversationId: 'provider-finished-parent'
  },
  {
    id: 'finished-child', parentSessionId: 'finished-parent', cmd: 'codex', args: [], cwd: '/tmp/child', cols: 120, rows: 40,
    createdAt: 5, pid: 105, tool: 'codex', working: false, lastDataAt: 5,
    lastUserMessageAt: 5, exited: true, exitCode: 0, exitSignal: null, exitedAt: 5
  },
  {
    id: 'live-grandchild', parentSessionId: 'finished-child', cmd: 'zsh', args: [], cwd: '/tmp/grandchild', cols: 120, rows: 40,
    createdAt: 6, pid: 106, tool: 'terminal', working: true, lastDataAt: 6,
    lastUserMessageAt: null, exited: false, exitCode: null, exitSignal: null, exitedAt: null
  }
];

function listen(server) {
  return new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
}

function addressOf(server) {
  const address = server.address();
  if (!address || typeof address === 'string') throw new Error('server did not bind');
  return address;
}

function daemonServer(sessionPrefix) {
  let sessionRequests = 0;
  const server = http.createServer((request, response) => {
    response.setHeader('access-control-allow-origin', '*');
    const requested = new URL(request.url ?? '/', 'http://sessions.test');
    if (requested.pathname === '/api/sessions') {
      if (requested.searchParams.get('include_exited') !== '1') {
        response.writeHead(400, { 'content-type': 'application/json' });
        response.end(JSON.stringify({ error: 'operations inbox must request exited sessions' }));
        return;
      }
      sessionRequests += 1;
      response.writeHead(200, { 'content-type': 'application/json' });
      const body = sessionPrefix
        ? sessions.map((session) => ({
            ...session,
            id: `${sessionPrefix}-${session.id}`,
            parentSessionId: session.parentSessionId ? `${sessionPrefix}-${session.parentSessionId}` : undefined
          }))
        : sessions;
      response.end(JSON.stringify({ sessions: body }));
      return;
    }
    if (requested.pathname.endsWith('/api/history/finished-parent/preview')) {
      response.writeHead(200, { 'content-type': 'application/json' });
      response.end(JSON.stringify({
        schemaVersion: 1,
        session: { id: 'finished-parent', name: 'Finished parent', tool: 'claude', cwd: '/tmp/parent', machine: 'Primary', created_at: 4, last_activity_at: 4, message_count: 1, conversation_available: true },
        messages: [{ role: 'assistant', text: 'Finished safely.', timestamp: '2026-07-22T00:00:00Z' }]
      }));
      return;
    }
    response.writeHead(404, { 'content-type': 'application/json' });
    response.end('{}');
  });
  return {
    server,
    get sessionRequests() { return sessionRequests; }
  };
}

const contentTypes = new Map([
  ['.css', 'text/css; charset=utf-8'],
  ['.html', 'text/html; charset=utf-8'],
  ['.js', 'text/javascript; charset=utf-8'],
  ['.json', 'application/json; charset=utf-8'],
  ['.png', 'image/png'],
  ['.svg', 'image/svg+xml'],
  ['.woff2', 'font/woff2']
]);

const uiServer = http.createServer((request, response) => {
  const pathname = decodeURIComponent(new URL(request.url ?? '/', 'http://localhost').pathname);
  const relativePath = pathname === '/' ? 'index.html' : pathname.replace(/^\/+/, '');
  const filePath = path.resolve(distDir, relativePath);
  if (!filePath.startsWith(`${distDir}${path.sep}`) || !fs.existsSync(filePath)) {
    response.writeHead(404).end('not found');
    return;
  }
  response.writeHead(200, {
    'content-type': contentTypes.get(path.extname(filePath)) ?? 'application/octet-stream'
  });
  fs.createReadStream(filePath).pipe(response);
});

const primary = daemonServer('');
const scoped = daemonServer('scoped');
await Promise.all([listen(uiServer), listen(primary.server), listen(scoped.server)]);

const uiAddress = addressOf(uiServer);
const primaryAddress = addressOf(primary.server);
const scopedAddress = addressOf(scoped.server);
const origin = `http://127.0.0.1:${uiAddress.port}`;
const storedServers = [
  {
    id: 'primary-server', name: 'Primary', host: '127.0.0.1', port: primaryAddress.port,
    isDefault: false, scheme: 'http'
  },
  {
    id: 'scoped-server', name: 'Scoped', host: '127.0.0.1', port: scopedAddress.port,
    isDefault: false, scheme: 'http'
  }
];

const browser = await puppeteer.launch({
  headless: true,
  args: ['--no-sandbox', '--disable-dev-shm-usage']
});

async function openCase(query, selector = '.session-view-host:not(.is-hidden)') {
  const page = await browser.newPage();
  const pageErrors = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  await page.evaluateOnNewDocument((serversValue) => {
    window.localStorage.clear();
    window.localStorage.setItem('sessions:servers', JSON.stringify(serversValue));
    window.localStorage.setItem('sessions:active-server', 'primary-server');
  }, storedServers);
  await page.goto(`${origin}/${query}`, { waitUntil: 'domcontentloaded', timeout: 15_000 });
  // Every scope case enters through here, so the wait has to say which URL it
  // opened. Without the query string, six different scenarios failed with one
  // indistinguishable message.
  await t.waitForSelector(
    page,
    selector,
    `the window opened at "/${query}" to settle on ${selector}`,
    { timeout: 10_000 }
  );
  return { page, pageErrors };
}

async function activeView(query) {
  const current = await openCase(query);
  const result = await current.page.evaluate(() => {
    const active = document.querySelector('.session-view-host:not(.is-hidden)');
    return {
      sessionID: active?.getAttribute('data-session-id') ?? null,
      tabIDs: [...document.querySelectorAll('[role="tab"][data-tab-id]')]
        .map((node) => node.getAttribute('data-tab-id'))
    };
  });
  assert.deepEqual(current.pageErrors, []);
  await current.page.close();
  return result;
}

async function managerTabIds() {
  const current = await openCase('', '.session-nav-row[data-session-id="finished-child"]');
  await current.page.evaluate(() => {
    const child = document.querySelector('.session-nav-row[data-session-id="finished-child"]');
    if (!(child instanceof HTMLElement)) throw new Error('finished child row is missing');
    child.click();
  });
  await t.waitForSelector(
    current.page,
    '.session-view-host[data-session-id="finished-child"]:not(.is-hidden)',
    'clicking the finished child row to make its view the visible one'
  );
  await current.page.evaluate(() => {
    const manager = document.querySelector('.session-nav-row[data-session-id="finished-parent"]');
    if (!(manager instanceof HTMLElement)) throw new Error('finished manager row is missing');
    manager.click();
  });
  await t.waitForFunction(
    current.page,
    () => document.querySelectorAll('[role="tab"][data-tab-id]').length === 2,
    'selecting the manager to expose exactly its two leaf tabs'
  );
  const ids = await current.page.evaluate(() =>
    [...document.querySelectorAll('[role="tab"][data-tab-id]')]
      .map((node) => node.getAttribute('data-tab-id'))
  );
  assert.deepEqual(current.pageErrors, []);
  await current.page.close();
  return ids;
}

async function navigatorIds(query) {
  const current = await openCase(query, '.session-nav-row[data-session-id]');
  const ids = await current.page.evaluate(() =>
    [...document.querySelectorAll('.session-nav-row[data-session-id]')]
      .map((node) => node.getAttribute('data-session-id'))
  );
  assert.deepEqual(current.pageErrors, []);
  await current.page.close();
  return ids;
}

async function assertFinishedSessionIsReadOnly() {
  const current = await openCase('', '.session-nav-row[data-session-id="finished-parent"]');
  await current.page.evaluate(() => {
    const row = document.querySelector('.session-nav-row[data-session-id="finished-parent"]');
    if (!(row instanceof HTMLElement)) throw new Error('finished parent row is missing');
    row.click();
  });
  await t.waitForSelector(
    current.page,
    '.session-view-host:not(.is-hidden) .session-history-body',
    'a finished session to open as read-only history rather than a live terminal'
  );
  const state = await current.page.evaluate(() => {
    const node = document.querySelector('.session-view-host:not(.is-hidden)');
    if (!node) throw new Error('active finished session is missing');
    return {
      history: Boolean(node.querySelector('.session-history-body')),
      terminal: Boolean(node.querySelector('.terminal-host')),
      copy: node.textContent,
      popOut: Boolean(node.querySelector('.session-popout-view')),
      resumeButtons: [...node.querySelectorAll('button')]
        .filter((button) => button.textContent?.trim().startsWith('Resume conversation'))
        .length,
      archiveButtons: [...node.querySelectorAll('button')]
        .filter((button) => button.textContent?.trim() === 'Archive from list')
        .length,
      globalResumeControls: document.querySelectorAll('.tab-resume').length,
      tabPopOutControls: document.querySelectorAll('.tab-popout').length
    };
  });
  assert.equal(state.history, true);
  assert.equal(state.terminal, false);
  assert.equal(state.popOut, true);
  assert.equal(state.resumeButtons, 1);
  assert.equal(state.archiveButtons, 1);
  assert.equal(state.globalResumeControls, 0);
  assert.equal(state.tabPopOutControls, 0);
  assert.match(state.copy ?? '', /viewing does not resume or send anything/i);
  assert.deepEqual(current.pageErrors, []);
  await current.page.close();
}

assert.match(
  appSource,
  /function SinglePopOut[\s\S]*onOpenSession=\{\(nextSessionId\) => \{[\s\S]*next\.searchParams\.set\('session', nextSessionId\);[\s\S]*window\.location\.assign\(next\);/,
  'single-session windows must keep attributable ended-by links navigable'
);

async function assertTreeDisclosureIsClear() {
  const current = await openCase('', '.session-tree-toggle');
  const disclosure = await current.page.$eval('.session-tree-toggle', (button) => {
    const rect = button.getBoundingClientRect();
    return {
      width: rect.width,
      height: rect.height,
      hasChevron: Boolean(button.querySelector('.session-disclosure-chevron'))
    };
  });
  assert.ok(disclosure.width >= 30);
  assert.ok(disclosure.height >= 30);
  assert.equal(disclosure.hasChevron, true);
  assert.deepEqual(current.pageErrors, []);
  await current.page.close();
}

try {
  // The operations-inbox contract keeps every scoped session in the
  // navigator but only the explicitly opened/active one in the tab strip.
  assert.deepEqual(await navigatorIds(''), ['finished-parent', 'finished-child', 'live-grandchild', 'shell-1', 'claude-1', 'codex-1']);
  await assertFinishedSessionIsReadOnly();
  await assertTreeDisclosureIsClear();
  t.scenario('an unscoped window opens the first session, and tool/server scopes narrow it');
  assert.deepEqual(await activeView(''), { sessionID: 'codex-1', tabIDs: [] });
  assert.deepEqual(await activeView('?tool=codex'), { sessionID: 'codex-1', tabIDs: [] });
  assert.deepEqual(await activeView('?tool=claude'), { sessionID: 'claude-1', tabIDs: [] });
  assert.deepEqual(await activeView('?tool=shell'), { sessionID: 'shell-1', tabIDs: [] });
  assert.deepEqual(await managerTabIds(), ['finished-parent', 'finished-child']);

  const primaryBefore = primary.sessionRequests;
  const scopedBefore = scoped.sessionRequests;
  assert.deepEqual(
    await activeView('?server=scoped-server'),
    { sessionID: 'scoped-codex-1', tabIDs: [] }
  );
  assert.equal(primary.sessionRequests, primaryBefore);
  assert.ok(scoped.sessionRequests > scopedBefore);

  t.scenario('?session=…&mode=single pins one session and hides the pop-out chrome');
  const single = await openCase('?session=codex-1&mode=single', '.single-mode');
  await t.waitForFunction(
    single.page,
    () => document.querySelector('.single-mode-label')?.textContent?.trim() === 'codex',
    'single-session mode to label the window with the codex session it was pinned to'
  );
  const singleLabel = await single.page.evaluate(
    () => document.querySelector('.single-mode-label')?.textContent?.trim()
  );
  assert.equal(singleLabel, 'codex');
  assert.equal(await single.page.$('.session-popout-view'), null);
  assert.deepEqual(single.pageErrors, []);
  await single.page.close();

  console.log(JSON.stringify({
    navigator: 6,
    readOnlyHistory: true,
    contextualResume: true,
    disclosure: '30px',
    popOut: true,
    leafTabs: 0,
    managerTabs: 2,
    toolScopes: ['codex', 'claude', 'shell'],
    serverScope: 'scoped-server',
    singleSession: 'codex-1',
    result: 'ok'
  }));
} finally {
  t.release();
  // Three servers and a browser, all closed with unbounded awaits. Any one
  // socket the browser had not released turned a finished suite into a silent
  // hang; bound every one of them.
  await closeBrowser(browser);
  await Promise.all([uiServer, primary.server, scoped.server].map((s) => closeServer(s)));
  // The dist snapshot is this suite's private copy; nothing else will remove it.
  await rm(distDir, { recursive: true, force: true });
}
