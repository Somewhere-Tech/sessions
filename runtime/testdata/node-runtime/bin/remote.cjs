'use strict';

const dns = require('node:dns');
const https = require('node:https');
const { spawn, spawnSync } = require('node:child_process');

const DOWNLOAD_URL = 'https://tailscale.com/download';
const DNS_ADMIN_URL = 'https://login.tailscale.com/admin/dns';
const WALKTHROUGH_URL = 'https://pretty-pty.somewhere.site/';

function commandText(result) {
  return [result.stdout, result.stderr]
    .filter(Boolean)
    .join('\n')
    .trim();
}

function runTailscale(args) {
  return spawnSync('tailscale', args, {
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
    maxBuffer: 4 * 1024 * 1024
  });
}

function runTailscaleStreaming(args) {
  return new Promise((resolve) => {
    const child = spawn('tailscale', args, { stdio: ['ignore', 'pipe', 'pipe'] });
    const stdout = [];
    const stderr = [];
    child.stdout.on('data', (chunk) => {
      stdout.push(chunk);
      process.stderr.write(chunk);
    });
    child.stderr.on('data', (chunk) => {
      stderr.push(chunk);
      process.stderr.write(chunk);
    });
    child.once('error', (error) => resolve({ status: null, stdout: '', stderr: '', error }));
    child.once('close', (status) => resolve({
      status,
      stdout: Buffer.concat(stdout).toString('utf8'),
      stderr: Buffer.concat(stderr).toString('utf8'),
      error: null
    }));
  });
}

function parseJson(text) {
  try { return JSON.parse(text); }
  catch { return null; }
}

function preflight(fail) {
  const version = runTailscale(['version']);
  if (version.error && version.error.code === 'ENOENT') {
    fail(`Tailscale is not installed. Download it: ${DOWNLOAD_URL}`, 1);
  }
  if (version.status !== 0) {
    fail(`could not run \`tailscale version\`: ${commandText(version) || version.error?.message || 'unknown error'}`, 2);
  }

  const status = runTailscale(['status', '--json']);
  const parsed = status.status === 0 ? parseJson(status.stdout) : null;
  const loggedIn = parsed && parsed.BackendState === 'Running' && parsed.Self;
  if (!loggedIn) {
    fail('Tailscale is not connected. Run: tailscale up', 1);
  }
  return parsed;
}

function normalizeProxyTarget(target) {
  try {
    const parsed = new URL(target);
    parsed.pathname = parsed.pathname.replace(/\/$/, '') || '/';
    return parsed.toString().replace(/\/$/, '');
  } catch {
    return String(target).replace(/\/$/, '');
  }
}

function endpointFromAuthority(authority) {
  try {
    return new URL(`https://${authority}`).origin;
  } catch {
    return null;
  }
}

function endpointFromServeJson(status, target) {
  if (!status || typeof status !== 'object' || !status.Web || typeof status.Web !== 'object') {
    return null;
  }

  const wantedTarget = target ? normalizeProxyTarget(target) : null;
  let firstEndpoint = null;
  for (const [authority, web] of Object.entries(status.Web)) {
    const endpoint = endpointFromAuthority(authority);
    if (!firstEndpoint && endpoint) firstEndpoint = endpoint;
    if (!wantedTarget || !web || typeof web !== 'object') continue;
    const handlers = web.Handlers;
    if (!handlers || typeof handlers !== 'object') continue;
    for (const handler of Object.values(handlers)) {
      if (handler && typeof handler === 'object' && typeof handler.Proxy === 'string' &&
          normalizeProxyTarget(handler.Proxy) === wantedTarget) {
        return endpoint;
      }
    }
  }
  return wantedTarget ? null : firstEndpoint;
}

function rootHandlerFromServeJson(status) {
  if (!status || typeof status !== 'object' || !status.Web || typeof status.Web !== 'object') {
    return null;
  }
  for (const [authority, web] of Object.entries(status.Web)) {
    const handler = web && typeof web === 'object' && web.Handlers && web.Handlers['/'];
    if (handler && typeof handler === 'object') {
      return {
        endpoint: endpointFromAuthority(authority),
        proxy: typeof handler.Proxy === 'string' ? handler.Proxy : null
      };
    }
  }
  return null;
}

function endpointFromServeText(text) {
  const match = String(text).match(/https:\/\/[^\s()]+/i);
  if (!match) return null;
  try { return new URL(match[0]).origin; }
  catch { return null; }
}

function readServeStatus(fail, target) {
  const jsonResult = runTailscale(['serve', 'status', '--json']);
  if (jsonResult.status === 0) {
    const parsed = parseJson(jsonResult.stdout);
    if (parsed) {
      const rootHandler = rootHandlerFromServeJson(parsed);
      return {
        endpoint: endpointFromServeJson(parsed, target),
        rootProxy: rootHandler && rootHandler.proxy,
        json: parsed,
        text: jsonResult.stdout.trim()
      };
    }
  }

  const textResult = runTailscale(['serve', 'status']);
  if (textResult.status !== 0) {
    fail(`could not read Tailscale Serve status: ${commandText(textResult) || 'unknown error'}`, 2);
  }
  return {
    endpoint: endpointFromServeText(textResult.stdout),
    rootProxy: null,
    json: null,
    text: textResult.stdout.trim()
  };
}

function formatDaemonTarget(host, port) {
  if (typeof host !== 'string' || !host.trim()) throw new Error('daemon health returned no listen host');
  const parsedPort = Number(port);
  if (!Number.isInteger(parsedPort) || parsedPort < 1 || parsedPort > 65535) {
    throw new Error(`daemon health returned invalid listen port: ${port}`);
  }
  const hostname = host.includes(':') && !host.startsWith('[') ? `[${host}]` : host;
  return `http://${hostname}:${parsedPort}`;
}

function isMagicDnsResolutionError(error) {
  return Boolean(error && (
    error.code === 'ENOTFOUND' ||
    error.code === 'EAI_AGAIN' ||
    /(?:getaddrinfo|name or service not known|nodename nor servname)/i.test(error.message || '')
  ));
}

function fetchHealth(endpoint, request = https.request) {
  return new Promise((resolve, reject) => {
    const healthUrl = new URL('/api/health', `${endpoint}/`);
    const req = request(healthUrl, {
      method: 'GET',
      headers: { accept: 'application/json' },
      timeout: 10_000
    }, (res) => {
      res.once('end', () => resolve({ status: res.statusCode, url: healthUrl.toString() }));
      res.resume();
    });
    req.once('timeout', () => req.destroy(Object.assign(new Error('request timed out'), { code: 'ETIMEDOUT' })));
    req.once('error', reject);
    req.end();
  });
}

async function verifyEndpoint(endpoint, dependencies = {}) {
  const lookup = dependencies.lookup || dns.promises.lookup;
  const request = dependencies.request || https.request;
  const hostname = new URL(endpoint).hostname;
  await lookup(hostname, { all: true });
  const response = await fetchHealth(endpoint, request);
  if (response.status !== 200) {
    const error = new Error(`${response.url} returned HTTP ${response.status || 'unknown'}`);
    error.code = 'BAD_HEALTH_STATUS';
    throw error;
  }
  return response;
}

function walkthroughUrl(endpoint) {
  return `${WALKTHROUGH_URL}#endpoint=${encodeURIComponent(endpoint)}`;
}

function printQr(connectUrl) {
  const qrcode = require('qrcode-terminal');
  process.stdout.write('\nScan on your phone:\n');
  qrcode.generate(connectUrl, { small: true }, (qr) => process.stdout.write(`${qr}\n`));
}

function printConnection(endpoint, target, wantJson) {
  const connectUrl = walkthroughUrl(endpoint);
  if (wantJson) {
    process.stdout.write(`${JSON.stringify({ enabled: true, verified: true, endpoint, target, connectUrl })}\n`);
    return;
  }
  process.stdout.write('\nRemote access verified (HTTP 200).\n');
  process.stdout.write(`  Endpoint: ${endpoint}\n`);
  process.stdout.write(`  Phone:    ${connectUrl}\n`);
  printQr(connectUrl);
}

function failVerification(error, endpoint, fail) {
  if (isMagicDnsResolutionError(error)) {
    fail(
      `Tailscale Serve is configured at ${endpoint}, but that .ts.net name does not resolve on this machine.\n` +
      'Enable Tailscale DNS locally with `tailscale set --accept-dns=true`, and make sure MagicDNS is enabled at\n' +
      `${DNS_ADMIN_URL}. Then retry: pretty remote status\n` +
      'Remote access was not verified; not reporting success.',
      2
    );
  }
  fail(`Tailscale Serve is configured at ${endpoint}, but HTTPS verification failed: ${error.message}\nRemote access was not verified.`, 2);
}

async function enable(options) {
  const { fail, getDaemonListen, wantJson } = options;
  const tailscaleStatus = preflight(fail);

  const listen = await getDaemonListen(tailscaleStatus);
  let target;
  try { target = formatDaemonTarget(listen.host, listen.port); }
  catch (error) { fail(error.message, 2); }

  process.stderr.write(
    'Privacy notice: enabling Tailscale HTTPS issues a public certificate.\n' +
    'Your machine’s .ts.net name will be visible in public Certificate Transparency logs.\n\n'
  );

  // Stream output while the command is running: on first use Tailscale can
  // pause for HTTPS consent, and the approval URL must be visible immediately.
  const result = await runTailscaleStreaming(['serve', '--bg', target]);
  if (result.status !== 0) {
    fail(
      'could not enable Tailscale Serve. If a one-time HTTPS approval URL appears above, open it and retry `pretty remote enable`.',
      2
    );
  }

  const serve = readServeStatus(fail, target);
  if (!serve.endpoint) {
    fail(`Tailscale Serve did not report an HTTPS endpoint proxying ${target}; remote access was not verified.`, 2);
  }

  try {
    await verifyEndpoint(serve.endpoint);
  } catch (error) {
    failVerification(error, serve.endpoint, fail);
  }
  printConnection(serve.endpoint, target, wantJson);
}

async function status(options) {
  const { fail, wantJson } = options;
  preflight(fail);
  const serve = readServeStatus(fail);
  if (!serve.endpoint) {
    if (wantJson) process.stdout.write(`${JSON.stringify({ enabled: false, verified: false })}\n`);
    else process.stdout.write('Remote access is disabled (no Tailscale Serve HTTPS endpoint).\n');
    return;
  }

  try {
    await verifyEndpoint(serve.endpoint);
  } catch (error) {
    failVerification(error, serve.endpoint, fail);
  }
  printConnection(serve.endpoint, null, wantJson);
}

function disable(options) {
  const { fail, wantJson } = options;
  preflight(fail);
  const before = readServeStatus(fail);
  if (!before.endpoint || (before.json && !before.rootProxy)) {
    if (wantJson) process.stdout.write(`${JSON.stringify({ enabled: false, changed: false })}\n`);
    else process.stdout.write('Remote access is already disabled.\n');
    return;
  }

  // Remove only the default HTTPS root handler installed by `remote enable`.
  // `tailscale serve reset` would also destroy unrelated Serve handlers.
  const result = runTailscale(['serve', '--https=443', '--set-path=/', 'off']);
  const output = commandText(result);
  if (output) process.stderr.write(`${output}\n`);
  if (result.status !== 0) fail(`could not disable Tailscale Serve: ${output || 'unknown error'}`, 2);

  const after = readServeStatus(fail);
  if (after.json ? Boolean(after.rootProxy) : after.endpoint === before.endpoint) {
    fail(`Tailscale still reports ${before.endpoint}; remote access was not disabled.`, 2);
  }
  if (wantJson) process.stdout.write(`${JSON.stringify({ enabled: false, changed: true })}\n`);
  else process.stdout.write('Remote access disabled.\n');
}

async function runRemote(action, options) {
  switch (action) {
    case 'enable': return enable(options);
    case 'disable': return disable(options);
    case 'status': return status(options);
    default: options.fail('usage: pretty remote <enable|disable|status>', 1);
  }
}

module.exports = {
  endpointFromServeJson,
  endpointFromServeText,
  formatDaemonTarget,
  isMagicDnsResolutionError,
  rootHandlerFromServeJson,
  runRemote,
  verifyEndpoint,
  walkthroughUrl
};
