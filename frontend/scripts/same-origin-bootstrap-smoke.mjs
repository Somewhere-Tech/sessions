import assert from 'node:assert/strict';
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import puppeteer from 'puppeteer';
import { smoke } from './lib/smoke.mjs';

const t = smoke('same-origin-bootstrap');
const frontendDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const distDir = path.join(frontendDir, 'dist');
if (!fs.existsSync(path.join(distDir, 'index.html'))) {
  throw new Error('frontend/dist is missing; run npx vite build first');
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

const server = http.createServer((request, response) => {
  const pathname = decodeURIComponent(new URL(request.url ?? '/', 'http://localhost').pathname);
  const relativePath = pathname === '/' ? 'index.html' : pathname.replace(/^\/+/, '');
  const filePath = path.resolve(distDir, relativePath);
  if (!filePath.startsWith(`${distDir}${path.sep}`) || !fs.existsSync(filePath) || !fs.statSync(filePath).isFile()) {
    response.writeHead(404).end('not found');
    return;
  }
  response.writeHead(200, {
    'content-type': contentTypes.get(path.extname(filePath)) ?? 'application/octet-stream'
  });
  fs.createReadStream(filePath).pipe(response);
});

await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
const address = server.address();
if (!address || typeof address === 'string') throw new Error('failed to bind smoke server');
assert.notEqual(address.port, 8787, 'smoke server must exercise a non-8787 origin');
const origin = `http://127.0.0.1:${address.port}`;

const browser = await puppeteer.launch({
  headless: true,
  args: ['--no-sandbox', '--disable-dev-shm-usage']
});

async function openCase(health, pairClaim = null) {
  const page = await browser.newPage();
  await page.setBypassServiceWorker(true);
  await page.evaluateOnNewDocument(() => window.localStorage.clear());

  let healthRequests = 0;
  let sessionsRequests = 0;
  let sessionsUnauthorized = false;
  let pairClaimRequests = 0;
  const pairClaimTickets = [];
  const sessionsAuthorizations = [];
  const pageErrors = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  await page.setRequestInterception(true);
  page.on('request', (request) => {
    const url = request.url();
    const requestUrl = new URL(url);
    const pathname = requestUrl.pathname;
    if (url === `${origin}/api/pair/claim`) {
      pairClaimRequests += 1;
      try {
        pairClaimTickets.push(JSON.parse(request.postData() ?? '{}').ticket ?? '');
      } catch {
        pairClaimTickets.push('');
      }
      if (pairClaim === 'success') {
        void request.respond({
          status: 201,
          contentType: 'application/json',
          body: JSON.stringify({
            device_id: '00000000-0000-4000-8000-000000000001',
            token: 'paired-device-token',
            name: 'Smoke browser'
          })
        });
      } else if (pairClaim === 'expired') {
        void request.respond({
          status: 410,
          contentType: 'application/json',
          body: JSON.stringify({
            error: 'Pairing ticket is invalid, expired, or already used. Run `sessions pair` to create a new one.'
          })
        });
      } else {
        void request.respond({ status: 404, contentType: 'application/json', body: '{}' });
      }
      return;
    }
    if (url === `${origin}/api/health`) {
      healthRequests += 1;
      if (health === 'reject') {
        void request.abort('failed');
      } else if (health === 'unauthorized') {
        void request.respond({ status: 401, contentType: 'application/json', body: '' });
      } else {
        // Contract-shaped health (runtime/CONTRACT/http-api.md § GET /api/health).
        // The origin probe now runs the same validateServerHealth as every
        // other entry point, so `ok` and the API range are both meaningful.
        void request.respond({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify({
            ok: true,
            name: 'sessionsd',
            version: '0.2.16',
            listen: { host: '127.0.0.1', port: address.port },
            lan: { enabled: false, url: null },
            discovering: false,
            sessionsLoaded: 0,
            compatibility: health === 'incompatible'
              ? { api: { current: 9, minimumClient: 8, maximumClient: 9 }, runner: { current: 2, minimum: 0, maximum: 2 } }
              : { api: { current: 1, minimumClient: 1, maximumClient: 1 }, runner: { current: 2, minimum: 0, maximum: 2 } }
          })
        });
      }
      return;
    }
    if (requestUrl.origin === origin && pathname === '/api/sessions') {
      sessionsRequests += 1;
      sessionsAuthorizations.push(request.headers().authorization ?? '');
      const status = sessionsUnauthorized ? 401 : 200;
      const body = status === 200 ? JSON.stringify({ sessions: [] }) : '';
      void request.respond({ status, contentType: 'application/json', body });
      return;
    }
    void request.continue();
  });

  return {
    page,
    pageErrors,
    get healthRequests() { return healthRequests; },
    get sessionsRequests() { return sessionsRequests; },
    get pairClaimRequests() { return pairClaimRequests; },
    get pairClaimTickets() { return [...pairClaimTickets]; },
    get sessionsAuthorizations() { return [...sessionsAuthorizations]; },
    requireSessionsToken() { sessionsUnauthorized = true; }
  };
}

try {
  t.scenario('a healthy same-origin daemon is adopted without showing the picker');
  const healthy = await openCase('healthy');
  await healthy.page.goto(origin, { waitUntil: 'domcontentloaded', timeout: 15_000 });
  await t.waitForSelector(healthy.page, '.app-shell', 'the app shell to mount over the adopted same-origin daemon', { timeout: 10_000 });
  await t.waitForFunction(healthy.page, () => {
    const servers = JSON.parse(window.localStorage.getItem('sessions:servers') ?? '[]');
    return servers.length === 1 && window.localStorage.getItem('sessions:active-server') === servers[0].id;
  }, 'the origin probe to store exactly one server and select it');
  const healthyState = await healthy.page.evaluate(() => ({
    pickerVisible: document.querySelector('[data-testid="connect-screen"]') !== null,
    servers: JSON.parse(window.localStorage.getItem('sessions:servers') ?? '[]'),
    activeId: window.localStorage.getItem('sessions:active-server')
  }));
  assert.equal(healthyState.pickerVisible, false);
  assert.deepEqual(healthyState.servers[0], {
    id: 'local',
    name: 'This machine',
    host: '127.0.0.1',
    port: address.port,
    isDefault: true,
    scheme: 'http'
  });
  assert.equal(healthyState.activeId, 'local');

  await t.waitForFunction(healthy.page, () => document.querySelector('.empty-state') !== null, 'the empty session list to render, proving /api/sessions was answered');
  t.scenario('a daemon that starts refusing the stored token prompts for a new one');
  healthy.requireSessionsToken();
  await t.waitForSelector(healthy.page, '.daemon-banner-token-input', 'the token prompt to appear once /api/sessions starts answering 401', { timeout: 6_000 });
  const runtimePrompt = await healthy.page.$eval(
    '.daemon-banner-host',
    (node) => node.textContent?.trim() ?? ''
  );
  assert.equal(runtimePrompt, origin);
  assert.deepEqual(healthy.pageErrors, []);
  await healthy.page.close();

  t.scenario('a 401 from the origin probe asks for a token rather than the picker');
  const unauthorized = await openCase('unauthorized');
  await unauthorized.page.goto(origin, { waitUntil: 'domcontentloaded', timeout: 15_000 });
  await t.waitForSelector(unauthorized.page, '.daemon-banner-token-input', 'the token prompt to appear for a same-origin daemon that answered 401', { timeout: 10_000 });
  const unauthorizedState = await unauthorized.page.evaluate(() => ({
    endpoint: document.querySelector('.daemon-banner-host')?.textContent?.trim() ?? '',
    tokenFocused: document.activeElement?.classList.contains('daemon-banner-token-input') ?? false,
    servers: JSON.parse(window.localStorage.getItem('sessions:servers') ?? '[]')
  }));
  assert.equal(unauthorizedState.endpoint, origin);
  assert.equal(unauthorizedState.tokenFocused, true);
  assert.equal(unauthorizedState.servers[0]?.id, 'local');
  assert.deepEqual(unauthorized.pageErrors, []);
  await unauthorized.page.close();

  t.scenario('an origin with no daemon falls back to the connect picker');
  const rejected = await openCase('reject');
  await rejected.page.goto(origin, { waitUntil: 'domcontentloaded', timeout: 15_000 });
  await t.waitForSelector(rejected.page, '[data-testid="connect-screen"]', 'the connect picker to appear when the origin probe is refused outright', { timeout: 10_000 });
  const rejectedState = await rejected.page.evaluate(() => ({
    servers: JSON.parse(window.localStorage.getItem('sessions:servers') ?? '[]'),
    activeId: window.localStorage.getItem('sessions:active-server')
  }));
  assert.deepEqual(rejectedState, { servers: [], activeId: null });
  assert.deepEqual(rejected.pageErrors, []);
  await rejected.page.close();

  // An out-of-range daemon must not be adopted silently. The probe used to
  // accept any body with an `ok` or `name` field, so a machine this client
  // cannot speak to became the active server and the user met whatever broke
  // next instead of the instructional compatibility message.
  t.scenario('an out-of-range daemon is refused with the compatibility message, not adopted');
  const incompatible = await openCase('incompatible');
  await incompatible.page.goto(origin, { waitUntil: 'domcontentloaded', timeout: 15_000 });
  await t.waitForSelector(incompatible.page, '[data-testid="connect-screen"]', 'the connect picker to appear instead of adopting an API-incompatible daemon', { timeout: 10_000 });
  await t.waitForSelector(incompatible.page, '.connect-error', 'the instructional compatibility error to be rendered on the picker', { timeout: 6_000 });
  const incompatibleState = await incompatible.page.evaluate(() => ({
    error: document.querySelector('.connect-error')?.textContent?.trim() ?? '',
    servers: JSON.parse(window.localStorage.getItem('sessions:servers') ?? '[]'),
    activeId: window.localStorage.getItem('sessions:active-server')
  }));
  assert.match(incompatibleState.error, /Sessions API 1, but the machine accepts 8–9/);
  assert.deepEqual(incompatibleState.servers, []);
  assert.equal(incompatibleState.activeId, null);
  assert.deepEqual(incompatible.pageErrors, []);
  await incompatible.page.close();

  const fragment = await openCase('healthy');
  const fragmentUrl = `${origin}/#endpoint=${encodeURIComponent(origin)}&token=fragment-smoke-token`;
  t.scenario('an #endpoint=…&token=… fragment bootstraps without probing, and is scrubbed');
  await fragment.page.goto(fragmentUrl, { waitUntil: 'domcontentloaded', timeout: 15_000 });
  await t.waitForSelector(fragment.page, '.app-shell', 'the app shell to mount from the URL fragment alone', { timeout: 10_000 });
  const fragmentState = await fragment.page.evaluate(() => ({
    hash: window.location.hash,
    servers: JSON.parse(window.localStorage.getItem('sessions:servers') ?? '[]'),
    activeId: window.localStorage.getItem('sessions:active-server')
  }));
  assert.equal(fragment.healthRequests, 0);
  assert.equal(fragmentState.hash, '');
  assert.equal(fragmentState.servers.length, 1);
  assert.equal(fragmentState.servers[0].token, 'fragment-smoke-token');
  assert.equal(fragmentState.servers[0].isDefault, false);
  assert.equal(fragmentState.activeId, fragmentState.servers[0].id);
  assert.deepEqual(fragment.pageErrors, []);
  await fragment.page.close();

  t.scenario('a #pair=… ticket is claimed once, stored, and scrubbed from the URL');
  const paired = await openCase('healthy', 'success');
  await paired.page.goto(`${origin}/#pair=one-time-smoke-ticket`, {
    waitUntil: 'domcontentloaded', timeout: 15_000
  });
  await t.waitForSelector(paired.page, '.app-shell', 'the app shell to mount after the pairing ticket was claimed', { timeout: 10_000 });
  await t.waitForFunction(paired.page, () => {
    const servers = JSON.parse(window.localStorage.getItem('sessions:servers') ?? '[]');
    return servers.length === 1 && servers[0].token === 'paired-device-token';
  }, 'the claimed pairing token to be stored as the one server\'s credential');
  const pairedState = await paired.page.evaluate(() => ({
    hash: window.location.hash,
    servers: JSON.parse(window.localStorage.getItem('sessions:servers') ?? '[]'),
    activeId: window.localStorage.getItem('sessions:active-server')
  }));
  assert.equal(paired.healthRequests, 0);
  assert.equal(paired.pairClaimRequests, 1);
  assert.deepEqual(paired.pairClaimTickets, ['one-time-smoke-ticket']);
  assert.equal(pairedState.hash, '');
  assert.equal(pairedState.servers[0].token, 'paired-device-token');
  assert.equal(pairedState.servers[0].isDefault, true);
  assert.equal(pairedState.activeId, pairedState.servers[0].id);
  await t.waitForFunction(paired.page, () => document.querySelector('.empty-state') !== null, 'the empty session list to render using the paired token');
  assert.ok(paired.sessionsAuthorizations.includes('Bearer paired-device-token'));
  paired.requireSessionsToken();
  await t.waitForSelector(paired.page, '.daemon-banner-token-input', 'the token prompt to reappear once the paired token is revoked', { timeout: 6_000 });
  assert.deepEqual(paired.pageErrors, []);
  await paired.page.close();

  t.scenario('an already-used pairing ticket says so and leaves the picker usable');
  const expiredPair = await openCase('healthy', 'expired');
  await expiredPair.page.goto(`${origin}/#pair=expired-smoke-ticket`, {
    waitUntil: 'domcontentloaded', timeout: 15_000
  });
  await t.waitForSelector(expiredPair.page, '[data-testid="connect-screen"]', 'the connect picker to appear after a refused pairing claim', { timeout: 10_000 });
  await t.waitForSelector(expiredPair.page, '.connect-error', 'the expired-ticket explanation to be rendered on the picker', { timeout: 10_000 });
  const expiredPairState = await expiredPair.page.evaluate(() => ({
    hash: window.location.hash,
    error: document.querySelector('.connect-error')?.textContent?.trim() ?? '',
    endpointInputUsable: !(document.querySelector('.connect-form input[type="url"]')?.disabled ?? true),
    activeId: window.localStorage.getItem('sessions:active-server')
  }));
  assert.equal(expiredPair.healthRequests, 0);
  assert.equal(expiredPair.pairClaimRequests, 1);
  assert.equal(expiredPairState.hash, '');
  assert.match(expiredPairState.error, /invalid, expired, or already used/i);
  assert.equal(expiredPairState.endpointInputUsable, true);
  assert.equal(expiredPairState.activeId, null);
  assert.deepEqual(expiredPair.pageErrors, []);
  await expiredPair.page.close();

  console.log(JSON.stringify({
    origin,
    healthy: {
      healthRequests: healthy.healthRequests,
      autoSelected: healthyState.activeId === 'local',
      pickerVisible: healthyState.pickerVisible,
      runtime401Prompt: runtimePrompt
    },
    unauthorized: {
      healthRequests: unauthorized.healthRequests,
      endpoint: unauthorizedState.endpoint,
      tokenFocused: unauthorizedState.tokenFocused
    },
    rejected: {
      healthRequests: rejected.healthRequests,
      pickerVisible: true,
      serverCount: rejectedState.servers.length
    },
    fragment: {
      healthRequests: fragment.healthRequests,
      hashAfterBootstrap: fragmentState.hash,
      selected: fragmentState.activeId === fragmentState.servers[0].id,
      tokenStored: fragmentState.servers[0].token === 'fragment-smoke-token'
    },
    pairing: {
      claimRequests: paired.pairClaimRequests,
      healthRequests: paired.healthRequests,
      hashAfterBootstrap: pairedState.hash,
      tokenStored: pairedState.servers[0].token === 'paired-device-token',
      revokedTokenPrompted: true
    },
    expiredPairing: {
      claimRequests: expiredPair.pairClaimRequests,
      healthRequests: expiredPair.healthRequests,
      hashAfterBootstrap: expiredPairState.hash,
      pickerUsable: expiredPairState.endpointInputUsable,
      error: expiredPairState.error
    }
  }, null, 2));
} finally {
  t.release();
  // Teardown used to be two unbounded awaits. `server.close()` does not return
  // until every connection is gone, so one keep-alive socket the browser had
  // not yet released turned a finished suite into a hang — an outcome with no
  // failing assertion and no message, which is the worst thing this gate can
  // produce. Bound both, and take the browser out by force if it will not go.
  const browserProcess = browser.process();
  await Promise.race([
    browser.close().catch(() => {}),
    new Promise((resolve) => setTimeout(resolve, 5_000))
  ]);
  if (browserProcess && browserProcess.exitCode === null && browserProcess.signalCode === null) {
    browserProcess.kill('SIGKILL');
  }
  server.closeAllConnections?.();
  await Promise.race([
    new Promise((resolve) => server.close(resolve)),
    new Promise((resolve) => setTimeout(resolve, 5_000))
  ]);
}
