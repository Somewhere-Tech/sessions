import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import puppeteer from 'puppeteer';

const appUrl = process.env.SESSIONS_WEBAPP_URL;
const daemonEndpoint = process.env.SESSIONS_ENDPOINT;
if (!appUrl || !daemonEndpoint) {
  console.error('usage: SESSIONS_WEBAPP_URL=http://127.0.0.1:<port> SESSIONS_ENDPOINT=http://127.0.0.1:<port> node scripts/webapp-smoke.mjs');
  process.exit(2);
}

const outputDir = process.env.SESSIONS_SMOKE_DIR
  ? path.resolve(process.env.SESSIONS_SMOKE_DIR)
  : fs.mkdtempSync(path.join(os.tmpdir(), 'sessions-webapp-smoke.'));
fs.mkdirSync(outputDir, { recursive: true });

const endpoint = new URL(daemonEndpoint);
const daemonOrigin = endpoint.origin;
const scheme = endpoint.protocol === 'https:' ? 'https' : 'http';
const port = endpoint.port ? Number(endpoint.port) : (scheme === 'https' ? 443 : 80);
const server = {
  id: 'scratch-acceptance',
  name: 'Scratch acceptance daemon',
  host: endpoint.hostname,
  port,
  isDefault: false,
  scheme
};

function isSessionsListResponse(response) {
  const candidate = new URL(response.url());
  return candidate.origin === daemonOrigin && candidate.pathname === '/api/sessions' && response.status() === 200;
}

const browser = await puppeteer.launch({
  headless: true,
  args: ['--no-sandbox']
});

try {
  const page = await browser.newPage();
  await page.setViewport({ width: 1440, height: 960, deviceScaleFactor: 1 });
  const pageErrors = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));

  await page.evaluateOnNewDocument(() => {
    if (window.sessionStorage.getItem('sessions-smoke-initialized') !== '1') {
      window.localStorage.clear();
      window.sessionStorage.setItem('sessions-smoke-initialized', '1');
    }
  });
  await page.goto(appUrl, { waitUntil: 'networkidle0', timeout: 15_000 });
  await page.waitForSelector('[data-testid="connect-screen"]', { visible: true, timeout: 10_000 });
  const connectHeading = await page.$eval('#connect-title', (node) => node.textContent?.trim() ?? '');
  const connectScreenshot = path.join(outputDir, 'connect-screen.png');
  await page.screenshot({ path: connectScreenshot, fullPage: true });

  await page.evaluate((config) => {
    window.localStorage.setItem('sessions:servers', JSON.stringify([config]));
    window.localStorage.setItem('sessions:active-server', config.id);
    window.localStorage.removeItem('sessions:sessions-cache:v1');
    window.localStorage.removeItem('sessions:active-session:v1');
  }, server);

  const sessionsResponse = page.waitForResponse(
    isSessionsListResponse,
    { timeout: 15_000 }
  );
  await page.reload({ waitUntil: 'domcontentloaded', timeout: 15_000 });
  const response = await sessionsResponse;
  await page.waitForSelector('[role="tab"][data-tab-id]', { visible: true, timeout: 15_000 });
  const tabCount = await page.$$eval('[role="tab"][data-tab-id]', (nodes) => nodes.length);
  const sessionsScreenshot = path.join(outputDir, 'session-list.png');
  await page.screenshot({ path: sessionsScreenshot, fullPage: true });

  const fragmentPage = await browser.newPage();
  await fragmentPage.setViewport({ width: 1180, height: 820, deviceScaleFactor: 1 });
  await fragmentPage.evaluateOnNewDocument(() => window.localStorage.clear());
  const fragmentUrl = `${appUrl}/#endpoint=${encodeURIComponent(daemonEndpoint)}&token=fragment-scratch-token`;
  const fragmentSessionsResponse = fragmentPage.waitForResponse(
    isSessionsListResponse,
    { timeout: 15_000 }
  );
  await fragmentPage.goto(fragmentUrl, { waitUntil: 'domcontentloaded', timeout: 15_000 });
  await fragmentSessionsResponse;
  await fragmentPage.waitForSelector('[role="tab"][data-tab-id]', { visible: true, timeout: 15_000 });
  const fragmentState = await fragmentPage.evaluate(() => ({
    hash: window.location.hash,
    activeId: window.localStorage.getItem('sessions:active-server'),
    servers: JSON.parse(window.localStorage.getItem('sessions:servers') ?? '[]')
  }));
  await fragmentPage.close();

  if (pageErrors.length > 0) {
    throw new Error(`page errors: ${pageErrors.join(' | ')}`);
  }

  console.log(JSON.stringify({
    appUrl,
    daemonEndpoint,
    connectHeading,
    connectScreenshot,
    sessionsRequest: response.url(),
    sessionsStatus: response.status(),
    tabCount,
    sessionsScreenshot,
    fragmentHashAfterBootstrap: fragmentState.hash,
    fragmentServerCount: fragmentState.servers.length,
    fragmentTokenStored: fragmentState.servers[0]?.token === 'fragment-scratch-token',
    fragmentServerSelected: fragmentState.activeId === fragmentState.servers[0]?.id
  }, null, 2));
} finally {
  await browser.close();
}
