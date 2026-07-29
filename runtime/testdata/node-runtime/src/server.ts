import { createServer } from 'node:http';
import { config } from './config.js';
import { handleHttp } from './http.js';
import { handleUpgrade } from './ws.js';
import { discoverRunners } from './sessions.js';

// Deploy preflight: static imports above are fully evaluated before this guard
// runs, so a zero exit proves dist/server.js and its dependency graph resolve.
// Exit before creating sockets or discovering runners.
if (process.env.PRETTYD_SMOKE === '1') {
  process.exit(0);
}

// Hard reject any-interface binding. 0.0.0.0 / :: would expose prettyd
// on every network the host has — coffee shop WiFi, home LAN, anything a
// future port-forward could reach. There is NO override flag for this:
// for tailnet access, set PRETTYD_HOST to a specific tailnet IP (e.g.
// 100.x.y.z) so prettyd binds only on the Tailscale interface, which
// already provides authentication and encryption.
const ANY_HOSTS = new Set(['0.0.0.0', '::', '::0', '*']);
if (ANY_HOSTS.has(config.host)) {
  console.error(
    `\n  prettyd: refusing to bind to ${config.host}.\n` +
    `  Set PRETTYD_HOST to a specific address — 127.0.0.1 for loopback only,\n` +
    `  or a tailnet IP (100.x.y.z) for access from other devices on your tailnet.\n`
  );
  process.exit(2);
}

const server = createServer((req, res) => {
  handleHttp(req, res).catch((err) => {
    res.statusCode = 500;
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify({ error: (err as Error).message }));
  });
});

// Keep HTTP/1.1 sockets reusable long enough for polling CLI commands
// (`pretty wait`) to stay on one TCP connection instead of cycling ports.
server.keepAliveTimeout = 60_000;
server.headersTimeout = 65_000;

// Disable Nagle's algorithm on every TCP connection (HTTP + the WS upgrades
// that ride the same sockets). A keystroke echo is a tiny lone packet;
// with Nagle on, TCP holds it waiting to coalesce until an ACK arrives, and
// the receiver's delayed-ACK timer means that wait is ~40ms PER keystroke —
// the exact stall that made typing feel "so much slower than tmux" (tmux
// rides SSH, which sets TCP_NODELAY for the same reason). node-pty echo is
// sub-ms; this removes the artificial 40ms tax on top of it.
server.on('connection', (socket) => {
  socket.setNoDelay(true);
});

server.on('upgrade', handleUpgrade);

// Catch EADDRINUSE and other bind errors early so they produce a readable
// message and a clean exit instead of an uncaught exception stack trace.
// EADDRINUSE in particular happens when a second prettyd is started while
// one is already running (e.g. a tsx-watch restart that races itself).
server.on('error', (err: NodeJS.ErrnoException) => {
  if (err.code === 'EADDRINUSE') {
    console.error(
      `prettyd: port ${config.port} is already in use on ${config.host}.\n` +
      `  Is another prettyd instance running? Check with: launchctl list | grep pretty`
    );
  } else {
    console.error(`prettyd: server error (${err.code ?? err.name}): ${err.message}`);
  }
  process.exit(1);
});

server.listen(config.port, config.host, () => {
  const isLocal = config.host === '127.0.0.1' || config.host === '::1' || config.host === 'localhost';
  console.log(`prettyd listening on http://${config.host}:${config.port}`);
  if (!isLocal) {
    console.log(`  warning: bound to non-local interface; prefer loopback + frontend proxy for phone use`);
  }
  // Reattach to runners that survived the previous prettyd lifetime
  // (typical case: tsx-watch hot reload on code edit) AFTER we're already
  // accepting connections. With many historical runners the serial reattach
  // scan can take 40s+; doing it before listen() made the UI/CLI look hung
  // while the daemon was actually up. The session list is briefly partial
  // until discovery finishes (see /api/health `discovering`).
  discoverRunners().catch((err) => console.error('runner discovery failed:', (err as Error).message));
});

const shutdown = (sig: string): void => {
  console.log(`prettyd: ${sig} received, shutting down`);
  server.close(() => process.exit(0));
  setTimeout(() => process.exit(1), 2000).unref();
};
process.on('SIGINT', () => shutdown('SIGINT'));
process.on('SIGTERM', () => shutdown('SIGTERM'));
