#!/usr/bin/env node
// `pretty` — CLI for prettyd. Designed for openclaw agents (and humans)
// to inspect and drive sessions without opening a WebSocket.
//
// Subcommands:
//   pretty ls                       List sessions (id, name, tool, cwd, working, age).
//   pretty snap <id>                Print current xterm buffer (clean text).
//   pretty snap <id> --raw          Print buffer with ANSI escapes preserved.
//   pretty send <id> <text...>      Send text + Enter to the session.
//   pretty send <id> --file P       Send UTF-8 file contents to the session.
//   pretty transcript <id>          Print user/assistant turns from events.
//   pretty keys <id> <key>          Send a control key: esc | up | down | ^c.
//   pretty new [--cwd P] [--name L] [--model M] [--effort L] [--fast]
//              [--on-idle C] [--wait-ready] [--cmd C] [args...]
//                                   Create a new session.
//   pretty model <id> <model> [--effort L]
//                                   Switch an idle Claude session.
//   pretty kill <id>                Terminate the session's runner.
//   pretty remote enable|disable|status
//                                   Manage tailnet-only HTTPS access.
//   pretty deploy [--repo P] [--no-pull] [--dry-run]
//                                   Safely update, build, preflight, and restart.
//   pretty attach <id>              Stream the session to your terminal
//                                   (raw bytes, two-way; Ctrl+Q to detach).
//   pretty help                     This.
//
// Global flags:
//   --json     Output JSON (machine-friendly) where applicable.
//   --host     prettyd host (default: 127.0.0.1).
//   --port     prettyd port (default: 8787).
// Session ids accept full ids or unique prefixes.
//
// Exit codes:
//   0 success · 1 user error (bad args, unknown id) · 2 transport error.

'use strict';

const http = require('node:http');
const https = require('node:https');
const path = require('node:path');
const url = require('node:url');
const { randomUUID } = require('node:crypto');
const os = require('node:os');
const fs = require('node:fs');

const argv = process.argv.slice(2);

function readGlobalFlag(name, fallback) {
  const i = argv.indexOf('--' + name);
  if (i < 0) return fallback;
  const v = argv[i + 1];
  argv.splice(i, 2);
  return v;
}
const HOST = readGlobalFlag('host', process.env.PRETTYD_HOST || '127.0.0.1');
const PORT = readGlobalFlag('port', process.env.PRETTYD_PORT || '8787');
const wantJson = argv.includes('--json');
if (wantJson) argv.splice(argv.indexOf('--json'), 1);

const sub = argv.shift();

const httpAgent = new http.Agent({
  keepAlive: true,
  maxSockets: 1,
  maxFreeSockets: 1
});
const httpsAgent = new https.Agent({
  keepAlive: true,
  maxSockets: 1,
  maxFreeSockets: 1
});
let apiAgentsDestroyed = false;
function destroyApiAgents() {
  if (apiAgentsDestroyed) return;
  apiAgentsDestroyed = true;
  httpAgent.destroy();
  httpsAgent.destroy();
}
process.once('exit', destroyApiAgents);

function apiTarget(p) {
  if (/^https?:\/\//i.test(HOST)) {
    const u = new URL(HOST);
    const protocol = u.protocol === 'https:' ? 'https:' : 'http:';
    const defaultPort = protocol === 'https:' ? 443 : 80;
    return {
      protocol,
      hostname: u.hostname,
      port: Number(u.port || PORT || defaultPort),
      path: p
    };
  }
  return {
    protocol: 'http:',
    hostname: HOST,
    port: Number(PORT),
    path: p
  };
}

// Auth token for communicating with an auth-enabled daemon (contract #1).
// The daemon writes a 32-byte hex token to this file on first start; the
// CLI reads it on every request so it works immediately after daemon start
// without restarting the CLI process. Returns null if the file is absent
// (older daemon without auth — safe to proceed unauthenticated).
const TOKEN_FILE = path.join(os.homedir(), '.local', 'state', 'pretty-PTY', 'token');
function readToken() {
  try { return fs.readFileSync(TOKEN_FILE, 'utf8').trim(); }
  catch { return null; }
}

function fail(msg, code = 1) {
  process.stderr.write(`pretty: ${msg}\n`);
  process.exit(code);
}

function api(method, p, body, timeoutMs) {
  return new Promise((resolve, reject) => {
    const data = body !== undefined ? Buffer.from(JSON.stringify(body)) : null;
    // Include auth token on every request; null token = older daemon without
    // auth, so we skip the header rather than sending a bare "Bearer ".
    const token = readToken();
    const headers = data
      ? { 'content-type': 'application/json', 'content-length': data.length }
      : {};
    if (token) headers['authorization'] = `Bearer ${token}`;
    const target = apiTarget(p);
    const transport = target.protocol === 'https:' ? https : http;
    const req = transport.request({
      ...target,
      method,
      agent: target.protocol === 'https:' ? httpsAgent : httpAgent,
      headers
    }, (res) => {
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => {
        const buf = Buffer.concat(chunks);
        resolve({ status: res.statusCode, headers: res.headers, body: buf });
      });
    });
    req.on('error', reject);
    if (timeoutMs) {
      req.setTimeout(timeoutMs, () => req.destroy(new Error(`request timed out after ${timeoutMs}ms`)));
    }
    if (data) req.write(data);
    req.end();
  });
}

async function getJson(p) {
  const r = await api('GET', p);
  if (r.status === 404) {
    const sid = sessionIdFromApiPath(p);
    if (sid) fail(unknownSessionMessage(sid), 1);
  }
  if (r.status >= 400) fail(`${p} → ${r.status} ${r.body.toString('utf8').slice(0, 200)}`, 2);
  return JSON.parse(r.body.toString('utf8'));
}
async function postJson(p, body) {
  const r = await api('POST', p, body);
  if (r.status === 404) {
    const sid = sessionIdFromApiPath(p);
    if (sid) fail(unknownSessionMessage(sid), 1);
  }
  if (r.status >= 400) fail(`${p} → ${r.status} ${r.body.toString('utf8').slice(0, 200)}`, 2);
  return JSON.parse(r.body.toString('utf8'));
}
async function del(p) {
  const r = await api('DELETE', p);
  if (r.status >= 400 && r.status !== 404) fail(`${p} → ${r.status}`, 2);
  return r.status === 200;
}
async function getText(p) {
  const r = await api('GET', p);
  if (r.status === 404) {
    const sid = sessionIdFromApiPath(p);
    if (sid) fail(unknownSessionMessage(sid), 1);
    return null;
  }
  if (r.status >= 400) fail(`${p} → ${r.status}`, 2);
  return r.body.toString('utf8');
}

function ageOf(createdAt) {
  const s = Math.max(0, Math.round((Date.now() - createdAt) / 1000));
  if (s < 60) return s + 's';
  const m = Math.round(s / 60);
  if (m < 60) return m + 'm';
  const h = Math.round(m / 60);
  if (h < 48) return h + 'h';
  return Math.round(h / 24) + 'd';
}

function classifyTool(cmd, args) {
  const c = (cmd || '').toLowerCase();
  if (c.endsWith('/claude') || c === 'claude') return 'claude';
  if (c.endsWith('/codex') || c === 'codex') return 'codex';
  return path.basename(c) || 'shell';
}

function shortToolName(t) {
  return t === 'claude-code' ? 'claude' : (t || 'shell');
}

function toolOfSession(s) {
  return s.tool || classifyTool(s.cmd, s.args);
}

function isConfirmableTool(tool) {
  return tool === 'claude-code' || tool === 'codex';
}

function unknownSessionMessage(idOrPrefix) {
  return `no session matching '${idOrPrefix}' — it may be a stale id after a daemon restart; run \`pretty ls\``;
}

function sessionIdFromApiPath(p) {
  const m = /^\/api\/sessions\/([^/?]+)/.exec(p);
  if (!m) return null;
  try { return decodeURIComponent(m[1]); }
  catch { return m[1]; }
}

function sessionLabel(s) {
  const parts = [shortToolName(toolOfSession(s))];
  if (s.name) parts.push(String(s.name));
  if (s.cwd) parts.push(s.cwd.replace(process.env.HOME || '', '~'));
  if (s.exited) parts.push('exited');
  else parts.push(s.working ? 'working' : 'idle');
  return parts.join(' ');
}

async function resolveSessionId(idOrPrefix) {
  const { sessions } = await getJson('/api/sessions?include_exited=1');
  const exact = sessions.find((s) => s.id === idOrPrefix);
  if (exact) return exact.id;

  const matches = sessions.filter((s) => typeof s.id === 'string' && s.id.startsWith(idOrPrefix));
  if (matches.length === 1) return matches[0].id;
  if (matches.length === 0) fail(unknownSessionMessage(idOrPrefix), 1);

  const lines = matches
    .map((s) => `  ${s.id.slice(0, 8)}  ${sessionLabel(s)}`)
    .join('\n');
  fail(`ambiguous session prefix '${idOrPrefix}' — matches:\n${lines}\nrun \`pretty ls\``, 1);
}

// Small ANSI stripper — same regex used everywhere in pretty-PTY.
const ANSI_RE = /\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07/g;
const CURSOR_FORWARD_RE = /\x1b\[(\d+)C/g;
function normalize(s) {
  return s.replace(CURSOR_FORWARD_RE, (_, n) => ' '.repeat(parseInt(n, 10)));
}

// ── JSONL event helpers ──────────────────────────────────────────────────
//
// These mirror the logic in prettyd/src/sessions.ts (isRealUserMessage) and
// prettyd/src/claudeSessionScanner.ts so the CLI can interpret raw events
// returned by GET /api/sessions/:id/events without a daemon round-trip.

// Extract the plain-text body from a structured JSONL event's message.content.
// Handles both the string form (older SDK) and the block-array form.
function extractEventText(ev) {
  const msg = ev && typeof ev === 'object' && ev.message;
  if (!msg || typeof msg !== 'object') return '';
  const c = msg.content;
  if (typeof c === 'string') return c;
  if (Array.isArray(c)) {
    return c
      .filter((b) => b && typeof b === 'object' &&
        (b.type === 'text' || b.type === 'input_text' || b.type === 'output_text') &&
        typeof b.text === 'string')
      .map((b) => b.text)
      .join('');
  }
  return '';
}

function extractEventToolCalls(ev) {
  const msg = ev && typeof ev === 'object' && ev.message;
  if (!msg || typeof msg !== 'object') return [];
  const calls = [];
  const push = (name) => {
    if (name) calls.push(String(name));
  };
  const c = msg.content;
  if (Array.isArray(c)) {
    for (const b of c) {
      if (!b || typeof b !== 'object') continue;
      if (b.type === 'tool_use' || b.type === 'server_tool_use') {
        push(b.name || b.tool_name || b.id || 'tool');
      } else if (b.type === 'function_call') {
        push(b.name || b.call_id || 'function_call');
      }
    }
  }
  if (Array.isArray(msg.tool_calls)) {
    for (const t of msg.tool_calls) {
      if (!t || typeof t !== 'object') continue;
      push((t.function && t.function.name) || t.name || t.type || 'tool');
    }
  }
  return calls;
}

// True when a JSONL event represents a real human message — not tool_result
// feedback and not the synthetic pseudo-messages tools write for their own
// bookkeeping (<command-name>, <system-reminder>, compact banners, etc.).
// Mirrors isRealUserMessage in prettyd/src/sessions.ts.
function isRealUserEvent(ev) {
  if (!ev || ev.type !== 'user') return false;
  const msg = ev.message;
  if (!msg || typeof msg !== 'object' || msg.role !== 'user') return false;
  const c = msg.content;
  // Skip tool_result-only events (system loop feedback, not human input).
  if (Array.isArray(c) && c.length > 0 && c.every((b) => b && b.type === 'tool_result')) return false;
  const tt = extractEventText(ev).trimStart();
  if (tt.startsWith('<')) return false;          // <command-name>/<system-reminder>/…
  if (tt.startsWith('Caveat:')) return false;
  if (tt.startsWith('This session is being continued')) return false;
  if (tt.startsWith('[Request interrupted')) return false;
  return true;
}

// Return the last ≤8 non-empty lines of the cleaned terminal snapshot.
// Used to check whether sent text is still sitting in the TUI composer box.
function getComposerLines(snap) {
  if (!snap) return [];
  const cleaned = normalize(snap).replace(ANSI_RE, '');
  const lines = cleaned.split('\n');
  while (lines.length > 0 && lines[lines.length - 1].trim() === '') lines.pop();
  return lines.slice(-8);
}

const SNAPSHOT_PICKER_FOOTER_RE = /Enter to select.*[↑↓].*to navigate/;
const SNAPSHOT_NUMBERED_OPTION_RE = /^\s*(?:[❯>]\s*)?\d+[\.)]\s+\S.+$/;
const SNAPSHOT_TRUST_PROMPT_RE = /\b(?:do you trust|trust (?:this|the)|trusted (?:folder|directory|workspace|project)|trust the files|only grant access to directories you trust)\b/i;
const SNAPSHOT_TRUST_CONTEXT_RE = /\b(?:folder|directory|workspace|project|files in this)\b/i;
const SNAPSHOT_UPDATE_NOTICE_RE = /\b(?:update available|new version|latest version|release notes|what'?s new|restart to update|install update|update now|press enter to continue|notice)\b/i;
const SNAPSHOT_BLOCKING_PROMPT_RE = /\b(?:press enter|hit enter|continue\?|confirm|are you sure|allow|deny|approve|permission|yes\/no|\[y\/n\]|\(y\/n\)|select|choose)\b/i;

function cleanSnapshotText(snap) {
  return normalize(snap || '').replace(ANSI_RE, '').replace(/\r/g, '');
}

function snapshotTailLines(snap, maxLines) {
  const lines = cleanSnapshotText(snap).split('\n');
  while (lines.length > 0 && lines[lines.length - 1].trim() === '') lines.pop();
  return lines.slice(-maxLines).map((line) => line.trimEnd());
}

function hasNumberedPicker(snap) {
  const lines = snapshotTailLines(snap, 44);
  let optionCount = 0;
  let hasSelectionMarker = false;
  let hasPickerLanguage = false;

  for (const line of lines) {
    const trimmed = line.trim();
    if (SNAPSHOT_PICKER_FOOTER_RE.test(trimmed)) return true;
    if (SNAPSHOT_NUMBERED_OPTION_RE.test(trimmed)) optionCount++;
    if (/^\s*[❯>]\s*\d+[\.)]\s+\S/.test(line)) hasSelectionMarker = true;
    if (/\b(?:enter to select|navigate|select|choose|resume|continue|esc to cancel)\b/i.test(trimmed)) {
      hasPickerLanguage = true;
    }
  }

  return optionCount >= 2 && (hasSelectionMarker || hasPickerLanguage);
}

function classifySnapshotComposerState(snap) {
  const tail = snapshotTailLines(snap, 44);
  const tailText = tail.join('\n');
  const fullText = cleanSnapshotText(snap);

  if (hasNumberedPicker(snap)) {
    return {
      kind: 'numbered-picker',
      title: 'Menu or picker is open',
      description: 'This session is showing a menu or picker, not a text box.'
    };
  }

  if (SNAPSHOT_TRUST_PROMPT_RE.test(tailText) && SNAPSHOT_TRUST_CONTEXT_RE.test(tailText)) {
    return {
      kind: 'trust-prompt',
      title: 'Trust prompt is open',
      description: 'This session is asking whether to trust a folder or workspace.'
    };
  }

  if (SNAPSHOT_UPDATE_NOTICE_RE.test(tailText)) {
    return {
      kind: 'update/notice-banner',
      title: 'Notice banner is open',
      description: 'This session is showing an update or notice banner before it will accept chat input.'
    };
  }

  if (SNAPSHOT_BLOCKING_PROMPT_RE.test(tailText) && fullText.trim().length > 0) {
    return {
      kind: 'unknown-blocking',
      title: 'Interactive prompt is open',
      description: 'This session appears to be waiting on a terminal prompt instead of accepting a chat message.'
    };
  }

  return {
    kind: 'normal-composer',
    title: 'Composer appears available',
    description: 'No blocking menu or prompt was detected in the terminal snapshot.'
  };
}

function isBlockingSnapshotState(state) {
  return state && state.kind !== 'normal-composer';
}

function snapshotStateCliLabel(state) {
  if (!state) return 'an interactive prompt';
  if (state.kind === 'numbered-picker') return 'a picker/menu';
  if (state.kind === 'trust-prompt') return 'a trust prompt';
  if (state.kind === 'update/notice-banner') return 'an update/notice banner';
  if (state.kind === 'unknown-blocking') return 'an interactive prompt';
  return 'the normal composer';
}

// ── sendAndConfirm ───────────────────────────────────────────────────────
//
// Core of `pretty send` for structured-event sessions: fires text + Enter
// then polls until the JSONL confirms the user event was written (→ the tool
// actually accepted the message) or the timeout expires.
//
// Anti-duplicate rule: re-presses Enter ONLY when the sent text is
// PROVABLY still visible in the composer (i.e. it never submitted).
// If the composer is clear of the text → do NOT re-press; wait for the
// JSONL event to appear (it's almost certainly just lagging).
//
// opts: { timeoutMs?: number, noWait?: boolean }
// Returns: { confirmed: true|false|null, tool?, text?, reason?,
//            textStillInComposer?, composerTail? }
//   confirmed=null  → fire-and-forget (noWait=true or non-confirmable tool)
//   confirmed=true  → JSONL user event confirmed
//   confirmed=false → timed out without confirmation
async function sendAndConfirm(id, text, opts) {
  const timeoutMs = (opts && opts.timeoutMs) || 10_000;
  const noWait    = !!(opts && opts.noWait);

  // ── 1. Baseline ──────────────────────────────────────────────────────
  const { sessions } = await getJson('/api/sessions');
  const s = sessions.find((x) => x.id === id);
  if (!s) fail(unknownSessionMessage(id), 1);

  const tool = toolOfSession(s);
  const confirmable = isConfirmableTool(tool);
  // lastUserMessageAt is a Unix-ms timestamp (or null/undefined for sessions
  // that haven't received a message yet). Treat absent as 0.
  const baseTs = (typeof s.lastUserMessageAt === 'number' ? s.lastUserMessageAt : 0);

  // Record the absolute event-log index BEFORE we send, so ?since= returns
  // only events that appeared AFTER our send — avoids confusing an old user
  // event for our new one.
  let baseNextIndex = 0;
  if (confirmable && !noWait) {
    try {
      const evData = await getJson(`/api/sessions/${encodeURIComponent(id)}/events?tail=1`);
      if (typeof evData.nextIndex === 'number') baseNextIndex = evData.nextIndex;
    } catch { /* fresh session with no JSONL yet — 0 is correct */ }
  }

  // ── 2. Fire ──────────────────────────────────────────────────────────
  // Write the text, pause so the TUI registers it, then send Enter
  // as a separate discrete keystroke (prevents the "\r" from being
  // interpreted as a newline inside the multiline box).
  const inputUrl = `/api/sessions/${encodeURIComponent(id)}/input`;
  await postJson(inputUrl, { data: text });
  await new Promise((r) => setTimeout(r, 150));
  await postJson(inputUrl, { data: '\r' });

  if (!confirmable || noWait) {
    return { confirmed: null, tool };
  }

  // ── 3. Poll for confirmation ─────────────────────────────────────────
  // Snippet for composer detection: first 25 chars of the first non-empty
  // line of the sent text. Long enough to be distinctive; short enough to
  // appear verbatim in the composer before the box wraps it.
  const snippetSource = text.split('\n').map((l) => l.trim()).find((l) => l) || text;
  const snippet = snippetSource.slice(0, 25);

  const start = Date.now();
  const pollMs = 300;
  let enterRetries = 0;
  const MAX_ENTER_RETRIES = 2; // original send + 2 re-presses = 3 total

  while (true) {
    // Primary confirmation signal: did lastUserMessageAt advance?
    // The daemon sets this only when the tool writes a real user event to the
    // JSONL, which happens exactly when the message is submitted.
    const { sessions: sess2 } = await getJson('/api/sessions');
    const s2 = sess2.find((x) => x.id === id);
    if (!s2) return { confirmed: true, reason: 'gone', text: '' };

    const newTs = typeof s2.lastUserMessageAt === 'number' ? s2.lastUserMessageAt : 0;
    if (newTs > baseTs) {
      // Confirmed. Fetch the event text for --json callers (best-effort;
      // don't let a transient fetch failure shadow the success).
      let confirmedText = '';
      try {
        const evData = await getJson(
          `/api/sessions/${encodeURIComponent(id)}/events?since=${baseNextIndex}`
        );
        const real = (evData.events || []).filter(isRealUserEvent);
        if (real.length > 0) confirmedText = extractEventText(real[real.length - 1]);
      } catch { /* best effort */ }
      return { confirmed: true, text: confirmedText };
    }

    // Timeout?
    if (Date.now() - start >= timeoutMs) {
      const snap = await getText(`/api/sessions/${encodeURIComponent(id)}/snapshot`);
      const composerLines = getComposerLines(snap);
      const composerTail = composerLines.join('\n');
      const snapshotState = classifySnapshotComposerState(snap);
      const textStillInComposer =
        snippet.length > 0 && composerLines.some((l) => l.includes(snippet));
      return { confirmed: false, reason: 'timeout', textStillInComposer, composerTail, snapshotState };
    }

    // Anti-duplicate check: should we re-press Enter?
    // ONLY if the text is PROVABLY still in the composer AND we haven't
    // exhausted retries. If the composer no longer shows the text, the
    // message almost certainly left the box — wait, do NOT re-send.
    if (enterRetries < MAX_ENTER_RETRIES) {
      const snap = await getText(`/api/sessions/${encodeURIComponent(id)}/snapshot`);
      const composerLines = getComposerLines(snap);
      const textInComposer =
        snippet.length > 0 && composerLines.some((l) => l.includes(snippet));
      if (textInComposer) {
        // Text is still sitting in the box — Enter didn't submit. Retry.
        await postJson(inputUrl, { data: '\r' });
        enterRetries++;
      }
      // Text gone from composer → do nothing (JSONL event is just lagging).
    }

    await new Promise((r) => setTimeout(r, pollMs));
  }
}

// ────────────────────────────────────────────────────────────────────────
// Subcommands.

// `pretty doctor` — per-session health: is the runner on the un-throttled
// (Interactive QoS) plist, and on the fast compiled spawn path? Surfaces the
// exact "this session is old / background-classed / running via tsx" state
// that otherwise needs shell archaeology to find. A session flagged here is
// fixed by recreating it (or a full app restart respawns every runner clean).
async function cmdDoctor() {
  const fs = require('node:fs');
  const os = require('node:os');
  const { execFileSync } = require('node:child_process');
  let nodePty;
  try {
    nodePty = require('node-pty');
    await new Promise((resolve, reject) => {
      const pty = nodePty.spawn('/usr/bin/true', [], {
        name: 'xterm-color',
        cols: 80,
        rows: 24,
        cwd: process.cwd(),
        env: process.env
      });
      const timer = setTimeout(() => {
        try { pty.kill(); } catch { /* best effort */ }
        reject(new Error('test PTY timed out'));
      }, 3_000);
      pty.onExit(({ exitCode }) => {
        clearTimeout(timer);
        if (exitCode === 0) resolve();
        else reject(new Error(`test PTY exited with status ${exitCode}`));
      });
    });
  } catch (err) {
    fail(`node-pty preflight failed: ${err.message || String(err)}; run xcode-select --install`, 2);
  }
  const agents = path.join(os.homedir(), 'Library', 'LaunchAgents');
  const ps1 = (fmt, pid) => {
    try { return execFileSync('ps', ['-o', fmt, '-p', String(pid)], { stdio: ['ignore', 'pipe', 'ignore'] }).toString().trim(); }
    catch { return ''; }
  };
  const { sessions } = await getJson('/api/sessions');
  let deep = null;
  try { deep = await getJson('/api/health/deep'); } catch { /* older daemon */ }

  const rows = sessions.map((s) => {
    let qos = 'no-plist';
    try {
      const xml = fs.readFileSync(path.join(agents, `tech.pretty-pty.runner.${s.id}.plist`), 'utf8');
      const m = xml.match(/<key>ProcessType<\/key>\s*<string>([^<]+)<\/string>/);
      qos = m ? m[1] : 'none';
    } catch { qos = 'no-plist'; }
    let spawn = 'dead?';
    if (s.pid) {
      const ppid = ps1('ppid=', s.pid);
      const pcmd = ppid ? ps1('command=', ppid) : '';
      spawn = /dist\/runner\.js/.test(pcmd) ? 'dist' : (/tsx\b/.test(pcmd) ? 'tsx-SLOW' : (pcmd ? 'other' : 'dead?'));
    }
    const ok = qos === 'Interactive' && spawn === 'dist';
    return { id: s.id, tool: s.tool || classifyTool(s.cmd, s.args), size: `${s.cols}x${s.rows}`, qos, spawn, ok };
  });

  if (wantJson) {
    process.stdout.write(JSON.stringify({ daemon: deep, sessions: rows }, null, 2) + '\n');
    return;
  }
  if (deep) {
    process.stdout.write(`daemon: ${deep.sessionsLoaded} sessions, discovering=${deep.discovering}, uptime=${deep.uptimeSec}s\n\n`);
  }
  const W = (s, n) => String(s).slice(0, n - 1).padEnd(n);
  process.stdout.write(`${W('ID', 10)}${W('TOOL', 8)}${W('SIZE', 10)}${W('QoS', 13)}${W('SPAWN', 10)}STATUS\n`);
  for (const r of rows) {
    process.stdout.write(`${W(r.id.slice(0, 8), 10)}${W(shortToolName(r.tool), 8)}${W(r.size, 10)}${W(r.qos, 13)}${W(r.spawn, 10)}${r.ok ? 'ok' : '⚠ needs recreate'}\n`);
  }
  const bad = rows.filter((r) => !r.ok);
  process.stdout.write(`\n${bad.length} of ${rows.length} sessions need recreate `);
  process.stdout.write(bad.length ? '(throttled QoS and/or slow tsx spawn — recreate them or do a full app restart for the fast path).\n' : '— all healthy (Interactive QoS, fast dist spawn).\n');
  if (bad.length) process.exitCode = 1;
}

async function cmdLs(args) {
  const includeExited = args.includes('--include-exited') || args.includes('-a') || wantJson;
  const path = includeExited ? '/api/sessions?include_exited=1' : '/api/sessions';
  const { sessions } = await getJson(path);
  if (wantJson) {
    process.stdout.write(JSON.stringify(sessions, null, 2) + '\n');
    return;
  }
  if (sessions.length === 0) {
    process.stdout.write('(no sessions)\n');
    return;
  }
  const stateOf = (s) => {
    if (s.exited) {
      const code = s.exitCode === null ? '∅' : s.exitCode;
      const sig = s.exitSignal ? ` ${s.exitSignal}` : '';
      return `exited(${code}${sig})`;
    }
    return s.working ? 'working' : 'idle';
  };
  const cols = [
    ['ID', (s) => s.id.slice(0, 8)],
    ['NAME', (s) => (s.name ? String(s.name).replace(/\s+/g, ' ').trim() : '-')],
    ['TOOL', (s) => s.tool || classifyTool(s.cmd, s.args)],
    ['CWD', (s) => s.cwd.replace(process.env.HOME || '', '~')],
    ['STATE', stateOf],
    ['AGE', (s) => ageOf(s.createdAt)],
    // When the human last typed a real message (from the structured event log).
    // '-' for sessions without event streams or before the first message. The
    // staleness signal: a session idle for days here is a cull candidate.
    ['LAST-USER', (s) => (s.lastUserMessageAt ? ageOf(s.lastUserMessageAt) : '-')],
    ['PID', (s) => String(s.pid)]
  ];
  const rows = [cols.map(([h]) => h), ...sessions.map((s) => cols.map(([, fn]) => fn(s)))];
  const widths = cols.map((_, c) => Math.max(...rows.map((r) => r[c].length)));
  for (const r of rows) {
    process.stdout.write(r.map((cell, i) => cell.padEnd(widths[i])).join('  ') + '\n');
  }
}

async function cmdSnap(id, raw) {
  if (!id) fail('usage: pretty snap <id> [--raw]');
  id = await resolveSessionId(id);
  const text = await getText(`/api/sessions/${encodeURIComponent(id)}/snapshot`);
  const out = raw ? text : normalize(text).replace(ANSI_RE, '');
  process.stdout.write(out);
  if (!out.endsWith('\n')) process.stdout.write('\n');
}

// `pretty send <id> [--no-wait] [--timeout Ns] [--file path] <text...>`
//
// For structured-event sessions (Claude/Codex): sends text + Enter then polls
// until the daemon's JSONL log confirms the message was actually submitted
// (via lastUserMessageAt advancing). Exits non-zero if confirmation times out.
//
// --no-wait reverts to fire-and-forget (no blocking, no confirmation).
// --timeout overrides the default 10s confirmation window.
// --file reads the message body from a UTF-8 file.
//
// For sessions without structured events: fire-and-forget (JSONL unavailable).
async function cmdSend(args) {
  const id = args.shift();
  if (!id) fail('usage: pretty send <id> [--no-wait] [--timeout Ns] [--file path] <text...>');

  // Strip send-specific flags before joining the remaining tokens as text.
  const noWait = args.includes('--no-wait');
  if (noWait) args.splice(args.indexOf('--no-wait'), 1);

  let timeoutMs = 10_000;
  const tIdx = args.indexOf('--timeout');
  if (tIdx >= 0 && args[tIdx + 1]) {
    timeoutMs = parseDuration(args[tIdx + 1]);
    args.splice(tIdx, 2);
  }

  let filePath = null;
  const fIdx = args.indexOf('--file');
  if (fIdx >= 0) {
    if (!args[fIdx + 1]) fail('--file needs a path', 1);
    filePath = args[fIdx + 1];
    args.splice(fIdx, 2);
  }

  let text = args.join(' ');
  if (filePath) {
    if (args.length > 0) fail('--file cannot be combined with inline text', 1);
    try { text = fs.readFileSync(filePath, 'utf8'); }
    catch (err) { fail(`could not read --file '${filePath}': ${err.message}`, 1); }
  }
  if (!text) fail('usage: pretty send <id> [--no-wait] [--timeout Ns] [--file path] <text...>');

  const resolvedId = await resolveSessionId(id);
  const result = await sendAndConfirm(resolvedId, text, { timeoutMs, noWait });

  // ── Fire-and-forget path (--no-wait or non-confirmable tool) ─────────
  if (result.confirmed === null) {
    if (!isConfirmableTool(result.tool)) {
      if (wantJson) {
        process.stdout.write(JSON.stringify({ submitted: null, tool: result.tool }) + '\n');
      } else {
        process.stdout.write(
          `sent (submission confirmation not available for tool: ${result.tool})\n`
        );
      }
    }
    // For --no-wait on a confirmable session: silent success like old behavior.
    return;
  }

  // ── Confirmed ────────────────────────────────────────────────────────
  if (result.confirmed) {
    if (wantJson) {
      const out = { submitted: true };
      if (result.text) out.text = result.text;
      process.stdout.write(JSON.stringify(out) + '\n');
    } else {
      process.stdout.write('submitted\n');
    }
    return;
  }

  // ── Timeout — not confirmed ───────────────────────────────────────────
  if (wantJson) {
    process.stdout.write(JSON.stringify({
      submitted: false,
      reason: result.reason,
      sessionState: result.snapshotState ? result.snapshotState.kind : undefined,
      sessionStateDescription: result.snapshotState ? result.snapshotState.description : undefined,
      textStillInComposer: result.textStillInComposer,
      composerTail: result.composerTail || ''
    }) + '\n');
  } else {
    process.stderr.write(`pretty send: could not confirm submission after ${timeoutMs}ms\n`);
    if (isBlockingSnapshotState(result.snapshotState)) {
      process.stderr.write(
        `  session is at ${snapshotStateCliLabel(result.snapshotState)} — not accepting a typed message; use \`pretty keys\` or the terminal view\n`
      );
      process.stderr.write(`  ${result.snapshotState.description}\n`);
    } else if (result.textStillInComposer) {
      process.stderr.write(
        '  the message is still in the composer (Enter did not submit)\n'
      );
    } else {
      process.stderr.write(
        '  sent but not confirmed — the session may still be starting; retry, or use `pretty wait` first\n'
      );
      process.stderr.write(
        '  message is no longer in the composer but no JSONL user event appeared yet\n'
      );
      process.stderr.write(
        '  (the tool may still be picking it up, or the session may be confused)\n'
      );
    }
    if (result.composerTail) {
      process.stderr.write('  composer tail:\n');
      for (const l of result.composerTail.split('\n')) {
        process.stderr.write('    ' + l + '\n');
      }
    }
  }
  process.exit(1);
}

// `pretty last <id> [--role user|assistant] [-n N]`
//
// Reads the last message(s) from the session's structured JSONL event log and
// prints them. Default: the most recent user message + most recent assistant
// message in chronological order. Useful for agents verifying receipt and
// reading the reply without attaching to the terminal stream.
async function cmdLast(args) {
  const id = args.shift();
  if (!id) fail('usage: pretty last <id> [--role user|assistant] [-n N]');

  let role = null; // null → both user and assistant
  let n = 1;       // how many of each role to return

  const roleIdx = args.indexOf('--role');
  if (roleIdx >= 0 && args[roleIdx + 1]) {
    role = args[roleIdx + 1].toLowerCase();
    if (role !== 'user' && role !== 'assistant') {
      fail('--role must be "user" or "assistant"', 1);
    }
    args.splice(roleIdx, 2);
  }

  const nIdx = args.indexOf('-n');
  if (nIdx >= 0 && args[nIdx + 1]) {
    n = parseInt(args[nIdx + 1], 10);
    if (!Number.isFinite(n) || n < 1) fail('-n must be a positive integer', 1);
    args.splice(nIdx, 2);
  }

  const resolvedId = await resolveSessionId(id);

  // Fetch enough events to find the last N of each role. A tail of
  // max(n*20, 100) is a generous heuristic that covers typical conversations.
  const tail = Math.max(n * 20, 100);
  const evData = await getJson(`/api/sessions/${encodeURIComponent(resolvedId)}/events?tail=${tail}`);
  const events = (evData && evData.events) || [];

  // Collect (event, originalIndex, role) triples to preserve chronological order.
  const matched = [];
  events.forEach((ev, i) => {
    const isUser  = isRealUserEvent(ev);
    const isAsst  = ev.type === 'assistant' &&
                    ev.message && typeof ev.message === 'object' &&
                    ev.message.role === 'assistant';
    if ((!role || role === 'user')      && isUser) matched.push({ ev, i, role: 'user' });
    if ((!role || role === 'assistant') && isAsst) matched.push({ ev, i, role: 'assistant' });
  });

  // Take the last N of each role then re-sort by original position.
  const byRole = (r) => matched.filter((m) => m.role === r).slice(-n);
  const toShow = (role
    ? byRole(role)
    : [...byRole('user'), ...byRole('assistant')].sort((a, b) => a.i - b.i));

  if (wantJson) {
    process.stdout.write(JSON.stringify(toShow.map(({ ev, role: r }) => ({
      role: r,
      text: extractEventText(ev),
      timestamp: ev.timestamp !== undefined ? ev.timestamp : null
    })), null, 2) + '\n');
    return;
  }

  if (toShow.length === 0) {
    process.stdout.write('(no messages)\n');
    return;
  }

  for (const { ev, role: r } of toShow) {
    const ts = ev.timestamp ? ageOf(Date.parse(ev.timestamp)) + ' ago' : '';
    const header = `[${r}]${ts ? '  ' + ts : ''}`;
    process.stdout.write(header + '\n');
    process.stdout.write((extractEventText(ev) || '(empty)').trimEnd());
    process.stdout.write('\n\n');
  }
}

// `pretty transcript <id>`
//
// Prints the user/assistant turns from the structured event log as readable
// text. This intentionally uses JSONL events, not terminal snapshots.
async function cmdTranscript(args) {
  const id = args.shift();
  if (!id) fail('usage: pretty transcript <id>');
  const resolvedId = await resolveSessionId(id);

  const evData = await getJson(`/api/sessions/${encodeURIComponent(resolvedId)}/events`);
  const events = (evData && evData.events) || [];
  const turns = [];

  for (const ev of events) {
    const isUser = isRealUserEvent(ev);
    const isAsst = ev.type === 'assistant' &&
                   ev.message && typeof ev.message === 'object' &&
                   ev.message.role === 'assistant';
    if (!isUser && !isAsst) continue;

    const role = isUser ? 'user' : 'assistant';
    const text = extractEventText(ev);
    const toolCalls = isAsst ? extractEventToolCalls(ev) : [];
    if (!text && toolCalls.length === 0) continue;

    const turn = {
      role,
      text,
      timestamp: ev.timestamp !== undefined ? ev.timestamp : null
    };
    if (toolCalls.length > 0) turn.toolCalls = toolCalls;
    turns.push(turn);
  }

  if (wantJson) {
    process.stdout.write(JSON.stringify(turns, null, 2) + '\n');
    return;
  }

  if (turns.length === 0) {
    process.stdout.write('(no messages)\n');
    return;
  }

  turns.forEach((turn, i) => {
    process.stdout.write(`[${turn.role}]\n`);
    const body = (turn.text || '').trimEnd();
    if (body) process.stdout.write(body + '\n');
    if (turn.toolCalls && turn.toolCalls.length > 0) {
      for (const name of turn.toolCalls) process.stdout.write(`⚙ ${name}\n`);
    }
    if (i !== turns.length - 1) process.stdout.write('\n');
  });
}

// `pretty ask <id> [--timeout Ns] [--idle Ns] [--wait-timeout Ns] <text...>`
//
// Convenience command for agent loops: send a message (with JSONL
// confirmation), wait for the tool to finish its reply (working→idle),
// then print the last assistant message. A single synchronous
// request→reply round-trip.
//
// Works for tools with structured events (Claude/Codex). For other tools use
// send + wait.
async function cmdAsk(args) {
  const id = args.shift();
  if (!id) fail('usage: pretty ask <id> [--timeout Ns] [--idle Ns] [--wait-timeout Ns] <text...>');

  let timeoutMs     = 10_000;   // max wait for JSONL confirmation of send
  let idleMs        = 2_000;    // idle threshold for "tool finished replying"
  let waitTimeoutMs = 120_000;  // max time waiting for the reply

  // Scan backwards so splice doesn't mess up upcoming indices.
  for (let i = args.length - 1; i >= 0; i--) {
    if (args[i] === '--timeout' && args[i + 1]) {
      timeoutMs = parseDuration(args[i + 1]);
      args.splice(i, 2);
    } else if (args[i] === '--idle' && args[i + 1]) {
      idleMs = parseDuration(args[i + 1]);
      args.splice(i, 2);
    } else if (args[i] === '--wait-timeout' && args[i + 1]) {
      waitTimeoutMs = parseDuration(args[i + 1]);
      args.splice(i, 2);
    }
  }

  const text = args.join(' ');
  if (!text) fail('usage: pretty ask <id> [options] <text...>');

  const resolvedId = await resolveSessionId(id);

  // ── 1. Send and confirm ──────────────────────────────────────────────
  const sendResult = await sendAndConfirm(resolvedId, text, { timeoutMs, noWait: false });

  if (sendResult.confirmed === null) {
    // Non-confirmable session — JSONL not available.
    const tool = sendResult.tool;
    if (wantJson) {
      process.stdout.write(JSON.stringify({ submitted: null, tool }) + '\n');
    } else {
      process.stderr.write(
        `pretty ask: submission confirmation not available for tool '${tool}'\n`
      );
      process.stderr.write("  use 'pretty send' + 'pretty wait' instead\n");
    }
    process.exit(1);
  }

  if (!sendResult.confirmed) {
    if (wantJson) {
      process.stdout.write(JSON.stringify({
        submitted: false,
        reason: sendResult.reason,
        sessionState: sendResult.snapshotState ? sendResult.snapshotState.kind : undefined,
        sessionStateDescription: sendResult.snapshotState ? sendResult.snapshotState.description : undefined,
        composerTail: sendResult.composerTail || ''
      }) + '\n');
    } else {
      process.stderr.write(
        `pretty ask: message not confirmed submitted (${sendResult.reason})\n`
      );
      if (isBlockingSnapshotState(sendResult.snapshotState)) {
        process.stderr.write(
          `  session is at ${snapshotStateCliLabel(sendResult.snapshotState)} — not accepting a typed message; use \`pretty keys\` or the terminal view\n`
        );
        process.stderr.write(`  ${sendResult.snapshotState.description}\n`);
      } else {
        process.stderr.write(
          '  the session may still be starting; retry, or use `pretty wait` first\n'
        );
      }
      if (sendResult.composerTail) {
        process.stderr.write(sendResult.composerTail + '\n');
      }
    }
    process.exit(1);
  }

  // ── 2. Wait for the tool to go working → idle ─────────────────────────
  // Give the tool a moment to start working before the first poll so we
  // don't exit immediately on a still-false working flag.
  await new Promise((r) => setTimeout(r, 500));

  const waitStart   = Date.now();
  const pollMs2     = Math.max(100, Math.min(idleMs / 4, 500));
  let notWorkingSince = null;
  let seenWorking     = false;

  while (true) {
    const { sessions } = await getJson('/api/sessions');
    const s2 = sessions.find((x) => x.id === resolvedId);
    if (!s2) break; // session gone — treat as done

    if (s2.working) {
      seenWorking = true;
      notWorkingSince = null;
    } else {
      if (notWorkingSince === null) notWorkingSince = Date.now();
    }

    const idleFor = notWorkingSince !== null ? Date.now() - notWorkingSince : 0;
    // Declare idle only once working was seen (so we don't exit before
    // the tool even starts) OR after a grace period (in case working flipped
    // so fast we missed it).
    const grace = 3_000;
    const elapsed = Date.now() - waitStart;
    if ((seenWorking || elapsed > grace) && idleFor >= idleMs) break;

    if (elapsed >= waitTimeoutMs) {
      if (wantJson) {
        process.stdout.write(JSON.stringify({
          submitted: true,
          reason: 'wait-timeout',
          working: s2.working
        }) + '\n');
      } else {
        process.stderr.write(
          `pretty ask: timed out waiting for reply after ${waitTimeoutMs}ms\n`
        );
      }
      process.exit(1);
    }

    await new Promise((r) => setTimeout(r, pollMs2));
  }

  // ── 3. Print last assistant message ──────────────────────────────────
  const evData = await getJson(`/api/sessions/${encodeURIComponent(resolvedId)}/events?tail=50`);
  const events = (evData && evData.events) || [];
  const assistantEvents = events.filter(
    (ev) => ev.type === 'assistant' &&
             ev.message && typeof ev.message === 'object' &&
             ev.message.role === 'assistant'
  );

  if (assistantEvents.length === 0) {
    if (wantJson) {
      process.stdout.write(JSON.stringify({ submitted: true, reply: null }) + '\n');
    } else {
      process.stdout.write('(no assistant reply found)\n');
    }
    return;
  }

  const last = assistantEvents[assistantEvents.length - 1];
  const replyText = extractEventText(last);

  if (wantJson) {
    process.stdout.write(JSON.stringify({
      submitted: true,
      reply: {
        text: replyText,
        timestamp: last.timestamp !== undefined ? last.timestamp : null
      }
    }) + '\n');
  } else {
    process.stdout.write(replyText.trimEnd());
    if (replyText && !replyText.endsWith('\n')) process.stdout.write('\n');
  }
}

const KEY_BYTES = {
  esc: '\x1b',
  escape: '\x1b',
  up: '\x1b[A',
  down: '\x1b[B',
  left: '\x1b[D',
  right: '\x1b[C',
  '^c': '\x03',
  ctrlc: '\x03',
  '^d': '\x04',
  ctrld: '\x04',
  enter: '\r',
  tab: '\t'
};
async function cmdKeys(id, key) {
  if (!id || !key) fail('usage: pretty keys <id> <esc|up|down|left|right|^c|^d|enter|tab>');
  const bytes = KEY_BYTES[key.toLowerCase()];
  if (!bytes) fail(`unknown key '${key}'. valid: ${Object.keys(KEY_BYTES).join(', ')}`);
  id = await resolveSessionId(id);
  await postJson(`/api/sessions/${encodeURIComponent(id)}/input`, { data: bytes });
}

// Tool shortcuts. `pretty new --tool claude` resolves to the same
// command + args you'd otherwise type by hand. We default to
// skip-permissions ON because that matches the New Session dialog's
// default and the day-to-day workflow — flag with --no-skip-perms to
// opt out for one-off careful runs.
const TOOL_PRESETS = {
  claude: {
    // Bare name — resolved via PATH at spawn time. The daemon's launchd env
    // includes /opt/homebrew/bin (Apple Silicon) and /usr/local/bin (Intel)
    // so this works on either Mac without hardcoding a Cellar path that
    // breaks on `brew upgrade node` or running on the other arch. (contract #6)
    cmd: 'claude',
    args: ['--dangerously-skip-permissions'],
    safeArgs: [] // --no-skip-perms → plain claude (prompts on each action)
  },
  codex: {
    cmd: 'codex', // bare name — see comment on claude above
    // Skip-perms (the default) = codex's exact twin of Claude's
    // --dangerously-skip-permissions: `--dangerously-bypass-approvals-and-
    // sandbox` — no sandbox, no approval prompts, full access. codex >=0.137
    // removed `--full-auto`, and the old `--sandbox workspace-write` still
    // boxed codex to the project; this matches Claude's full-access posture.
    // `-c check_for_update_on_startup=false` also kills codex's full-screen
    // "update available" prompt, which takes over the TUI and swallows input
    // (another silent hang mode). Orthogonal to approvals, so it's in both.
    args: ['-c', 'check_for_update_on_startup=false', '--dangerously-bypass-approvals-and-sandbox'],
    // --no-skip-perms → sandboxed to the workspace and prompts on request.
    safeArgs: ['-c', 'check_for_update_on_startup=false', '--sandbox', 'workspace-write', '--ask-for-approval', 'on-request']
  },
  shell: {
    cmd: undefined, // prettyd default = $SHELL
    args: undefined
  }
};

// When a session's command IS a known tool (codex/claude) — regardless of
// whether it arrived via --tool, --cmd, or as a positional — inject that
// tool's default args so skip-perms/full-access is the default for BOTH
// tools, every entry path. Idempotent: if the caller already passed the
// bypass/safe flags (or extra `-c` config), we don't double-add.
function applyToolDefault(body, noSkipPerms) {
  if (!body.cmd) return;
  const base = String(body.cmd).split('/').pop().toLowerCase();
  const preset = TOOL_PRESETS[base];
  if (!preset || !preset.args) return;
  const existing = body.args || [];
  const alreadyChoseMode = existing.some((a) =>
    a === '--dangerously-bypass-approvals-and-sandbox' ||
    a === '--dangerously-skip-permissions' ||
    a === '--sandbox' || a === '--ask-for-approval' || a === '--full-auto');
  if (alreadyChoseMode) return; // caller was explicit — respect it
  const def = (noSkipPerms ? preset.safeArgs : preset.args) || [];
  // Preset default args first, then any extra args the caller passed.
  body.args = def.slice().concat(existing);
  // Pin a Claude session id (prettyd's only JSONL locator) as --tool does,
  // so positional `pretty new claude` also gets a working Pretty view.
  if (base === 'claude' && !body.args.some((x) => x === '--session-id' || x === '--resume')) {
    body.args.push('--session-id', randomUUID());
  }
}

function hasArgValue(args, names) {
  return args.some((arg, i) => names.includes(arg) && typeof args[i + 1] === 'string');
}

function hasConfigValue(args, key) {
  return args.some((arg, i) =>
    (arg === '-c' || arg === '--config') &&
    typeof args[i + 1] === 'string' &&
    args[i + 1].startsWith(key + '='));
}

// Apply Pretty's tool-neutral controls after every command-resolution path
// (--tool, --cmd, and positional) has converged on body.cmd/body.args.
// Existing native flags win so callers can mix these controls with explicit
// tool arguments without receiving duplicates.
function applyAgentControls(body, controls) {
  const requested = controls.model !== undefined || controls.effort !== undefined || controls.fast;
  if (!requested) return;

  const base = body.cmd ? String(body.cmd).split('/').pop().toLowerCase() : 'shell';
  if (base !== 'claude' && base !== 'codex') {
    fail('--model, --effort, and --fast are only for claude/codex', 1);
  }
  if (base === 'claude' && controls.fast) {
    fail('--fast is not supported for claude (claude has no service tier)', 1);
  }

  const nativeArgs = body.args || (body.args = []);
  if (controls.model !== undefined && !hasArgValue(nativeArgs, ['--model', '-m'])) {
    nativeArgs.push('--model', controls.model);
  }
  if (controls.effort !== undefined) {
    if (base === 'claude' && !hasArgValue(nativeArgs, ['--effort'])) {
      nativeArgs.push('--effort', controls.effort);
    } else if (base === 'codex' && !hasConfigValue(nativeArgs, 'model_reasoning_effort')) {
      nativeArgs.push('-c', `model_reasoning_effort="${controls.effort}"`);
    }
  }
  if (controls.fast && !hasConfigValue(nativeArgs, 'service_tier')) {
    nativeArgs.push('-c', 'service_tier="priority"');
  }
}

async function cmdNew(args) {
  const body = {};
  // Strip flags one at a time; recompute indices after each splice so
  // later flags aren't off-by-N. Whatever's left is `cmd [args...]`
  // when --cmd wasn't given, or just additional args to the --cmd value
  // when it was.
  function pluck(name) {
    const i = args.indexOf(name);
    if (i < 0) return undefined;
    const v = args[i + 1];
    args.splice(i, 2);
    return v;
  }
  function hasFlag(name) {
    const i = args.indexOf(name);
    if (i < 0) return false;
    args.splice(i, 1);
    return true;
  }
  function pluckControl(name) {
    const i = args.indexOf(name);
    if (i < 0) return undefined;
    const value = args[i + 1];
    if (value === undefined || value.startsWith('--')) fail(`${name} needs a value`, 1);
    args.splice(i, 2);
    return value;
  }
  const model = pluckControl('--model');
  const effort = pluckControl('--effort');
  const fast = hasFlag('--fast');

  const cwdVal = pluck('--cwd');
  if (cwdVal !== undefined) body.cwd = cwdVal;

  const nameVal = pluck('--name');
  if (nameVal !== undefined) {
    if (nameVal.trim().length === 0) fail('--name needs a non-empty label', 1);
    body.name = nameVal.trim();
  }

  const onIdleVal = pluck('--on-idle');
  if (onIdleVal !== undefined) {
    if (onIdleVal.trim().length === 0) fail('--on-idle needs a shell command', 1);
    body.onIdle = onIdleVal;
  }

  if (hasFlag('--wait-ready')) body.waitReady = true;

  const toolVal = pluck('--tool');
  const noSkipPerms = hasFlag('--no-skip-perms');
  if (toolVal !== undefined) {
    const preset = TOOL_PRESETS[toolVal.toLowerCase()];
    if (!preset) {
      fail(`unknown --tool '${toolVal}'. valid: ${Object.keys(TOOL_PRESETS).join(', ')}`, 1);
    }
    if (preset.cmd) body.cmd = preset.cmd;
    // Skip-perms is the default; --no-skip-perms selects the preset's safe
    // variant. Per-tool because "safe" isn't just dropping a flag — codex's
    // safe mode is a different flag set (sandbox + approvals), not the
    // absence of one. (The old regex-strip silently did nothing for codex.)
    const chosenArgs = noSkipPerms ? preset.safeArgs : preset.args;
    if (chosenArgs) {
      body.args = chosenArgs.slice();
    }
    // Any leftover positional args become extra args to the tool.
    if (args.length > 0) {
      body.args = (body.args || []).concat(args);
    }
    // Pin a Claude session id so prettyd's JSONL watcher can locate the
    // conversation file (it's the ONLY locator — no mtime fallback). The
    // New Session dialog already does this; without it `pretty new --tool
    // claude` sessions get an empty Pretty view, no titles, and may boot
    // into Claude's resume picker. Skip if the caller already pinned one.
    if (toolVal.toLowerCase() === 'claude') {
      const a = body.args || (body.args = []);
      if (!a.some((x) => x === '--session-id' || x === '--resume')) {
        a.push('--session-id', randomUUID());
      }
    }
  } else {
    const cmdVal = pluck('--cmd');
    if (cmdVal !== undefined) {
      body.cmd = cmdVal;
      if (args.length > 0) body.args = args.slice();
    } else if (args.length > 0) {
      body.cmd = args[0];
      body.args = args.slice(1);
    }
    // Even WITHOUT --tool: if the command itself is a known tool
    // (`pretty new codex`, `--cmd codex`, `pretty new claude`), apply that
    // tool's skip-perms default so it's full-access by default — exactly
    // like `--tool codex`. Without this, `pretty new codex` ran bare codex
    // with its per-command approval layer ON and agents hung/crashed.
    applyToolDefault(body, noSkipPerms);
  }
  applyAgentControls(body, { model, effort, fast });
  const info = await postJson('/api/sessions', body);
  if (wantJson) {
    process.stdout.write(JSON.stringify(info, null, 2) + '\n');
  } else {
    process.stdout.write(info.id + '\n');
  }
}

async function cmdModel(args) {
  const idArg = args.shift();
  const model = args.shift();
  if (!idArg || !model) fail('usage: pretty model <session> <model> [--effort <level>]', 1);

  let effort;
  const effortIdx = args.indexOf('--effort');
  if (effortIdx >= 0) {
    effort = args[effortIdx + 1];
    if (!effort) fail('--effort needs a value', 1);
    args.splice(effortIdx, 2);
  }
  if (args.length > 0) fail('usage: pretty model <session> <model> [--effort <level>]', 1);

  const id = await resolveSessionId(idArg);
  const { sessions } = await getJson('/api/sessions?include_exited=1');
  const session = sessions.find((candidate) => candidate.id === id);
  if (!session) fail(unknownSessionMessage(idArg), 1);
  if (session.working) fail(`session is mid-turn; try when idle (pretty wait ${id})`, 1);

  const tool = toolOfSession(session);
  if (tool === 'codex') {
    fail('live model switch for codex requires an app-server-backed session (coming); use /model in the Terminal view, or respin with: pretty new --tool codex --model <m>', 1);
  }
  if (tool !== 'claude-code') {
    fail('live model switch is only supported for claude sessions', 1);
  }

  const inputPath = `/api/sessions/${encodeURIComponent(id)}/input`;
  const modelCommand = `/model ${model}`;
  await postJson(inputPath, { data: modelCommand + '\r' });
  process.stdout.write(`sent ${modelCommand}\n`);
  if (effort !== undefined) {
    await new Promise((resolve) => setTimeout(resolve, 1_000));
    const effortCommand = `/effort ${effort}`;
    await postJson(inputPath, { data: effortCommand + '\r' });
    process.stdout.write(`sent ${effortCommand}\n`);
  }
}

async function cmdKill(ids) {
  if (!ids || ids.length === 0) fail('usage: pretty kill <id> [<id>...]');
  // Accept several ids so stale sessions can be culled in one command
  // (`pretty kill a1b2 c3d4 …`). Each kill is reported individually;
  // exit 1 if any id was unknown.
  let anyFailed = false;
  for (const idArg of ids) {
    const id = await resolveSessionId(idArg);
    const ok = await del(`/api/sessions/${encodeURIComponent(id)}`);
    if (ok) {
      process.stdout.write(`killed ${id}\n`);
    } else {
      process.stderr.write(unknownSessionMessage(idArg) + '\n');
      anyFailed = true;
    }
  }
  if (anyFailed) process.exit(1);
}

// Parse 2s, 500ms, 1m, etc. into milliseconds. Bare numbers are seconds.
function parseDuration(s, fallbackMs) {
  if (!s) return fallbackMs;
  const m = /^(\d+(?:\.\d+)?)\s*(ms|s|m|h)?$/i.exec(s);
  if (!m) fail(`bad duration '${s}' — try 2s, 500ms, 1m, 30s`, 1);
  const n = parseFloat(m[1]);
  const unit = (m[2] || 's').toLowerCase();
  return unit === 'ms' ? n
    : unit === 's' ? n * 1000
    : unit === 'm' ? n * 60_000
    : n * 3_600_000;
}

async function cmdTail(args) {
  const id = args.shift();
  if (!id) fail('usage: pretty tail <id> [-f] [-n N]');
  const follow = args.includes('-f') || args.includes('--follow');
  let n = 50;
  for (let i = 0; i < args.length; i++) {
    if ((args[i] === '-n' || args[i] === '--lines') && args[i + 1]) {
      n = parseInt(args[i + 1], 10);
      if (!Number.isFinite(n) || n < 1) fail('--lines must be a positive integer', 1);
    }
  }
  const resolvedId = await resolveSessionId(id);
  // Print the current buffer's last N lines (clean, normalized).
  const text = await getText(`/api/sessions/${encodeURIComponent(resolvedId)}/snapshot`);
  const cleaned = normalize(text).replace(ANSI_RE, '');
  const lines = cleaned.split('\n');
  // Trim trailing blank rows xterm pads to fill the buffer to its row count.
  while (lines.length > 0 && lines[lines.length - 1].trim() === '') lines.pop();
  const tailLines = lines.slice(-n);
  process.stdout.write(tailLines.join('\n'));
  if (tailLines.length > 0) process.stdout.write('\n');

  if (!follow) return;

  // Live follow — open a WS and print every OUTPUT frame's data straight
  // through. ANSI escapes pass through too so colors render in the user's
  // terminal. Ctrl+C exits. The seq we anchor at is whatever is current
  // when the snapshot was taken, so there's a tiny chance of a 1-event
  // overlap — agents using this for coordination should prefer `wait`.
  const ws = (() => {
    try { return require(path.resolve(__dirname, '..', 'node_modules', 'ws')); }
    catch { fail('tail -f needs `ws` installed in prettyd/node_modules', 2); }
  })();
  // WS auth uses query param — browsers cannot set WS headers (contract #1/#5).
  const wsTok = readToken();
  const wsTokParam = wsTok ? `&token=${encodeURIComponent(wsTok)}` : '';
  const sock = new ws(`ws://${HOST}:${PORT}/ws?sessionId=${encodeURIComponent(resolvedId)}${wsTokParam}`);
  sock.on('message', (raw) => {
    let m;
    try { m = JSON.parse(raw.toString()); } catch { return; }
    if (m.type === 'output') process.stdout.write(m.data);
    if (m.type === 'exit') {
      process.stdout.write(`\n[session exited code=${m.code ?? '∅'}]\n`);
      process.exit(0);
    }
  });
  sock.on('close', () => process.exit(0));
  destroyApiAgents();
  // Keep the process alive on the WS event loop. Don't return.
  await new Promise(() => {});
}

async function cmdWait(args) {
  const id = args.shift();
  if (!id) fail('usage: pretty wait <id> [--idle 2s] [--timeout 30s]');
  let idleMs = 2000;
  let timeoutMs = 30_000;
  for (let i = 0; i < args.length; i++) {
    if (args[i] === '--idle' && args[i + 1]) idleMs = parseDuration(args[i + 1]);
    if (args[i] === '--timeout' && args[i + 1]) timeoutMs = parseDuration(args[i + 1]);
  }
  const resolvedId = await resolveSessionId(id);
  // "Done with its turn" detection. For structured-event tools (Claude Code
  // and Codex) we key off the `working` flag — NOT byte rate. This is the
  // honest signal: a custom statusline (e.g. `/goal active (3d)◎`) repaints
  // forever and keeps `lastDataAt` fresh, so the old lastDataAt-based wait
  // would never return for an idle agent session.
  // We return once `working` has stayed false for idleMs continuously.
  //
  // Other sessions (bash, …) have no such flag, so we fall back to the
  // byte-rate lastDataAt heuristic.
  const start = Date.now();
  const pollInterval = Math.max(100, Math.min(idleMs / 4, 500));
  let notWorkingSince = null; // structured-event path: when `working` last went false
  while (true) {
    const { sessions } = await getJson('/api/sessions');
    const s = sessions.find((x) => x.id === resolvedId);
    if (!s) {
      // Treat "session gone" as an exit — the user's caller usually wants
      // to know the session is no longer running, not error out.
      if (wantJson) process.stdout.write(JSON.stringify({ ok: true, reason: 'gone' }) + '\n');
      else process.stdout.write('gone\n');
      return;
    }
    let idleFor;
    if (isConfirmableTool(toolOfSession(s))) {
      if (s.working) notWorkingSince = null;
      else if (notWorkingSince === null) notWorkingSince = Date.now();
      idleFor = notWorkingSince === null ? 0 : Date.now() - notWorkingSince;
    } else {
      idleFor = Date.now() - (s.lastDataAt || s.createdAt);
    }
    if (idleFor >= idleMs) {
      const result = { ok: true, reason: 'idle', idleMs: idleFor, working: s.working };
      if (wantJson) process.stdout.write(JSON.stringify(result) + '\n');
      else process.stdout.write(`idle for ${idleFor}ms\n`);
      return;
    }
    if (Date.now() - start >= timeoutMs) {
      const result = { ok: false, reason: 'timeout', idleMs: idleFor, working: s.working };
      if (wantJson) process.stdout.write(JSON.stringify(result) + '\n');
      else process.stderr.write(`timeout: still active after ${timeoutMs}ms (last ${idleFor}ms ago)\n`);
      process.exit(1);
    }
    await new Promise((r) => setTimeout(r, pollInterval));
  }
}

async function cmdAttach(id) {
  if (!id) fail('usage: pretty attach <id>  (Ctrl+Q to detach)');
  id = await resolveSessionId(id);
  const WebSocket = (() => {
    try { return require(path.resolve(__dirname, '..', 'node_modules', 'ws')); }
    catch { fail('attach needs `ws` installed in prettyd/node_modules', 2); }
  })();
  // WS auth uses query param — browsers cannot set WS headers (contract #1/#5).
  const wsTok = readToken();
  const wsTokParam = wsTok ? `&token=${encodeURIComponent(wsTok)}` : '';
  const sock = new WebSocket(`ws://${HOST}:${PORT}/ws?sessionId=${encodeURIComponent(id)}${wsTokParam}`);
  process.stdin.setRawMode(true);
  process.stdin.resume();
  const onStdin = (chunk) => {
    // Ctrl+Q (\x11) → detach without killing the session.
    if (chunk.includes(0x11)) {
      cleanup();
      return;
    }
    if (sock.readyState === sock.OPEN) {
      sock.send(JSON.stringify({ type: 'input', data: chunk.toString('utf8') }));
    }
  };
  function cleanup() {
    process.stdin.removeListener('data', onStdin);
    try { process.stdin.setRawMode(false); } catch {}
    process.stdin.pause();
    try { sock.close(); } catch {}
    process.exit(0);
  }
  sock.on('open', () => {
    const { rows, columns } = process.stdout;
    if (rows && columns) sock.send(JSON.stringify({ type: 'resize', cols: columns, rows }));
    process.stdin.on('data', onStdin);
  });
  sock.on('message', (raw) => {
    let msg;
    try { msg = JSON.parse(raw.toString()); } catch { return; }
    if (msg.type === 'output') process.stdout.write(msg.data);
    if (msg.type === 'exit') { process.stdout.write(`\n[session exited code=${msg.code ?? '∅'}]\n`); cleanup(); }
  });
  sock.on('close', () => cleanup());
  process.stdout.on('resize', () => {
    if (sock.readyState === sock.OPEN) {
      sock.send(JSON.stringify({ type: 'resize', cols: process.stdout.columns, rows: process.stdout.rows }));
    }
  });
}

// `pretty token` — print the daemon's auth token so the user can paste
// it into the web UI's server-settings dialog. The daemon generates the
// token on first start and writes it to TOKEN_FILE (mode 0600). This
// command simply reads and surfaces that file value.
function cmdToken() {
  const tok = readToken();
  if (!tok) {
    process.stderr.write(`pretty: no token found at ${TOKEN_FILE}\n`);
    process.stderr.write('        start the daemon first (or run: pretty install), then retry.\n');
    process.exit(1);
  }
  process.stdout.write(tok + '\n');
}

// `pretty deploy` — the one supported path for updating a running checkout.
// Dependency installation is deliberately unconditional and happens before
// either build and, most importantly, before launchd is allowed to restart the
// daemon. The smoke import then proves every dist/server.js import resolves.
async function cmdDeploy(args) {
  const { spawnSync } = require('node:child_process');

  let repoOverride = null;
  let noPull = false;
  let dryRun = false;
  for (let i = 0; i < args.length; i++) {
    const arg = args[i];
    if (arg === '--repo') {
      if (repoOverride !== null || !args[i + 1] || args[i + 1].startsWith('--')) {
        fail('usage: pretty deploy [--repo <dir>] [--no-pull] [--dry-run]');
      }
      repoOverride = args[++i];
    } else if (arg === '--no-pull') {
      if (noPull) fail('--no-pull may only be specified once');
      noPull = true;
    } else if (arg === '--dry-run') {
      if (dryRun) fail('--dry-run may only be specified once');
      dryRun = true;
    } else {
      fail(`unknown deploy option: ${arg}\nusage: pretty deploy [--repo <dir>] [--no-pull] [--dry-run]`);
    }
  }

  const shellQuote = (value) => {
    const s = String(value);
    if (/^[A-Za-z0-9_./:@%+=,-]+$/.test(s)) return s;
    return `'${s.replace(/'/g, `'"'"'`)}'`;
  };
  const commandText = (command, commandArgs, cwd, envPrefix) => {
    const rendered = [command, ...commandArgs].map(shellQuote).join(' ');
    const prefixed = envPrefix ? `${envPrefix} ${rendered}` : rendered;
    return cwd ? `(cd ${shellQuote(cwd)} && ${prefixed})` : prefixed;
  };
  const abort = (step, message) => {
    process.stderr.write(`\nFAIL: deploy aborted during ${step}\n`);
    fail(message, 2);
  };
  const spawn = (step, command, commandArgs, options = {}) => {
    process.stdout.write(`  $ ${commandText(command, commandArgs, options.cwd, options.envPrefix)}\n`);
    const result = spawnSync(command, commandArgs, {
      cwd: options.cwd,
      env: options.env || process.env,
      stdio: options.stdio || 'inherit',
      timeout: options.timeout
    });
    if (result.error) {
      const detail = result.error.code === 'ETIMEDOUT'
        ? `command timed out after ${options.timeout}ms`
        : result.error.message;
      abort(step, detail);
    }
    if (result.status !== 0) {
      abort(step, `${command} exited with status ${result.status ?? 'unknown'}`);
    }
    return result;
  };

  const findGitRoot = (startPath) => {
    let current;
    try {
      current = fs.realpathSync(startPath);
    } catch (err) {
      fail(`cannot resolve deploy repo '${startPath}': ${err.message}`, 1);
    }
    let stat;
    try { stat = fs.statSync(current); }
    catch (err) { fail(`cannot inspect deploy repo '${current}': ${err.message}`, 1); }
    if (!stat.isDirectory()) fail(`deploy repo is not a directory: ${current}`, 1);

    while (true) {
      if (fs.existsSync(path.join(current, '.git'))) return current;
      const parent = path.dirname(current);
      if (parent === current) break;
      current = parent;
    }
    fail(`no git root found above ${startPath}`, 1);
  };

  // Follow the real CLI path (important when `pretty` is a symlink) and walk
  // upward. An explicit --repo may point at the root or any directory in it.
  const defaultStart = path.dirname(fs.realpathSync(__filename));
  const repo = findGitRoot(repoOverride ? path.resolve(repoOverride) : defaultStart);
  const prettydDir = path.join(repo, 'prettyd');
  const frontendDir = path.join(repo, 'frontend');
  const serverJs = path.join(prettydDir, 'dist', 'server.js');

  for (const required of [
    path.join(prettydDir, 'package.json'),
    path.join(frontendDir, 'package.json')
  ]) {
    if (!fs.existsSync(required)) fail(`deploy repo is incomplete: missing ${required}`, 1);
  }

  const conflictCheck = spawnSync('git', ['diff', '--name-only', '--diff-filter=U'], {
    cwd: repo,
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe']
  });
  if (conflictCheck.error) abort('conflict check', conflictCheck.error.message);
  if (conflictCheck.status !== 0) {
    const detail = (conflictCheck.stderr || '').trim();
    abort('conflict check', detail || `git exited with status ${conflictCheck.status}`);
  }
  const conflicts = conflictCheck.stdout.trim();
  if (conflicts) abort('conflict check', `working tree has unresolved conflicts:\n${conflicts}`);

  const uid = process.getuid?.();
  if (uid === undefined) fail('deploy requires a platform with process.getuid()', 2);
  const serviceTarget = `gui/${uid}/tech.pretty-pty.daemon`;
  const importExpression = `await import(${JSON.stringify(url.pathToFileURL(serverJs).href)})`;
  const smokeArgs = ['--input-type=module', '-e', importExpression];
  const smokeEnv = { ...process.env, PRETTYD_SMOKE: '1' };

  const runPreflight = () => {
    const step = 'dist/server.js import preflight';
    let builtServer;
    try {
      builtServer = fs.readFileSync(serverJs, 'utf8');
    } catch (err) {
      abort(step, `cannot read ${serverJs}: ${err.message}`);
    }
    // A stale dist from before the smoke guard would start a real server when
    // imported. Refuse it so dry-run remains incapable of touching the daemon.
    if (!builtServer.includes('PRETTYD_SMOKE')) {
      abort(step, `${serverJs} is stale and lacks the PRETTYD_SMOKE guard; run a live deploy to rebuild it safely`);
    }
    const result = spawn(step, process.execPath, smokeArgs, {
      cwd: prettydDir,
      env: smokeEnv,
      envPrefix: 'PRETTYD_SMOKE=1',
      timeout: 5_000
    });
    process.stdout.write('  PASS: dist/server.js imports resolved within 5s\n');
    return result;
  };

  const runnerCount = (step) => {
    process.stdout.write('  $ pgrep -f dist/runner.js | wc -l\n');
    const result = spawnSync('pgrep', ['-f', 'dist/runner.js'], {
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe']
    });
    if (result.error) abort(step, result.error.message);
    if (result.status !== 0 && result.status !== 1) {
      const detail = (result.stderr || '').trim();
      abort(step, detail || `pgrep exited with status ${result.status}`);
    }
    const output = result.stdout.trim();
    return output ? output.split(/\r?\n/).filter(Boolean).length : 0;
  };

  const pollHealth = async () => {
    const deadline = Date.now() + 30_000;
    let lastError = 'no response';
    while (Date.now() < deadline) {
      try {
        const response = await api('GET', '/api/health', undefined, 1_500);
        if (response.status !== 200) {
          lastError = `HTTP ${response.status}`;
        } else {
          const health = JSON.parse(response.body.toString('utf8'));
          if (health && health.ok === true && health.name === 'prettyd') {
            const listen = health.listen && typeof health.listen === 'object'
              ? health.listen
              : { host: HOST, port: Number(PORT) };
            return { health, listen };
          }
          lastError = 'response did not report prettyd ok=true';
        }
      } catch (err) {
        lastError = err.message || String(err);
      }
      await new Promise((resolve) => setTimeout(resolve, 500));
    }
    abort('health check', `daemon did not become healthy within 30s (${lastError})`);
  };

  process.stdout.write(`pretty deploy\nrepo: ${repo}\nmode: ${dryRun ? 'dry-run' : 'live'}\n\n`);

  if (dryRun) {
    const plan = [
      `${noPull ? 'SKIP (--no-pull)' : 'SKIP (--dry-run)'}  ${commandText('git', ['pull', '--ff-only'], repo)}`,
      `SKIP (--dry-run)  ${commandText('npm', ['install'], prettydDir)}`,
      `SKIP (--dry-run)  ${commandText('npm', ['install'], frontendDir)}`,
      `SKIP (--dry-run)  ${commandText('npm', ['run', 'build'], prettydDir)}`,
      `SKIP (--dry-run)  ${commandText('npm', ['run', 'build'], frontendDir)}`,
      `RUN                ${commandText(process.execPath, smokeArgs, prettydDir, 'PRETTYD_SMOKE=1')}`,
      'SKIP (--dry-run)  pgrep -f dist/runner.js | wc -l  # runner baseline',
      `SKIP (--dry-run)  launchctl kickstart -k ${serviceTarget}`,
      `SKIP (--dry-run)  poll ${HOST}:${PORT}/api/health for up to 30s`,
      'SKIP (--dry-run)  verify runner count >= baseline - 1'
    ];
    process.stdout.write('Plan:\n');
    plan.forEach((line, index) => process.stdout.write(`  ${index + 1}. ${line}\n`));
    process.stdout.write('\nExecuting the import preflight (the only dry-run action):\n');
    runPreflight();
    process.stdout.write('\nPASS: dry-run preflight succeeded; no deploy actions were executed\n');
    return;
  }

  if (noPull) {
    process.stdout.write('[1/10] SKIP git pull --ff-only (--no-pull)\n');
  } else {
    process.stdout.write('[1/10] Pull latest changes (fast-forward only)\n');
    spawn('git pull', 'git', ['pull', '--ff-only'], { cwd: repo });
  }
  process.stdout.write('[2/10] Install prettyd dependencies (always)\n');
  spawn('prettyd dependency install', 'npm', ['install'], { cwd: prettydDir });
  process.stdout.write('[3/10] Install frontend dependencies (always)\n');
  spawn('frontend dependency install', 'npm', ['install'], { cwd: frontendDir });
  process.stdout.write('[4/10] Build prettyd\n');
  spawn('prettyd build', 'npm', ['run', 'build'], { cwd: prettydDir });
  process.stdout.write('[5/10] Build frontend (TypeScript + Vite)\n');
  spawn('frontend build', 'npm', ['run', 'build'], { cwd: frontendDir });
  process.stdout.write('[6/10] Preflight dist/server.js imports\n');
  runPreflight();
  process.stdout.write('[7/10] Record runner baseline\n');
  const runnerBaseline = runnerCount('runner baseline');
  process.stdout.write(`  runner baseline: ${runnerBaseline}\n`);
  process.stdout.write('[8/10] Restart prettyd LaunchAgent\n');
  spawn('launchd restart', 'launchctl', ['kickstart', '-k', serviceTarget]);
  process.stdout.write('[9/10] Poll /api/health (up to 30s)\n');
  const { listen } = await pollHealth();
  const listenHost = typeof listen.host === 'string' ? listen.host : HOST;
  const listenPort = Number.isFinite(Number(listen.port)) ? Number(listen.port) : Number(PORT);
  process.stdout.write(`  healthy: ${listenHost}:${listenPort}\n`);
  process.stdout.write('[10/10] Verify runner survival\n');
  const runnerAfter = runnerCount('runner survival check');
  const minimum = runnerBaseline - 1;
  if (runnerAfter < minimum) {
    abort('runner survival check', `runner count ${runnerAfter} is below required minimum ${minimum} (baseline ${runnerBaseline})`);
  }
  process.stdout.write(`  runners: ${runnerAfter} (baseline ${runnerBaseline}, required >= ${minimum})\n`);
  process.stdout.write('\nPASS: deploy completed; dependencies installed, builds preflighted, daemon healthy\n');
}

// `pretty install` — register prettyd as a macOS LaunchAgent daemon and
// start it. Safe to re-run: an already-loaded agent is kickstarted.
// All paths are resolved at runtime from __dirname so the installed CLI
// works wherever the repo was cloned — no hardcoded paths.
async function cmdInstall() {
  const { spawnSync } = require('node:child_process');

  // __dirname = prettyd/bin/ → prettyd/ is one level up.
  const prettydDir = path.resolve(__dirname, '..');
  const serverJs  = path.join(prettydDir, 'dist', 'server.js');
  const webIndex  = path.join(prettydDir, 'web', 'index.html');
  const logDir    = path.join(os.homedir(), 'Library', 'Logs', 'pretty-pty');
  const logFile   = path.join(logDir, 'daemon.log');
  const agentsDir = path.join(os.homedir(), 'Library', 'LaunchAgents');
  const plistPath = path.join(agentsDir, 'tech.pretty-pty.daemon.plist');

  // PATH must include both Homebrew roots so bare 'claude'/'codex'/'node'
  // resolve on Apple Silicon (/opt/homebrew/bin) and Intel (/usr/local/bin).
  const daemonPath = '/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin';

  function executableIn(searchPath, name) {
    for (const dir of searchPath.split(':')) {
      const candidate = path.join(dir, name);
      try {
        fs.accessSync(candidate, fs.constants.X_OK);
        return candidate;
      } catch { /* keep looking */ }
    }
    return null;
  }

  // Validate the package and host before writing a plist or invoking launchd.
  if (!fs.existsSync(serverJs)) fail(`install is incomplete: missing daemon at ${serverJs}; reinstall pretty-pty`, 2);
  if (!fs.existsSync(webIndex)) fail(`install is incomplete: missing bundled frontend at ${webIndex}; reinstall pretty-pty`, 2);
  const launchctl = executableIn('/bin:/usr/bin', 'launchctl');
  if (!launchctl) fail('install requires launchctl, but it was not found in /bin or /usr/bin', 2);
  const nodeBinary = executableIn(daemonPath, 'node');
  if (!nodeBinary) fail(`install requires a node binary on the daemon PATH (${daemonPath})`, 2);

  // Propagate any user-pinned bind host/port into the daemon environment.
  const envVars = { PATH: daemonPath };
  if (process.env.PRETTYD_HOST) envVars.PRETTYD_HOST = process.env.PRETTYD_HOST;
  if (process.env.PRETTYD_PORT) envVars.PRETTYD_PORT = process.env.PRETTYD_PORT;

  function escapeXml(s) {
    return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  }
  const envEntries = Object.entries(envVars)
    .map(([k, v]) => `    <key>${escapeXml(k)}</key>\n    <string>${escapeXml(v)}</string>`)
    .join('\n');

  // Use /usr/bin/env node so the current Homebrew node resolves via PATH,
  // not a versioned Cellar path that breaks on `brew upgrade node` (contract #6).
  const plistXml = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>tech.pretty-pty.daemon</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/bin/env</string>
    <string>node</string>
    <string>${escapeXml(serverJs)}</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
${envEntries}
  </dict>
  <key>WorkingDirectory</key>
  <string>${escapeXml(prettydDir)}</string>
  <key>RunAtLoad</key>
  <true/>
  <!-- KeepAlive=true: the daemon itself should restart on crash. This is
       distinct from the per-session runner plists, which use KeepAlive only
       on non-zero exit (SuccessfulExit=false). -->
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>${escapeXml(logFile)}</string>
  <key>StandardErrorPath</key>
  <string>${escapeXml(logFile)}</string>
</dict>
</plist>
`;

  fs.mkdirSync(agentsDir, { recursive: true });
  fs.mkdirSync(logDir,    { recursive: true });
  // 0600: plist may carry env vars with sensitive values (API keys, proxy)
  // in the future — be restrictive now so we don't have to chmod later.
  fs.writeFileSync(plistPath, plistXml, { mode: 0o600 });
  try { fs.chmodSync(plistPath, 0o600); } catch { /* best effort */ }
  process.stdout.write(`wrote plist: ${plistPath}\n`);

  // Bootstrap the daemon. On upgrade, restart an already-loaded agent so it
  // picks up the newly installed package without requiring a manual unload.
  const uid = process.getuid?.() ?? 0;
  const serviceTarget = `gui/${uid}/tech.pretty-pty.daemon`;
  const r = spawnSync(launchctl, ['bootstrap', `gui/${uid}`, plistPath], {
    stdio: ['ignore', 'pipe', 'pipe']
  });
  if (r.status !== 0) {
    const err = (r.stderr || Buffer.alloc(0)).toString().trim();
    const alreadyLoaded = r.status === 17 || /already (loaded|bootstrapped)/i.test(err);
    if (alreadyLoaded) {
      const kick = spawnSync(launchctl, ['kickstart', '-k', serviceTarget], {
        stdio: ['ignore', 'pipe', 'pipe']
      });
      if (kick.status !== 0) {
        const kickErr = (kick.stderr || Buffer.alloc(0)).toString().trim();
        fail(`launchctl kickstart failed (status=${kick.status}): ${kickErr || 'unknown error'}`, 2);
      }
    } else {
      fail(`launchctl bootstrap failed (status=${r.status}): ${err || r.error?.message || 'unknown error'}`, 2);
    }
  }

  const daemonHost = process.env.PRETTYD_HOST || '127.0.0.1';
  const daemonPort = process.env.PRETTYD_PORT || '8787';
  const healthDeadline = Date.now() + 15_000;
  let lastHealthError = 'no response';
  while (Date.now() < healthDeadline) {
    try {
      const health = await api('GET', '/api/health', undefined, 1_000);
      if (health.status === 200) {
        lastHealthError = '';
        break;
      }
      lastHealthError = `HTTP ${health.status}`;
    } catch (err) {
      lastHealthError = err.message || String(err);
    }
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  if (lastHealthError) {
    fail(`daemon did not become healthy at http://${daemonHost}:${daemonPort}/api/health within 15s (${lastHealthError}); see ${logFile}`, 2);
  }
  const tok = readToken();

  process.stdout.write('\nprettyd daemon registered, started, and healthy.\n');
  process.stdout.write(`  URL:   http://${daemonHost}:${daemonPort}\n`);
  if (tok) {
    process.stdout.write(`  Token: ${tok}\n`);
    process.stdout.write('\nPaste the URL and token into the pretty-PTY web UI (server settings).\n');
  } else {
    process.stdout.write('\nToken not yet generated — give the daemon a moment, then run: pretty token\n');
  }
  process.stdout.write(`  Logs:  ${logFile}\n`);
}

function requestHealthAt(target) {
  return new Promise((resolve, reject) => {
    const transport = target.protocol === 'https:' ? https : http;
    const req = transport.request({
      protocol: target.protocol,
      hostname: target.host,
      port: target.port,
      path: '/api/health',
      method: 'GET',
      headers: { accept: 'application/json' },
      timeout: 2_000
    }, (res) => {
      const chunks = [];
      res.on('data', (chunk) => chunks.push(chunk));
      res.once('end', () => resolve({ status: res.statusCode, body: Buffer.concat(chunks) }));
    });
    req.once('timeout', () => req.destroy(Object.assign(new Error('request timed out'), { code: 'ETIMEDOUT' })));
    req.once('error', reject);
    req.end();
  });
}

async function cmdRemote(action, extraArgs) {
  if (extraArgs.length > 0) fail('usage: pretty remote <enable|disable|status>');
  const { runRemote } = require('./remote.cjs');
  await runRemote(action, {
    fail,
    wantJson,
    getDaemonListen: async (tailscaleStatus) => {
      const resolved = apiTarget('/api/health');
      const candidates = [{
        protocol: resolved.protocol,
        host: resolved.hostname,
        port: resolved.port
      }];
      for (const host of tailscaleStatus.Self.TailscaleIPs || []) {
        if (!candidates.some((candidate) => candidate.host === host && candidate.port === Number(PORT))) {
          candidates.push({ protocol: 'http:', host, port: Number(PORT) });
        }
      }

      for (const candidate of candidates) {
        let response;
        try {
          response = await requestHealthAt(candidate);
        } catch {
          continue;
        }
        if (response.status !== 200) continue;
        let health;
        try { health = JSON.parse(response.body.toString('utf8')); }
        catch { continue; }
        if (!health || health.ok !== true || health.name !== 'prettyd') continue;
        if (health.listen && typeof health.listen.host === 'string') return health.listen;
        return { host: candidate.host, port: candidate.port };
      }

      const attempted = candidates.map((candidate) => `${candidate.host}:${candidate.port}`).join(', ');
      fail(`cannot reach prettyd at ${attempted}. Start it first with \`pretty install\`.`, 2);
    }
  });
}

function help() {
  process.stdout.write([
    'pretty — prettyd CLI',
    'Session ids may be full ids or unique prefixes from `pretty ls`.',
    '',
    'Subcommands:',
    '  ls [-a | --include-exited]  list sessions (default: hides exited)',
    '  snap <id> [--raw]        print current buffer (default: clean text)',
    '  tail <id> [-f] [-n N]    print last N (default 50) lines; -f to follow',
    '  wait <id> [--idle Ns] [--timeout Ns]',
    '                           block until session has been idle for Ns.',
    '                           Default --idle 2s, default --timeout 30s;',
    '                           --timeout is tunable/uncapped (e.g. 30m).',
    '                           Background use: pretty wait <id> --timeout 1800s &',
    '                           so an orchestrating agent can be re-invoked on completion.',
    '  send <id> [--timeout Ns] [--no-wait] [--file path] <text...>',
    '                           send text + Enter (alias: `input`).',
    '                           For Claude/Codex sessions: blocks until the',
    '                           event log confirms receipt (default --timeout 10s).',
    '                           Re-presses Enter only when text is still visible',
    '                           in the composer (anti-duplicate guard).',
    '                           --file reads the message body from UTF-8 file.',
    '                           --no-wait: fire-and-forget (old behavior).',
    '                           Exits non-zero if confirmation times out.',
    '  input <id> <text...>     same as send',
    '  last <id> [--role user|assistant] [-n N]',
    '                           print the last message(s) from the JSONL log.',
    '                           Default: last user + last assistant message.',
    '  transcript <id>          print all user/assistant turns from the event log',
    '                           (clean text; --json emits structured turns).',
    '  ask <id> [--timeout Ns] [--idle Ns] [--wait-timeout Ns] <text...>',
    '                           send (with event confirmation), wait for the tool',
    '                           to finish its reply (working→idle), then print',
    '                           the last assistant message. Claude/Codex only.',
    '  keys <id> <key>          send esc|up|down|left|right|^c|^d|enter|tab',
    '  new --tool <claude|codex|shell> [--cwd P] [--name L]',
    '                           [--model M] [--effort L] [--fast]',
    '                           [--on-idle C] [--wait-ready] [--no-skip-perms] [extra args]',
    '  new [--cwd P] [--name L] [--model M] [--effort L] [--fast]',
    '                           [--on-idle C] [--wait-ready] [--cmd C] [args...]',
    '                           create a session.  --tool is the easy path:',
    '                              pretty new --tool claude',
    '                              pretty new --tool claude --cwd ~/foo',
    '                              pretty new --tool codex --no-skip-perms',
    '                           --name labels the session in `pretty ls`.',
    '                           --on-idle runs a shell command on working→idle.',
    '                           --wait-ready waits for tool startup before returning.',
    '                           or supply --cmd / a positional command directly.',
    '  model <id> <model> [--effort L]',
    '                           switch model/effort on an idle Claude session.',
    '  kill <id> [<id>...]      terminate one or more sessions',
    '  attach <id>              raw two-way stream (Ctrl+Q to detach)',
    '  doctor                   per-session health: QoS (throttled?), spawn',
    '                           path (dist/tsx), flags sessions needing recreate',
    '  token                    print the daemon auth token (paste into web UI)',
    '  install                  register prettyd as a macOS LaunchAgent and start it',
    '  deploy [--repo P] [--no-pull] [--dry-run]',
    '                           canonical safe update: pull, always install both',
    '                           dependency trees, build, smoke-import dist/server.js,',
    '                           then restart, health-check, and verify runners.',
    '                           --no-pull skips only git pull; --dry-run executes',
    '                           only the smoke import and never touches launchd.',
    '  remote enable            expose the daemon over tailnet-only Tailscale HTTPS',
    '  remote disable           remove Pretty\'s Tailscale Serve HTTPS root handler',
    '  remote status            verify the Serve endpoint and /api/health',
    '',
    'Global flags:',
    '  --json   machine-friendly output',
    '  --host   prettyd host (default 127.0.0.1)',
    '  --port   prettyd port (default 8787)',
    ''
  ].join('\n'));
}

function cmdVersion() {
  const pkg = require(path.resolve(__dirname, '..', 'package.json'));
  process.stdout.write(`${pkg.version}\n`);
}

async function dispatch() {
  switch (sub) {
    case 'ls': return cmdLs(argv);
    case 'snap': return cmdSnap(argv[0], argv.includes('--raw'));
    case 'send':
    case 'input':  // alias — same operation, more intuitive for agent loops
      return cmdSend(argv.slice()); // argv still has id + text + flags
    case 'last': return cmdLast(argv.slice());
    case 'transcript': return cmdTranscript(argv.slice());
    case 'ask':  return cmdAsk(argv.slice());
    case 'keys': return cmdKeys(argv[0], argv[1]);
    case 'new':  return cmdNew(argv);
    case 'model': return cmdModel(argv.slice());
    case 'kill': return cmdKill(argv);
    case 'tail': return cmdTail(argv);
    case 'wait': return cmdWait(argv);
    case 'attach': return cmdAttach(argv[0]);
    case 'doctor':  return cmdDoctor();
    case 'token':   return cmdToken();
    case 'install': return cmdInstall();
    case 'deploy':  return cmdDeploy(argv.slice());
    case 'version':
    case '--version':
    case '-v':
      return cmdVersion();
    case 'remote':  return cmdRemote(argv.shift(), argv);
    case undefined:
    case 'help':
    case '--help':
    case '-h':
      return help();
    default:
      fail(`unknown command: ${sub}\n\nrun 'pretty help' for usage`);
  }
}

(async () => {
  try {
    await dispatch();
  } finally {
    destroyApiAgents();
  }
})().catch((err) => fail(err.message || String(err), 2));
