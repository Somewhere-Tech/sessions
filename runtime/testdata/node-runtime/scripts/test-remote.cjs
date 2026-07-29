'use strict';

const assert = require('node:assert/strict');
const { EventEmitter } = require('node:events');
const test = require('node:test');
const {
  endpointFromServeJson,
  endpointFromServeText,
  formatDaemonTarget,
  isMagicDnsResolutionError,
  rootHandlerFromServeJson,
  verifyEndpoint,
  walkthroughUrl
} = require('../bin/remote.cjs');

const serveStatus = {
  TCP: { '443': { HTTPS: true } },
  Web: {
    'mac-mini.example.ts.net:443': {
      Handlers: {
        '/': { Proxy: 'http://100.86.76.84:8787' }
      }
    }
  }
};

test('formats the daemon actual IPv4 and IPv6 listen addresses', () => {
  assert.equal(formatDaemonTarget('100.86.76.84', 8787), 'http://100.86.76.84:8787');
  assert.equal(formatDaemonTarget('fd7a:115c:a1e0::1', '8787'), 'http://[fd7a:115c:a1e0::1]:8787');
  assert.throws(() => formatDaemonTarget('127.0.0.1', 70000), /invalid listen port/);
});

test('finds only the Serve endpoint proxying the requested daemon target', () => {
  assert.equal(
    endpointFromServeJson(serveStatus, 'http://100.86.76.84:8787'),
    'https://mac-mini.example.ts.net'
  );
  assert.equal(endpointFromServeJson(serveStatus, 'http://127.0.0.1:8787'), null);
  assert.deepEqual(rootHandlerFromServeJson(serveStatus), {
    endpoint: 'https://mac-mini.example.ts.net',
    proxy: 'http://100.86.76.84:8787'
  });
});

test('parses the human-readable Serve status fallback', () => {
  assert.equal(
    endpointFromServeText('https://mac-mini.example.ts.net (tailnet only)\n|-- / proxy http://127.0.0.1:8787'),
    'https://mac-mini.example.ts.net'
  );
  assert.equal(endpointFromServeText('No serve config'), null);
});

test('classifies local resolver failures as the MagicDNS gap', () => {
  assert.equal(isMagicDnsResolutionError(Object.assign(new Error('getaddrinfo ENOTFOUND'), { code: 'ENOTFOUND' })), true);
  assert.equal(isMagicDnsResolutionError(Object.assign(new Error('certificate expired'), { code: 'CERT_HAS_EXPIRED' })), false);
});

function requestReturning(status, observed) {
  return (url, _options, callback) => {
    observed.url = url.toString();
    const request = new EventEmitter();
    request.end = () => {
      queueMicrotask(() => {
        const response = new EventEmitter();
        response.statusCode = status;
        response.resume = () => {};
        callback(response);
        queueMicrotask(() => response.emit('end'));
      });
    };
    request.destroy = (error) => request.emit('error', error);
    return request;
  };
}

test('verification resolves the ts.net name and requires a real health 200', async () => {
  const observed = {};
  const lookup = async (hostname, options) => {
    observed.hostname = hostname;
    observed.lookupOptions = options;
    return [{ address: '100.64.0.1', family: 4 }];
  };
  const result = await verifyEndpoint('https://mac-mini.example.ts.net', {
    lookup,
    request: requestReturning(200, observed)
  });
  assert.equal(observed.hostname, 'mac-mini.example.ts.net');
  assert.deepEqual(observed.lookupOptions, { all: true });
  assert.equal(observed.url, 'https://mac-mini.example.ts.net/api/health');
  assert.equal(result.status, 200);

  await assert.rejects(
    verifyEndpoint('https://mac-mini.example.ts.net', {
      lookup,
      request: requestReturning(503, {})
    }),
    /returned HTTP 503/
  );
});

test('phone link pre-fills the verified endpoint', () => {
  assert.equal(
    walkthroughUrl('https://mac-mini.example.ts.net'),
    'https://pretty-pty.somewhere.site/#endpoint=https%3A%2F%2Fmac-mini.example.ts.net'
  );
});
