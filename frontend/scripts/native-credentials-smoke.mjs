import assert from 'node:assert/strict';
import fs from 'node:fs';
import { rm } from 'node:fs/promises';
import http from 'node:http';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import puppeteer from 'puppeteer';
import { smoke, stableDistSnapshot, closeBrowser, closeServer } from './lib/smoke.mjs';

const t = smoke('native-credentials');
const frontendDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
// The suites below serve the real build. They serve a private snapshot of it
// rather than frontend/dist itself: `vite build` empties dist before rewriting
// it, so any concurrent build on the machine used to yank the app out from
// under a running suite — dist missing at start-up, or every asset 404ing
// mid-run, which reads exactly like a slow machine. See stableDistSnapshot.
const distDir = await stableDistSnapshot('native-credentials');

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
  t.scenario('a plaintext token in localStorage is migrated into the native vault before the first request');
  const migrated = await openCase({
    storedServer: legacyServer,
    nativeCredentials: []
  });
  await t.waitForSelector(migrated.page, '.app-shell', 'the app shell to mount for the legacy plaintext-token server', { timeout: 10_000 });
  await t.waitForFunction(migrated.page, () => document.querySelector('.empty-state') !== null, 'the empty session list to render, proving the migrated credential authorized /api/sessions');
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

  t.scenario('an existing native vault entry is used as-is, with no rewrite');
  const hydrated = await openCase({
    storedServer: { ...legacyServer, token: undefined },
    nativeCredentials: [{ serverId: legacyServer.id, token: legacyServer.token }]
  });
  await t.waitForSelector(hydrated.page, '.app-shell', 'the app shell to mount from a vault-only credential', { timeout: 10_000 });
  await t.waitForFunction(hydrated.page, () => document.querySelector('.empty-state') !== null, 'the empty session list to render using the already-stored vault credential');
  const hydratedState = await hydrated.page.evaluate(() => ({
    metadata: JSON.parse(window.localStorage.getItem('sessions:servers') ?? '[]'),
    native: window.__credentialSmokeState
  }));
  assert.equal(hydratedState.metadata[0].token, undefined);
  assert.equal(hydratedState.native.saveCalls.length, 0);
  assert.deepEqual(hydrated.authorizations, [`Bearer ${legacyServer.token}`]);
  assert.deepEqual(hydrated.pageErrors, []);
  await hydrated.page.close();

  t.scenario('a failed metadata write rolls the vault back and blocks daemon requests');
  const rolledBack = await openCase({
    storedServer: legacyServer,
    nativeCredentials: [],
    metadataWriteError: true
  });
  await t.waitForSelector(rolledBack.page, '[data-testid="connect-screen"]', 'the connect picker to appear after the metadata write failed', { timeout: 10_000 });
  await t.waitForSelector(rolledBack.page, '.connect-error', 'the rollback explanation to be rendered rather than a silent half-migration', { timeout: 10_000 });
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
  t.release();
  // Both of these used to be unbounded. `server.close()` waits for every socket
  // the browser may not have released yet, so a finished suite could hang with
  // nothing to report. A gate must fail, never wait.
  await closeBrowser(browser);
  await closeServer(server);
  // The dist snapshot is this suite's private copy; nothing else will remove it.
  await rm(distDir, { recursive: true, force: true });
}
