import assert from 'node:assert/strict';
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import puppeteer from 'puppeteer';

const frontendDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const distDir = path.join(frontendDir, 'dist');
if (!fs.existsSync(path.join(distDir, 'index.html'))) {
  throw new Error('frontend/dist is missing; run npm --prefix frontend run build first');
}

const contentTypes = new Map([
  ['.css', 'text/css; charset=utf-8'],
  ['.html', 'text/html; charset=utf-8'],
  ['.js', 'text/javascript; charset=utf-8'],
  ['.json', 'application/json'],
  ['.png', 'image/png'],
  ['.svg', 'image/svg+xml'],
  ['.woff2', 'font/woff2']
]);

const server = http.createServer((request, response) => {
  const pathname = decodeURIComponent(new URL(request.url ?? '/', 'http://localhost').pathname);
  const relativePath = pathname === '/' ? 'index.html' : pathname.replace(/^\/+/, '');
  const filePath = path.resolve(distDir, relativePath);
  if (
    !filePath.startsWith(`${distDir}${path.sep}`)
    || !fs.existsSync(filePath)
    || !fs.statSync(filePath).isFile()
  ) {
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
const origin = `http://127.0.0.1:${address.port}`;

const browser = await puppeteer.launch({
  headless: true,
  args: ['--no-sandbox', '--disable-dev-shm-usage']
});

const legacyServer = {
  id: 'remote',
  machineId: 'machine-smoke',
  name: 'Smoke host',
  host: '127.0.0.1',
  port: address.port,
  isDefault: false,
  scheme: 'http',
  token: 'legacy-device-token'
};

async function openCase({
  storedServer,
  nativeCredentials,
  metadataWriteError = false
}) {
  const page = await browser.newPage();
  await page.setBypassServiceWorker(true);
  const events = [];
  const authorizations = [];
  const pageErrors = [];
  await page.exposeFunction('credentialSmokeEvent', (event) => events.push(event));
  page.on('pageerror', (error) => pageErrors.push(error.message));
  await page.evaluateOnNewDocument((fixture) => {
    window.localStorage.clear();
    window.localStorage.setItem('sessions:servers', JSON.stringify([fixture.storedServer]));
    window.localStorage.setItem('sessions:active-server', fixture.storedServer.id);
    const originalSetItem = Storage.prototype.setItem;
    if (fixture.metadataWriteError) {
      Storage.prototype.setItem = function setItem(key, value) {
        if (key === 'sessions:servers') {
          throw new DOMException('credential metadata write denied', 'QuotaExceededError');
        }
        return originalSetItem.call(this, key, value);
      };
    }
    const nativeState = {
      credentials: fixture.nativeCredentials.map((credential) => ({ ...credential })),
      saveCalls: []
    };
    window.__credentialSmokeState = nativeState;
    window.__TAURI_INTERNALS__ = {
      invoke: async (command, args = {}) => {
        if (command === 'native_machine_credentials_load') {
          await window.credentialSmokeEvent('credentials-load');
          return {
            supported: true,
            credentials: nativeState.credentials.map((credential) => ({ ...credential }))
          };
        }
        if (command === 'native_machine_credentials_save') {
          const credentials = (args.credentials ?? []).map((credential) => ({ ...credential }));
          nativeState.saveCalls.push(credentials);
          nativeState.credentials = credentials;
          await window.credentialSmokeEvent(`credentials-save-${nativeState.saveCalls.length}`);
          return {
            supported: true,
            credentials: credentials.map((credential) => ({ ...credential }))
          };
        }
        if (command === 'native_connection_settings') {
          return {
            port: 8787,
            runtime: {
              state: 'client-only',
              detail: 'credential smoke client',
              serviceLabel: '',
              runtimeVersion: null
            }
          };
        }
        if (command === 'set_tray_servers') return null;
        throw new Error(`unexpected native command: ${command}`);
      }
    };
  }, { storedServer, nativeCredentials, metadataWriteError });

  await page.setRequestInterception(true);
  page.on('request', (request) => {
    const requestUrl = new URL(request.url());
    if (requestUrl.origin === origin && requestUrl.pathname === '/api/sessions') {
      events.push('sessions-request');
      authorizations.push(request.headers().authorization ?? '');
      void request.respond({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ sessions: [] })
      });
      return;
    }
    void request.continue();
  });

  await page.goto(origin, { waitUntil: 'domcontentloaded', timeout: 15_000 });
  return { page, events, authorizations, pageErrors };
}

try {
  const migrated = await openCase({
    storedServer: legacyServer,
    nativeCredentials: []
  });
  await migrated.page.waitForSelector('.app-shell', { timeout: 10_000 });
  await migrated.page.waitForFunction(() => document.querySelector('.empty-state') !== null);
  const migratedState = await migrated.page.evaluate(() => ({
    metadata: JSON.parse(window.localStorage.getItem('sessions:servers') ?? '[]'),
    native: window.__credentialSmokeState
  }));
  assert.equal(migratedState.metadata[0].token, undefined);
  assert.deepEqual(migratedState.native.credentials, [{
    serverId: legacyServer.id,
    token: legacyServer.token
  }]);
  assert.deepEqual(migrated.authorizations, [`Bearer ${legacyServer.token}`]);
  assert.ok(
    migrated.events.indexOf('credentials-load') < migrated.events.indexOf('sessions-request'),
    'native credential hydration must finish before the first daemon request'
  );
  assert.deepEqual(migrated.pageErrors, []);
  await migrated.page.close();

  const hydrated = await openCase({
    storedServer: { ...legacyServer, token: undefined },
    nativeCredentials: [{ serverId: legacyServer.id, token: legacyServer.token }]
  });
  await hydrated.page.waitForSelector('.app-shell', { timeout: 10_000 });
  await hydrated.page.waitForFunction(() => document.querySelector('.empty-state') !== null);
  const hydratedState = await hydrated.page.evaluate(() => ({
    metadata: JSON.parse(window.localStorage.getItem('sessions:servers') ?? '[]'),
    native: window.__credentialSmokeState
  }));
  assert.equal(hydratedState.metadata[0].token, undefined);
  assert.equal(hydratedState.native.saveCalls.length, 0);
  assert.deepEqual(hydrated.authorizations, [`Bearer ${legacyServer.token}`]);
  assert.deepEqual(hydrated.pageErrors, []);
  await hydrated.page.close();

  const rolledBack = await openCase({
    storedServer: legacyServer,
    nativeCredentials: [],
    metadataWriteError: true
  });
  await rolledBack.page.waitForSelector('[data-testid="connect-screen"]', { timeout: 10_000 });
  await rolledBack.page.waitForSelector('.connect-error', { timeout: 10_000 });
  const rolledBackState = await rolledBack.page.evaluate(() => ({
    error: document.querySelector('.connect-error')?.textContent?.trim() ?? '',
    metadata: JSON.parse(window.localStorage.getItem('sessions:servers') ?? '[]'),
    activeId: window.localStorage.getItem('sessions:active-server'),
    rememberedConnectDisabled:
      document.querySelector('.connect-server-pick')?.disabled ?? false,
    manualConnectDisabled:
      document.querySelector('.connect-form button[type="submit"]')?.disabled ?? false,
    native: window.__credentialSmokeState
  }));
  assert.match(rolledBackState.error, /restored the previous protected credentials/i);
  assert.equal(rolledBackState.metadata[0].token, legacyServer.token);
  assert.equal(rolledBackState.activeId, legacyServer.id);
  assert.equal(rolledBackState.rememberedConnectDisabled, true);
  assert.equal(rolledBackState.manualConnectDisabled, true);
  assert.deepEqual(rolledBackState.native.credentials, []);
  assert.equal(rolledBackState.native.saveCalls.length, 2);
  assert.deepEqual(rolledBack.authorizations, []);
  assert.deepEqual(rolledBack.pageErrors, []);
  await rolledBack.page.close();

  console.log(JSON.stringify({
    migration: {
      plaintextRemoved: true,
      credentialProtectedBeforeRequest: true,
      authorizedAfterHydration: true
    },
    existingVault: {
      hydratedWithoutRewrite: true,
      authorizedAfterHydration: true
    },
    metadataFailure: {
      legacyPlaintextPreserved: true,
      protectedVaultRolledBack: true,
      daemonRequestBlocked: true,
      connectionActionsDisabled: true
    }
  }, null, 2));
} finally {
  await browser.close();
  await new Promise((resolve, reject) => {
    server.close((error) => error ? reject(error) : resolve());
  });
}
