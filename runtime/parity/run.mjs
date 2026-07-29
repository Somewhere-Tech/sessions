#!/usr/bin/env node

import { createRequire } from 'node:module';
import { once } from 'node:events';
import { createServer } from 'node:net';
import {
  closeSync,
  existsSync,
  openSync,
  readFileSync,
  readdirSync
} from 'node:fs';
import { chmod, copyFile, mkdir, rm, symlink, writeFile } from 'node:fs/promises';
import { dirname, join, relative, resolve } from 'node:path';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { homedir } from 'node:os';

const require = createRequire(import.meta.url);
const parityDir = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(parityDir, '../..');
const frontendDir = join(repoRoot, 'frontend');
const nodeRuntimeDir = join(repoRoot, 'runtime', 'testdata', 'node-runtime');
const goDir = join(repoRoot, 'runtime');
const scratchDir = join(parityDir, '.scratch');
const runtimeDir = join(parityDir, '.r');
const artifactsDir = join(parityDir, 'artifacts');
const binDir = join(scratchDir, 'bin');
const frontendDist = join(scratchDir, 'frontend-dist');
const tsRuntime = join(scratchDir, 'ts-runtime');
const launchctlPath = join(parityDir, 'launchctl');
const reportPath = join(artifactsDir, 'report.json');
const screenshotPath = join(artifactsDir, 'go-frontend-smoke.png');

const HTTP_MARKER = 'HTTP_PARITY_MARKER';
const WS_MARKER = 'WS_PARITY_MARKER';
const FRONTEND_MARKER = 'FRONTEND_PARITY_MARKER';
const sleep = (ms) => new Promise((resolvePromise) => setTimeout(resolvePromise, ms));

function stable(value) {
  return JSON.stringify(value);
}

function shape(value) {
  if (value === null) return 'null';
  if (Array.isArray(value)) {
    const itemShapes = [...new Set(value.map((item) => stable(shape(item))))]
      .sort()
      .map((item) => JSON.parse(item));
    return { type: 'array', items: itemShapes };
  }
  if (typeof value === 'object') {
    const fields = {};
    for (const key of Object.keys(value).sort()) fields[key] = shape(value[key]);
    return { type: 'object', fields };
  }
  return typeof value;
}

async function freePort() {
  const server = createServer();
  server.unref();
  await new Promise((resolvePromise, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolvePromise);
  });
  const address = server.address();
  const port = typeof address === 'object' && address ? address.port : 0;
  await new Promise((resolvePromise) => server.close(resolvePromise));
  if (!port) throw new Error('failed to allocate scratch port');
  return port;
}

async function runLogged(name, command, args, options = {}) {
  const logPath = join(artifactsDir, `${name}.log`);
  const fd = openSync(logPath, 'w');
  const child = spawn(command, args, {
    cwd: options.cwd ?? repoRoot,
    env: options.env ?? process.env,
    stdio: ['ignore', fd, fd]
  });
  closeSync(fd);
  const [code, signal] = await once(child, 'exit');
  if (code !== 0) {
    throw new Error(`${name} failed (code=${code}, signal=${signal}); see ${relative(repoRoot, logPath)}`);
  }
}

function startLogged(name, command, args, options = {}) {
  const logPath = join(artifactsDir, `${name}.log`);
  const fd = openSync(logPath, 'w');
  const child = spawn(command, args, {
    cwd: options.cwd ?? repoRoot,
    env: options.env ?? process.env,
    stdio: ['ignore', fd, fd]
  });
  closeSync(fd);
  return { child, logPath };
}

async function stopProcess(record) {
  if (!record?.child || record.child.exitCode !== null) return;
  record.child.kill('SIGTERM');
  await Promise.race([once(record.child, 'exit'), sleep(3000)]);
  if (record.child.exitCode === null) {
    record.child.kill('SIGKILL');
    await Promise.race([once(record.child, 'exit'), sleep(1000)]);
  }
}

async function waitForHealth(record, port) {
  const deadline = Date.now() + 30_000;
  let lastError;
  while (Date.now() < deadline) {
    if (record.child.exitCode !== null) {
      throw new Error(`daemon exited before health check; see ${relative(repoRoot, record.logPath)}`);
    }
    try {
      const response = await fetch(`http://127.0.0.1:${port}/api/health`);
      if (response.ok) return await response.json();
    } catch (error) {
      lastError = error;
    }
    await sleep(50);
  }
  throw new Error(`health check timed out on port ${port}: ${lastError?.message ?? 'no response'}`);
}

function daemonLayout(name) {
  const root = join(runtimeDir, name);
  const home = join(root, 'h');
  const stateRoot = join(home, '.local', 'state', 'sessions');
  return {
    root,
    home,
    stateRoot,
    // Keep Unix socket paths below macOS's sockaddr_un limit. The UUID plus
    // ".sock" consumes 41 bytes, so nesting under the normal state path from
    // this deliberately long checkout name would fail with EINVAL.
    runnerState: join(root, 'r'),
    launchctlState: join(root, 'l')
  };
}

async function prepareLayout(layout) {
  await mkdir(layout.runnerState, { recursive: true, mode: 0o700 });
  await mkdir(layout.stateRoot, { recursive: true, mode: 0o700 });
  await mkdir(join(layout.home, 'Library', 'LaunchAgents'), { recursive: true, mode: 0o700 });
  await mkdir(layout.launchctlState, { recursive: true, mode: 0o700 });
  // TS auth reads the scratch HOME state root. Go keeps auth next to an
  // explicitly configured SESSIONS_STATE_DIR whose basename is not "runners".
  await writeFile(join(layout.stateRoot, 'open'), '', { mode: 0o600 });
  await writeFile(join(layout.runnerState, 'open'), '', { mode: 0o600 });
}

function daemonEnv(layout, port, extra = {}) {
  return {
    ...process.env,
    HOME: layout.home,
    SHELL: '/bin/bash',
    SESSIONS_HOST: '127.0.0.1',
    SESSIONS_PORT: String(port),
    SESSIONS_STATE_DIR: layout.runnerState,
    PARITY_LAUNCHCTL_STATE: layout.launchctlState,
    PATH: `${parityDir}:${process.env.PATH ?? '/usr/bin:/bin'}`,
    TMPDIR: join(scratchDir, 'tmp'),
    ...extra
  };
}

async function requestJSON(base, path, init) {
  const response = await fetch(`${base}${path}`, init);
  const text = await response.text();
  let body;
  try {
    body = text ? JSON.parse(text) : null;
  } catch {
    throw new Error(`${init?.method ?? 'GET'} ${path} returned non-JSON ${response.status}: ${text.slice(0, 200)}`);
  }
  return {
    response,
    body,
    signature: {
      status: response.status,
      contentType: response.headers.get('content-type'),
      body: shape(body)
    }
  };
}

async function requestSnapshot(base, id) {
  const response = await fetch(`${base}/api/sessions/${encodeURIComponent(id)}/snapshot`);
  const text = await response.text();
  return {
    text,
    signature: {
      status: response.status,
      contentType: response.headers.get('content-type'),
      xPrettySeq: shape(response.headers.get('x-sessions-seq')),
      body: shape(text)
    }
  };
}

function createBody(cwd) {
  return {
    cmd: '/bin/bash',
    args: ['--noprofile', '--norc'],
    cwd,
    cols: 100,
    rows: 30,
    env: { PS1: '', PS2: '', BASH_SILENCE_DEPRECATION_WARNING: '1' }
  };
}

async function createSession(base, cwd) {
  const result = await requestJSON(base, '/api/sessions', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(createBody(cwd))
  });
  if (result.response.status !== 201 || typeof result.body?.id !== 'string') {
    throw new Error(`session create failed: ${stable(result.body)}`);
  }
  return result;
}

async function killSession(base, id) {
  return requestJSON(base, `/api/sessions/${encodeURIComponent(id)}`, { method: 'DELETE' });
}

async function runHTTPSequence(base) {
  const actions = {};
  let id;
  let killed = false;
  try {
    const created = await createSession(base, repoRoot);
    id = created.body.id;
    actions['POST /api/sessions'] = created.signature;

    const listed = await requestJSON(base, '/api/sessions');
    actions['GET /api/sessions'] = listed.signature;

    const input = await requestJSON(base, `/api/sessions/${encodeURIComponent(id)}/input`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ data: "printf '%s%s\\n' 'HTTP_PARITY_' 'MARKER'\n" })
    });
    actions['POST /api/sessions/:id/input'] = input.signature;

    let snapshot;
    const deadline = Date.now() + 10_000;
    do {
      snapshot = await requestSnapshot(base, id);
      if (snapshot.text.includes(HTTP_MARKER)) break;
      await sleep(50);
    } while (Date.now() < deadline);
    if (!snapshot.text.includes(HTTP_MARKER)) throw new Error('HTTP marker did not reach snapshot');
    actions['GET /api/sessions/:id/snapshot'] = snapshot.signature;

    const tail = await requestJSON(base, `/api/sessions/${encodeURIComponent(id)}/events?tail=5`);
    actions['GET /api/sessions/:id/events?tail=5'] = tail.signature;

    const killedResult = await killSession(base, id);
    killed = true;
    actions['DELETE /api/sessions/:id'] = killedResult.signature;
    return { actions, marker: HTTP_MARKER, markerFound: true };
  } finally {
    if (id && !killed) {
      try { await killSession(base, id); } catch { /* best-effort cleanup */ }
    }
  }
}

function loadWebSocket() {
  const wsModule = require(join(nodeRuntimeDir, 'node_modules', 'ws'));
  return wsModule.WebSocket ?? wsModule;
}

async function runWSSequence(base, cwd) {
  const created = await createSession(base, cwd);
  const id = created.body.id;
  const WebSocket = loadWebSocket();
  let socket;
  try {
    const messages = await new Promise((resolvePromise, reject) => {
      const captured = [];
      let sawAck = false;
      let sawMarker = false;
      let inputSent = false;
      let finishTimer;
      const timeout = setTimeout(() => reject(new Error('WS sequence timed out')), 12_000);
      socket = new WebSocket(`${base.replace(/^http/, 'ws')}/ws?mux=1`);

      const maybeFinish = () => {
        if (!sawAck || !sawMarker || finishTimer) return;
        finishTimer = setTimeout(() => {
          clearTimeout(timeout);
          resolvePromise(captured);
        }, 150);
      };

      socket.on('open', () => {
        socket.send(JSON.stringify({
          type: 'attach',
          sessionId: id,
          outputReplay: true,
          claudeReplay: false,
          claudeLive: false
        }));
      });
      socket.on('message', (raw) => {
        let message;
        try { message = JSON.parse(raw.toString('utf8')); }
        catch { return; }
        captured.push({ type: message.type, shape: shape(message) });
        if (message.type === 'hello' && !inputSent) {
          inputSent = true;
          setTimeout(() => {
            socket.send(JSON.stringify({
              type: 'input',
              sessionId: id,
              requestId: 'parity-input-1',
              data: "printf '%s%s\\n' 'WS_PARITY_' 'MARKER'\n"
            }));
          }, 250);
        }
        if (message.type === 'inputAck' && message.requestId === 'parity-input-1' && message.ok === true) sawAck = true;
        if (message.type === 'output' && typeof message.data === 'string' && message.data.includes(WS_MARKER)) sawMarker = true;
        maybeFinish();
      });
      socket.on('error', (error) => {
        clearTimeout(timeout);
        reject(error);
      });
    });
    return { messages, marker: WS_MARKER, markerFound: true };
  } finally {
    try { socket?.close(); } catch { /* already closed */ }
    try { await killSession(base, id); } catch { /* best-effort cleanup */ }
  }
}

async function clickButtonByText(page, text) {
  const clicked = await page.evaluate((wanted) => {
    const button = [...document.querySelectorAll('button')]
      .find((candidate) => candidate.textContent?.trim() === wanted);
    if (!button) return false;
    button.click();
    return true;
  }, text);
  if (!clicked) throw new Error(`button not found: ${text}`);
}

async function frontendSmoke(base) {
  const puppeteer = require(join(frontendDir, 'node_modules', 'puppeteer'));
  const browser = await puppeteer.launch({
    headless: true,
    args: ['--disable-gpu', '--no-sandbox']
  });
  const consoleErrors = [];
  const apiCreates = [];
  const fsListings = [];
  const failedResponses = [];
  let page;
  try {
    page = await browser.newPage();
    page.on('console', (message) => {
      if (message.type() === 'error') consoleErrors.push(message.text());
    });
    page.on('pageerror', (error) => consoleErrors.push(error.message));
    page.on('response', (response) => {
      const request = response.request();
      if (response.status() >= 400) {
        failedResponses.push({ method: request.method(), url: response.url(), status: response.status() });
      }
      if (request.method() === 'POST' && new URL(response.url()).pathname === '/api/sessions') {
        apiCreates.push({ url: response.url(), status: response.status() });
      }
      if (request.method() === 'GET' && new URL(response.url()).pathname === '/api/fs/list') {
        fsListings.push({ url: response.url(), status: response.status() });
      }
    });
    await page.evaluateOnNewDocument(() => {
      window.localStorage.setItem('sessions:viewMode', 'terminal');
      const original = HTMLCanvasElement.prototype.getContext;
      HTMLCanvasElement.prototype.getContext = function patched(type, ...args) {
        if (type === 'webgl' || type === 'webgl2' || type === '2d') return null;
        return original.call(this, type, ...args);
      };
    });
    await page.setViewport({ width: 1440, height: 900, deviceScaleFactor: 1 });
    await page.goto(base, { waitUntil: 'networkidle0', timeout: 30_000 });
    await page.waitForFunction(() => document.body.innerText.includes('No active session'), { timeout: 15_000 });
    await clickButtonByText(page, '+ New session');
    await page.waitForSelector('.dialog', { timeout: 10_000 });
    await clickButtonByText(page, '⬛Shell');
    await page.waitForSelector('.dir-browser-path', { timeout: 10_000 });
    await page.evaluate((cwd) => {
      const input = document.querySelector('.dir-browser-path');
      if (!(input instanceof HTMLInputElement)) throw new Error('cwd input missing');
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')?.set;
      setter?.call(input, cwd);
      input.dispatchEvent(new Event('input', { bubbles: true }));
    }, repoRoot);
    await page.waitForFunction(() => {
      const cwd = document.querySelector('.dir-browser-path');
      const submit = document.querySelector('button[type="submit"]');
      return cwd instanceof HTMLInputElement && cwd.value.length > 0 && submit instanceof HTMLButtonElement && !submit.disabled;
    }, { timeout: 15_000 });
    await clickButtonByText(page, 'Start');
    await page.waitForSelector('.terminal-host .xterm-helper-textarea', { timeout: 20_000 });
    await page.waitForFunction(() => [...document.querySelectorAll('.status-text')]
      .some((node) => node.textContent === 'open'), { timeout: 15_000 });
    await page.click('.terminal-host .xterm-helper-textarea');
    await page.keyboard.type("printf '%s%s\\n' 'FRONTEND_PARITY_' 'MARKER'", { delay: 1 });
    await page.keyboard.press('Enter');
    await page.waitForFunction((marker) => [...document.querySelectorAll('.xterm-rows')]
      .some((node) => node.textContent?.includes(marker)), { timeout: 15_000 }, FRONTEND_MARKER);
    const domText = await page.evaluate(() => [...document.querySelectorAll('.xterm-rows')]
      .map((node) => node.textContent ?? '')
      .join('\n'));
    await page.screenshot({ path: screenshotPath, fullPage: true });
    if (!domText.includes(FRONTEND_MARKER)) throw new Error('frontend marker was not present in xterm DOM text');
    if (!apiCreates.some((entry) => entry.status === 201)) throw new Error('frontend did not complete a POST /api/sessions');
    return {
      marker: FRONTEND_MARKER,
      markerFound: true,
      domSelector: '.xterm-rows',
      apiCreates,
      fsListings,
      failedResponses,
      screenshot: relative(repoRoot, screenshotPath),
      consoleErrors
    };
  } finally {
    await browser.close();
    try {
      const listed = await requestJSON(base, '/api/sessions');
      for (const session of listed.body?.sessions ?? []) {
        try { await killSession(base, session.id); } catch { /* best-effort cleanup */ }
      }
    } catch { /* daemon failure is reported elsewhere */ }
  }
}

function comparisons(tsResult, goResult) {
  const result = {};
  for (const endpoint of Object.keys(tsResult.actions)) {
    const ts = tsResult.actions[endpoint];
    const go = goResult.actions[endpoint];
    result[endpoint] = { identical: stable(ts) === stable(go), ts, go };
  }
  return result;
}

function wsComparison(tsResult, goResult) {
  return {
    identical: stable(tsResult.messages) === stable(goResult.messages),
    ts: tsResult.messages,
    go: goResult.messages
  };
}

function killShimChildren(layout) {
  if (!existsSync(layout.launchctlState)) return;
  for (const name of readdirSync(layout.launchctlState)) {
    if (!name.endsWith('.json')) continue;
    try {
      const record = JSON.parse(readFileSync(join(layout.launchctlState, name), 'utf8'));
      if (!Number.isInteger(record.pid) || record.pid <= 1) continue;
      try { process.kill(-record.pid, 'SIGTERM'); }
      catch { try { process.kill(record.pid, 'SIGTERM'); } catch { /* gone */ } }
    } catch { /* malformed scratch record */ }
  }
}

async function preserveRunnerLogs(layout, daemonName) {
  if (!existsSync(layout.runnerState)) return;
  for (const name of readdirSync(layout.runnerState)) {
    if (!name.endsWith('.log') && !name.endsWith('.json')) continue;
    try {
      await copyFile(
        join(layout.runnerState, name),
        join(artifactsDir, `${daemonName}-runner-${name}`)
      );
    } catch { /* a runner may remove its state while failure cleanup races */ }
  }
}

async function main() {
  await rm(scratchDir, { recursive: true, force: true });
  await rm(runtimeDir, { recursive: true, force: true });
  await rm(artifactsDir, { recursive: true, force: true });
  await mkdir(binDir, { recursive: true });
  await mkdir(join(scratchDir, 'tmp'), { recursive: true });
  await mkdir(artifactsDir, { recursive: true });
  await chmod(launchctlPath, 0o755);

  const tsLayout = daemonLayout('ts');
  const goLayout = daemonLayout('go');
  await Promise.all([prepareLayout(tsLayout), prepareLayout(goLayout)]);

  const buildHome = join(scratchDir, 'build-home');
  const userGoPath = join(process.env.HOME ?? homedir(), 'go');
  await mkdir(buildHome, { recursive: true });
  await Promise.all([
    runLogged('build-go-sessionsd', 'go', ['build', '-o', join(binDir, 'sessionsd'), './cmd/sessionsd'], {
      cwd: goDir,
      env: {
        ...process.env,
        HOME: buildHome,
        GOPATH: userGoPath,
        GOMODCACHE: join(userGoPath, 'pkg', 'mod'),
        GOCACHE: join(scratchDir, 'go-cache'),
        TMPDIR: join(scratchDir, 'tmp')
      }
    }),
    runLogged('build-go-runner', 'go', ['build', '-o', join(binDir, 'runner'), './cmd/sessions-runner'], {
      cwd: goDir,
      env: {
        ...process.env,
        HOME: buildHome,
        GOPATH: userGoPath,
        GOMODCACHE: join(userGoPath, 'pkg', 'mod'),
        GOCACHE: join(scratchDir, 'go-cache'),
        TMPDIR: join(scratchDir, 'tmp')
      }
    }),
    runLogged('build-ts-sessionsd', join(nodeRuntimeDir, 'node_modules', '.bin', 'tsc'), [
      '-p', join(nodeRuntimeDir, 'tsconfig.json'), '--outDir', tsRuntime
    ], { cwd: repoRoot, env: { ...process.env, HOME: buildHome, TMPDIR: join(scratchDir, 'tmp') } }),
    runLogged('build-frontend', join(frontendDir, 'node_modules', '.bin', 'vite'), [
      'build', '--outDir', frontendDist, '--emptyOutDir'
    ], {
      cwd: frontendDir,
      env: { ...process.env, HOME: buildHome, VITE_BUILD_ID: 'parity-lane' }
    })
  ]);
  await writeFile(join(tsRuntime, 'package.json'), '{"type":"module"}\n');
  await symlink(join(nodeRuntimeDir, 'node_modules'), join(tsRuntime, 'node_modules'), 'dir');

  const [tsPort, goPort] = await Promise.all([freePort(), freePort()]);
  const tsBase = `http://127.0.0.1:${tsPort}`;
  const goBase = `http://127.0.0.1:${goPort}`;
  let tsDaemon;
  let goDaemon;
  let report;
  try {
    tsDaemon = startLogged('ts-daemon', '/usr/bin/env', [
      'node', join(tsRuntime, 'server.js')
    ], { cwd: repoRoot, env: daemonEnv(tsLayout, tsPort) });
    goDaemon = startLogged('go-daemon', join(binDir, 'sessionsd'), [], {
      cwd: repoRoot,
      env: daemonEnv(goLayout, goPort, {
        SESSIONS_RUNNER: join(binDir, 'runner'),
        SESSIONS_WEB_DIR: frontendDist
      })
    });
    await Promise.all([waitForHealth(tsDaemon, tsPort), waitForHealth(goDaemon, goPort)]);

    const [tsHTTP, goHTTP] = await Promise.all([runHTTPSequence(tsBase), runHTTPSequence(goBase)]);
    const [tsWS, goWS] = await Promise.all([runWSSequence(tsBase, repoRoot), runWSSequence(goBase, repoRoot)]);
    const frontend = await frontendSmoke(goBase);
    const http = comparisons(tsHTTP, goHTTP);
    const ws = wsComparison(tsWS, goWS);
    report = {
      generatedAt: new Date().toISOString(),
      isolation: {
        ts: { port: tsPort, home: relative(repoRoot, tsLayout.home), state: relative(repoRoot, tsLayout.runnerState) },
        go: { port: goPort, home: relative(repoRoot, goLayout.home), state: relative(repoRoot, goLayout.runnerState) },
        launchctl: 'parity-owned shim; no LaunchAgent registration'
      },
      http: {
        marker: HTTP_MARKER,
        markerFound: tsHTTP.markerFound && goHTTP.markerFound,
        comparisons: http
      },
      ws: {
        marker: WS_MARKER,
        markerFound: tsWS.markerFound && goWS.markerFound,
        comparison: ws
      },
      frontend,
      verdict: {
        httpIdentical: Object.values(http).every((entry) => entry.identical),
        wsIdentical: ws.identical,
        frontendPassed: frontend.markerFound && frontend.apiCreates.some((entry) => entry.status === 201)
      }
    };
    await writeFile(reportPath, JSON.stringify(report, null, 2) + '\n');
    process.stdout.write(JSON.stringify(report.verdict) + '\n');
  } catch (error) {
    await Promise.all([
      preserveRunnerLogs(tsLayout, 'ts'),
      preserveRunnerLogs(goLayout, 'go')
    ]);
    const failure = {
      generatedAt: new Date().toISOString(),
      error: error.stack ?? error.message,
      partial: report ?? null
    };
    await writeFile(reportPath, JSON.stringify(failure, null, 2) + '\n');
    throw error;
  } finally {
    await Promise.all([stopProcess(tsDaemon), stopProcess(goDaemon)]);
    killShimChildren(tsLayout);
    killShimChildren(goLayout);
    await sleep(200);
    await rm(runtimeDir, { recursive: true, force: true });
    await rm(scratchDir, { recursive: true, force: true });
  }
}

await main();
