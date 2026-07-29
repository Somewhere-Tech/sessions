import type { IncomingMessage, ServerResponse } from 'node:http';
import { randomUUID, timingSafeEqual } from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import nodePath from 'node:path';
import { fileURLToPath } from 'node:url';
import { createSession, getSession, killSession, listSessions, snapshot, writeInput, isDiscovering, deepSessionDiagnostics } from './sessions.js';
import { scanResumableSessions } from './claudeSessionScanner.js';
import { listDirectoryCandidates } from './directories.js';
import { config, getAuthToken, isAllowedOrigin, isAuthOpen } from './config.js';
import { addSubscription, getVapidPublicKey, removeSubscription } from './push.js';
import type { CreateSessionRequest } from './types.js';

const MODULE_DIR = nodePath.dirname(fileURLToPath(import.meta.url));
const DEFAULT_WEB_DIR = nodePath.resolve(MODULE_DIR, '../../frontend/dist');
const BUNDLED_WEB_DIR = nodePath.resolve(MODULE_DIR, '../web');
const WEB_DIR = nodePath.resolve(
  process.env.PRETTYD_WEB_DIR ?? (fs.existsSync(DEFAULT_WEB_DIR) ? DEFAULT_WEB_DIR : BUNDLED_WEB_DIR)
);
let loggedMissingWebDir = false;

// Reflect the request origin back as ACAO only when it is on the
// allowlist — replaces the former wildcard `*` that would have let any
// page on the user's browser silently read daemon responses. Methods and
// headers are always advertised regardless of origin (browsers need them
// for preflight even when ACAO is absent). `authorization` is listed so
// browsers allow the Bearer-token header on CORS requests.
function send(res: ServerResponse, status: number, body: unknown, corsOrigin?: string): void {
  res.statusCode = status;
  res.setHeader('Content-Type', 'application/json');
  if (corsOrigin) res.setHeader('Access-Control-Allow-Origin', corsOrigin);
  res.setHeader('Vary', 'Origin'); // response varies by Origin — don't let a cache cross-serve it
  res.setHeader('Access-Control-Allow-Methods', 'GET,POST,DELETE,OPTIONS');
  res.setHeader('Access-Control-Allow-Headers', 'content-type, authorization');
  res.end(JSON.stringify(body));
}

// Cap JSON request bodies. Session-create and input payloads are tiny
// (a command + args, or a keystroke); without a cap a single request can
// buffer unbounded memory in the daemon. Uploads use a separate 25MB path.
const MAX_JSON_BODY = 2 * 1024 * 1024;

async function readJson<T>(req: IncomingMessage): Promise<T> {
  const chunks: Buffer[] = [];
  let total = 0;
  for await (const chunk of req) {
    total += (chunk as Buffer).length;
    if (total > MAX_JSON_BODY) throw new Error('request body too large');
    chunks.push(chunk as Buffer);
  }
  const raw = Buffer.concat(chunks).toString('utf8');
  if (!raw) return {} as T;
  return JSON.parse(raw) as T;
}

// Constant-time token comparison. timingSafeEqual requires equal-length
// buffers; a length mismatch is rejected in O(1) before reaching it so
// length itself leaks nothing beyond "wrong length" (acceptable — the
// token is always 64 hex chars so any other length is trivially wrong).
function verifyToken(provided: string, expected: string): boolean {
  const a = Buffer.from(provided, 'utf8');
  const b = Buffer.from(expected, 'utf8');
  return a.length === b.length && timingSafeEqual(a, b);
}

// Returns true when the request carries a valid auth token — either the
// `Authorization: Bearer <t>` header or the `?token=<t>` query param.
function checkAuth(req: IncomingMessage, url: URL): boolean {
  // Escape hatch: if ~/.local/state/pretty-PTY/open exists, skip token auth
  // (trusted-network / Tailscale-only mode — how it ran before auth shipped).
  // Fully reversible: delete the file to re-enable tokens. The Origin
  // allowlist still applies, so cross-site (CSWSH) protection is unaffected.
  if (isAuthOpen()) return true;
  const token = getAuthToken();
  const authHeader = req.headers.authorization ?? '';
  if (authHeader.startsWith('Bearer ')) {
    if (verifyToken(authHeader.slice(7), token)) return true;
  }
  const qToken = url.searchParams.get('token') ?? '';
  if (qToken && verifyToken(qToken, token)) return true;
  return false;
}

function isStaticRequest(path: string, method: string): boolean {
  return method === 'GET' && !path.startsWith('/api/') && path !== '/api' && !path.startsWith('/ws');
}

function contentType(filePath: string): string {
  const ext = nodePath.extname(filePath).toLowerCase();
  switch (ext) {
    case '.html': return 'text/html; charset=utf-8';
    case '.js': return 'text/javascript; charset=utf-8';
    case '.css': return 'text/css; charset=utf-8';
    case '.json': return 'application/json; charset=utf-8';
    case '.svg': return 'image/svg+xml';
    case '.png': return 'image/png';
    case '.jpg':
    case '.jpeg': return 'image/jpeg';
    case '.webp': return 'image/webp';
    case '.ico': return 'image/x-icon';
    case '.webmanifest': return 'application/manifest+json; charset=utf-8';
    case '.woff': return 'font/woff';
    case '.woff2': return 'font/woff2';
    case '.ttf': return 'font/ttf';
    case '.otf': return 'font/otf';
    case '.wasm': return 'application/wasm';
    default: return 'application/octet-stream';
  }
}

function webDirExists(): boolean {
  try {
    return fs.statSync(WEB_DIR).isDirectory();
  } catch {
    if (!loggedMissingWebDir) {
      loggedMissingWebDir = true;
      console.log(`[http] frontend build dir not found; static serving disabled: ${WEB_DIR}`);
    }
    return false;
  }
}

function resolveStaticCandidate(path: string): string | null {
  let decoded: string;
  try {
    decoded = decodeURIComponent(path);
  } catch {
    return null;
  }
  const relative = decoded.replace(/^\/+/, '');
  const normalized = nodePath.normalize(relative);
  if (normalized.startsWith('..') || nodePath.isAbsolute(normalized)) return null;
  const resolved = nodePath.resolve(WEB_DIR, normalized);
  if (resolved !== WEB_DIR && !resolved.startsWith(WEB_DIR + nodePath.sep)) return null;
  return resolved;
}

function fileIfReadable(path: string): string | null {
  try {
    const st = fs.statSync(path);
    if (st.isFile()) return path;
    if (st.isDirectory()) {
      const index = nodePath.join(path, 'index.html');
      return fs.statSync(index).isFile() ? index : null;
    }
  } catch {
    return null;
  }
  return null;
}

function serveStatic(path: string, res: ServerResponse): boolean {
  if (!webDirExists()) return false;
  const candidate = resolveStaticCandidate(path);
  if (!candidate) {
    res.statusCode = 400;
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify({ error: 'invalid path' }));
    return true;
  }
  const indexPath = nodePath.join(WEB_DIR, 'index.html');
  const filePath = fileIfReadable(candidate) ?? fileIfReadable(indexPath);
  if (!filePath) return false;
  res.statusCode = 200;
  res.setHeader('Content-Type', contentType(filePath));
  fs.createReadStream(filePath)
    .on('error', (err) => {
      if (!res.headersSent) {
        res.statusCode = 500;
        res.setHeader('Content-Type', 'application/json');
      }
      res.end(JSON.stringify({ error: (err as Error).message }));
    })
    .pipe(res);
  return true;
}

export async function handleHttp(req: IncomingMessage, res: ServerResponse): Promise<void> {
  const url = new URL(req.url ?? '/', 'http://localhost');
  const path = url.pathname;
  const method = req.method ?? 'GET';

  // Echo the request's Origin back as ACAO only when it is on the
  // allowlist. Non-browser clients send no Origin and get no ACAO header
  // (which is fine — they don't enforce CORS).
  const origin = req.headers.origin;
  const corsOrigin = isAllowedOrigin(origin, config.host) ? origin : undefined;

  // Closure so every route gets the correct ACAO header without threading
  // corsOrigin through every call-site individually.
  const reply = (status: number, body: unknown): void => send(res, status, body, corsOrigin);

  if (method === 'OPTIONS') {
    // CORS preflight — must succeed without auth so the browser can
    // discover which headers are allowed before sending the real request.
    reply(204, {});
    return;
  }

  if (isStaticRequest(path, method)) {
    if (!serveStatic(path, res)) {
      reply(404, { error: 'web build not found', path: WEB_DIR });
    }
    return;
  }

  // Health probes are always unauthenticated: `pretty doctor`, load
  // balancers, and the UI connectivity indicator all need them.
  if (path === '/api/health' && method === 'GET') {
    reply(200, {
      ok: true,
      name: 'prettyd',
      version: '0.1.0',
      listen: { host: config.host, port: config.port },
      discovering: isDiscovering(),
      sessionsLoaded: listSessions({ includeExited: true }).length
    });
    return;
  }

  // Deep health for `pretty doctor` / diagnostics: daemon uptime + per-session
  // facts the daemon knows. QoS class and runner spawn-path (the throttling
  // saga's culprits) are read CLI-side from plists + ps.
  if (path === '/api/health/deep' && method === 'GET') {
    reply(200, {
      ok: true,
      name: 'prettyd',
      version: '0.1.0',
      discovering: isDiscovering(),
      sessionsLoaded: listSessions({ includeExited: true }).length,
      uptimeSec: Math.round(process.uptime()),
      sessions: deepSessionDiagnostics()
    });
    return;
  }

  // All routes below this point require a valid auth token.
  if (!checkAuth(req, url)) {
    reply(401, { error: 'unauthorized' });
    return;
  }

  if (path === '/api/push/vapid' && method === 'GET') {
    try {
      reply(200, { publicKey: getVapidPublicKey() });
    } catch (err) {
      reply(500, { error: (err as Error).message });
    }
    return;
  }

  if (path === '/api/push/subscribe' && method === 'POST') {
    try {
      const body = await readJson<unknown>(req);
      addSubscription(body);
      reply(200, { ok: true });
    } catch (err) {
      reply(400, { error: (err as Error).message });
    }
    return;
  }

  if (path === '/api/push/unsubscribe' && method === 'POST') {
    try {
      const body = await readJson<{ endpoint?: unknown }>(req);
      if (typeof body.endpoint !== 'string' || body.endpoint.length === 0) {
        reply(400, { error: 'endpoint is required' });
        return;
      }
      removeSubscription(body.endpoint);
      reply(200, { ok: true });
    } catch (err) {
      reply(400, { error: (err as Error).message });
    }
    return;
  }

  if (path === '/api/sessions' && method === 'GET') {
    const includeExited = url.searchParams.get('include_exited') === '1';
    reply(200, { sessions: listSessions({ includeExited }) });
    return;
  }

  if (path === '/api/directories' && method === 'GET') {
    reply(200, { directories: listDirectoryCandidates() });
    return;
  }

  // /api/fs/list?path=<abs> — direct directory listing for the cwd
  // picker. No "project shape" smart-filtering: every immediate child
  // of the requested path that we can stat shows up. Default path is
  // $HOME. Returned entries are sorted: dirs first, alphabetical.
  // Scoped to the user's HOME directory — realpathSync + prefix check
  // prevents path-traversal to /etc, /private, etc.
  if (path === '/api/fs/list' && method === 'GET') {
    let p = url.searchParams.get('path') ?? os.homedir();
    if (!nodePath.isAbsolute(p)) {
      reply(400, { error: 'path must be absolute' });
      return;
    }
    try {
      // Resolve symlinks + ".." so the returned canonical path is a
      // real, navigable absolute (avoids "/x/../y" living in the URL).
      let canonical: string;
      try { canonical = fs.realpathSync(p); }
      catch { canonical = nodePath.resolve(p); }

      // Reject paths outside the user's HOME. The fs/list endpoint is
      // only for the cwd picker — there is no reason to expose / or
      // /private/etc through it. realpath the home too so the comparison
      // is symlink-resolved on BOTH sides — otherwise a symlinked home
      // path component (e.g. macOS /tmp → /private/tmp, or a home on a
      // symlinked volume) makes canonical diverge from the raw homedir and
      // rejects legitimate in-home paths.
      let home: string;
      try { home = fs.realpathSync(os.homedir()); } catch { home = os.homedir(); }
      if (canonical !== home && !canonical.startsWith(home + nodePath.sep)) {
        reply(403, { error: 'path outside home directory', path: canonical });
        return;
      }

      const st = fs.statSync(canonical);
      if (!st.isDirectory()) {
        reply(400, { error: 'not a directory', path: canonical });
        return;
      }
      const names = fs.readdirSync(canonical);
      const entries = names.map((name) => {
        const full = nodePath.join(canonical, name);
        let kind: 'dir' | 'file' | 'symlink' | 'other' = 'other';
        try {
          const lst = fs.lstatSync(full);
          if (lst.isSymbolicLink()) {
            // Resolve symlink target's kind so the user can click into
            // a symlink-to-dir as if it were a real directory.
            try {
              const tst = fs.statSync(full);
              kind = tst.isDirectory() ? 'dir' : (tst.isFile() ? 'file' : 'other');
            } catch { kind = 'symlink'; }
          } else if (lst.isDirectory()) kind = 'dir';
          else if (lst.isFile()) kind = 'file';
        } catch { /* unreadable — keep kind 'other' */ }
        return { name, kind, hidden: name.startsWith('.') };
      });
      entries.sort((a, b) => {
        if ((a.kind === 'dir') !== (b.kind === 'dir')) return a.kind === 'dir' ? -1 : 1;
        return a.name.localeCompare(b.name, undefined, { sensitivity: 'base' });
      });
      const parent = canonical === home ? null : nodePath.dirname(canonical);
      reply(200, { path: canonical, parent, entries });
    } catch (err) {
      const e = err as NodeJS.ErrnoException;
      const code = e.code === 'ENOENT' ? 404 : e.code === 'EACCES' ? 403 : 500;
      reply(code, { error: e.message, code: e.code });
    }
    return;
  }

  if (path === '/api/sessions' && method === 'POST') {
    try {
      const body = await readJson<CreateSessionRequest>(req);
      const info = await createSession(body);
      reply(201, info);
    } catch (err) {
      reply(400, { error: (err as Error).message });
    }
    return;
  }

  // /api/sessions/:id
  const idMatch = /^\/api\/sessions\/([^/]+)$/.exec(path);
  if (idMatch && method === 'DELETE') {
    const id = idMatch[1]!;
    const ok = killSession(id);
    reply(ok ? 200 : 404, { ok });
    return;
  }

  // /api/sessions/:id/snapshot — current xterm-headless serialized buffer.
  // Used by the `pretty snap <id>` CLI and by the future inactive-tab
  // parser-icon detector. text/plain so curl|less prints something useful.
  //
  // Optional `?cols=N` triggers server-side ANSI-aware reflow to width N.
  // The browser ReflowedView passes its viewport width here so prose wraps
  // cleanly while the PTY itself stays at the wide canonical cols.
  const snapMatch = /^\/api\/sessions\/([^/]+)\/snapshot$/.exec(path);
  if (snapMatch && method === 'GET') {
    const id = snapMatch[1]!;
    const colsParam = url.searchParams.get('cols');
    const cols = colsParam ? Math.max(0, Number(colsParam) | 0) : 0;
    const result = await snapshot(id, cols > 0 ? { cols } : undefined);
    if (result === null) {
      reply(404, { error: 'unknown session', id });
      return;
    }
    res.statusCode = 200;
    res.setHeader('Content-Type', 'text/plain; charset=utf-8');
    // Only echo back an allowed origin — same policy as send().
    if (corsOrigin) {
      res.setHeader('Access-Control-Allow-Origin', corsOrigin);
      // Expose the seq this snapshot represents so the browser can
      // open a WebSocket with ?lastSeq=N and skip re-replaying
      // everything that's already painted into xterm.
      res.setHeader('Access-Control-Expose-Headers', 'X-Pretty-Seq');
    }
    res.setHeader('X-Pretty-Seq', String(result.seq));
    res.end(result.text);
    return;
  }

  // /api/claude-sessions — list every Claude session this user has
  // on disk, grouped by cwd. Powers the "Resume a session" picker in
  // the New Session dialog. Sourced from ~/.claude/projects/*.
  if (path === '/api/claude-sessions' && method === 'GET') {
    const list = await scanResumableSessions();
    reply(200, { sessions: list });
    return;
  }

  // /api/sessions/:id/events — return Claude JSONL events.
  //
  // Query params for incremental / windowed access (any pair is fine):
  //   ?since=<n>   start at index n in the server-side log (incremental
  //                polling — client tracks the previous nextIndex and
  //                fetches only what it hasn't seen).
  //   ?tail=<n>    return only the last n events in the selected window
  //                (cheap for chat previews; the GridView cells use tail=40).
  //   ?before=<n>  end the window before absolute index n, so clients can
  //                page older history without fetching the full ring.
  //
  // Default (no params) preserves the original behavior — returns the
  // entire log. The response always includes `nextIndex` so the client
  // can resume from there next time, and `totalCount` so it can size
  // an empty state correctly.
  //
  // Sizing context: live sessions hit 10-20 MB of JSONL events. The
  // GridView cells polling this every 2s with no params would blow
  // through a phone's CPU + bandwidth. With ?tail=40 they fetch ~50KB.
  const eventsMatch = /^\/api\/sessions\/([^/]+)\/events$/.exec(path);
  if (eventsMatch && method === 'GET') {
    const id = eventsMatch[1]!;
    const sess = getSession(id);
    if (!sess) {
      reply(404, { error: 'unknown session', id });
      return;
    }
    const log = sess.claudeEventLog;
    const base = sess.claudeEventBase;        // absolute index of log[0]
    const len = log.length;
    const total = base + len;                 // absolute count ever seen
    const sinceRaw = url.searchParams.get('since');
    const tailRaw = url.searchParams.get('tail');
    const beforeRaw = url.searchParams.get('before');
    // Local array offset. `since` is an ABSOLUTE index (matches nextIndex
    // and the WS counter) → map through base. `before` caps the window end,
    // and `tail` then caps the result to the last N before that end. With
    // since+tail, take the max so the response is at most N events AND only
    // those after `since` — the bandwidth cap still holds on incremental
    // polls (the old code silently ignored tail when since was present).
    let end = len;
    if (beforeRaw != null) {
      const n = Number(beforeRaw);
      if (Number.isFinite(n) && n >= 0) end = Math.max(0, Math.min(n - base, len));
    }
    let start = 0;
    if (sinceRaw != null) {
      const n = Number(sinceRaw);
      if (Number.isFinite(n) && n >= 0) start = Math.max(0, Math.min(n - base, end));
    }
    if (tailRaw != null) {
      const n = Number(tailRaw);
      if (Number.isFinite(n) && n > 0) start = Math.max(start, Math.max(0, end - Math.floor(n)));
    }
    if (start > end) start = end;
    const events = start === 0 && end === len ? log : log.slice(start, end);
    reply(200, {
      events,
      nextIndex: total,
      totalCount: total,
      startIndex: base + start,
      endIndex: base + end
    });
    return;
  }

  // /api/sessions/:id/input — POST { data: string } to send raw bytes.
  // Used by `pretty send` and `pretty keys`.
  const inputMatch = /^\/api\/sessions\/([^/]+)\/input$/.exec(path);
  if (inputMatch && method === 'POST') {
    const id = inputMatch[1]!;
    try {
      const body = await readJson<{ data: string }>(req);
      const ok = writeInput(id, body.data ?? '');
      reply(ok ? 200 : 404, { ok });
    } catch (err) {
      reply(400, { error: (err as Error).message });
    }
    return;
  }

  // /api/sessions/:id/upload — POST raw file body (with X-Pretty-Filename
  // and Content-Type headers describing the file). Saves to a per-user
  // uploads dir and returns the absolute path. The frontend then types
  // that path into the InputBar so Claude/Codex can read the file off
  // disk — matches the macOS Terminal.app drag-drop convention where
  // dropping a file pastes its path.
  const uploadMatch = /^\/api\/sessions\/([^/]+)\/upload$/.exec(path);
  if (uploadMatch && method === 'POST') {
    const id = uploadMatch[1]!;
    if (!getSession(id)) {
      reply(404, { error: 'unknown session', id });
      return;
    }
    try {
      const filename = String(req.headers['x-pretty-filename'] ?? 'file');
      // Restrict to a safe basename — the header is user-controlled and
      // we never want a path-traversal that escapes the uploads dir.
      const safeBase = nodePath.basename(filename).replace(/[^\w.\- ]/g, '_').slice(0, 96);
      const ext = nodePath.extname(safeBase) || '';
      const stem = nodePath.basename(safeBase, ext) || 'file';
      const uploadsDir = nodePath.join(
        os.homedir(),
        '.local', 'state', 'pretty-PTY', 'uploads'
      );
      fs.mkdirSync(uploadsDir, { recursive: true, mode: 0o700 });
      const outName = `${stem}-${randomUUID().slice(0, 8)}${ext}`;
      const outPath = nodePath.join(uploadsDir, outName);
      const chunks: Buffer[] = [];
      let total = 0;
      // Cap at 25 MB so a runaway client can't fill the disk. Claude
      // doesn't usefully consume larger images anyway.
      const MAX = 25 * 1024 * 1024;
      for await (const chunk of req) {
        const buf = chunk as Buffer;
        total += buf.length;
        if (total > MAX) {
          reply(413, { error: 'file too large', max: MAX });
          // Drain the remaining request body so the underlying socket can
          // close cleanly. Without this the client may stall waiting for
          // the server to consume the bytes it already sent.
          req.resume();
          return;
        }
        chunks.push(buf);
      }
      fs.writeFileSync(outPath, Buffer.concat(chunks), { mode: 0o600 });
      reply(200, { path: outPath, size: total });
    } catch (err) {
      reply(500, { error: (err as Error).message });
    }
    return;
  }

  reply(404, { error: 'not found', path });
}
