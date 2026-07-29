import assert from 'node:assert/strict';
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import puppeteer from 'puppeteer';

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
      } else {
        const status = health === 'unauthorized' ? 401 : 200;
        const body = status === 200 ? JSON.stringify({ name: 'sessionsd' }) : '';
        void request.respond({ status, contentType: 'application/json', body });
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
  const healthy = await openCase('healthy');
  await healthy.page.goto(origin, { waitUntil: 'domcontentloaded', timeout: 15_000 });
  await healthy.page.waitForSelector('.app-shell', { timeout: 10_000 });
  await healthy.page.waitForFunction(() => {
    const servers = JSON.parse(window.localStorage.getItem('sessions:servers') ?? '[]');
    return servers.length === 1 && window.localStorage.getItem('sessions:active-server') === servers[0].id;
  });
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

  await healthy.page.waitForFunction(() => document.querySelector('.empty-state') !== null);
  healthy.requireSessionsToken();
  await healthy.page.waitForSelector('.daemon-banner-token-input', { timeout: 6_000 });
  const runtimePrompt = await healthy.page.$eval(
    '.daemon-banner-host',
    (node) => node.textContent?.trim() ?? ''
  );
  assert.equal(runtimePrompt, origin);
  assert.deepEqual(healthy.pageErrors, []);
  await healthy.page.close();

  const unauthorized = await openCase('unauthorized');
  await unauthorized.page.goto(origin, { waitUntil: 'domcontentloaded', timeout: 15_000 });
  await unauthorized.page.waitForSelector('.daemon-banner-token-input', { timeout: 10_000 });
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

  const rejected = await openCase('reject');
  await rejected.page.goto(origin, { waitUntil: 'domcontentloaded', timeout: 15_000 });
  await rejected.page.waitForSelector('[data-testid="connect-screen"]', { timeout: 10_000 });
  const rejectedState = await rejected.page.evaluate(() => ({
    servers: JSON.parse(window.localStorage.getItem('sessions:servers') ?? '[]'),
    activeId: window.localStorage.getItem('sessions:active-server')
  }));
  assert.deepEqual(rejectedState, { servers: [], activeId: null });
  assert.deepEqual(rejected.pageErrors, []);
  await rejected.page.close();

  const fragment = await openCase('healthy');
  const fragmentUrl = `${origin}/#endpoint=${encodeURIComponent(origin)}&token=fragment-smoke-token`;
  await fragment.page.goto(fragmentUrl, { waitUntil: 'domcontentloaded', timeout: 15_000 });
  await fragment.page.waitForSelector('.app-shell', { timeout: 10_000 });
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

  const paired = await openCase('healthy', 'success');
  await paired.page.goto(`${origin}/#pair=one-time-smoke-ticket`, {
    waitUntil: 'domcontentloaded', timeout: 15_000
  });
  await paired.page.waitForSelector('.app-shell', { timeout: 10_000 });
  await paired.page.waitForFunction(() => {
    const servers = JSON.parse(window.localStorage.getItem('sessions:servers') ?? '[]');
    return servers.length === 1 && servers[0].token === 'paired-device-token';
  });
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
  await paired.page.waitForFunction(() => document.querySelector('.empty-state') !== null);
  assert.ok(paired.sessionsAuthorizations.includes('Bearer paired-device-token'));
  paired.requireSessionsToken();
  await paired.page.waitForSelector('.daemon-banner-token-input', { timeout: 6_000 });
  assert.deepEqual(paired.pageErrors, []);
  await paired.page.close();

  const expiredPair = await openCase('healthy', 'expired');
  await expiredPair.page.goto(`${origin}/#pair=expired-smoke-ticket`, {
    waitUntil: 'domcontentloaded', timeout: 15_000
  });
  await expiredPair.page.waitForSelector('[data-testid="connect-screen"]', { timeout: 10_000 });
  await expiredPair.page.waitForSelector('.connect-error', { timeout: 10_000 });
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
  await browser.close();
  await new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
}
