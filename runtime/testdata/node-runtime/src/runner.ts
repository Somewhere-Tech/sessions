// Long-lived per-session runner process. Owns one PTY, a 4MB event log,
// an xterm-headless mirror of the buffer, and a Unix socket. Detached
// from the prettyd parent so it survives when prettyd exits — sessions
// outlive `tsx watch` reloads, hand-restarts, even prettyd crashes.
//
// Spawned by prettyd via:
//   spawn(node, [runner.js], {
//     env: { RUNNER_ID, RUNNER_CWD, RUNNER_COLS, RUNNER_ROWS,
//            RUNNER_CMD, RUNNER_ARGS_JSON },
//     detached: true, stdio: ['ignore','ignore','ignore'] });
//
// On startup the runner:
//   1. Spawns the PTY
//   2. Writes <state-dir>/<id>.json (metadata) + opens <state-dir>/<id>.sock
//   3. Mirrors PTY output to xterm-headless + the EventLog
//   4. Accepts client connections (typically just prettyd) over the socket
//   5. On PTY exit, holds the socket open so reconnecting clients see the
//      final state, then exits 30s after the last client disconnects.
//
// On clean shutdown (KILL frame, or PTY exit + idle timeout), removes
// both files and exits.

import { spawn, type IPty } from 'node-pty';
// @xterm/headless ships a CJS `main` and an .mjs `module` whose ESM
// shape doesn't expose `Terminal` as a named export under tsx/node20.
// createRequire bypasses the ESM facade and pulls the CJS module that
// works everywhere.
import { createRequire } from 'node:module';
const xtermRequire = createRequire(import.meta.url);
const { Terminal } = xtermRequire('@xterm/headless') as typeof import('@xterm/headless');
const { SerializeAddon } = xtermRequire('@xterm/addon-serialize') as typeof import('@xterm/addon-serialize');
import { connect, createServer, type Server, type Socket } from 'node:net';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { EventLog } from './eventLog.js';
import { PersistentLog } from './persistentLog.js';
import {
  FrameParser, FrameType, encodeFrame, encodeOutput,
  RUNNER_PROTOCOL_VERSION,
  type RunnerHello, type RunnerExit
} from './runnerProtocol.js';

const RUNNER_ID = process.env.RUNNER_ID;
if (!RUNNER_ID) {
  console.error('runner: RUNNER_ID env var required');
  process.exit(2);
}
const STATE_DIR = process.env.RUNNER_STATE_DIR
  ?? path.join(os.homedir(), '.local', 'state', 'pretty-PTY', 'runners');
const SOCK_PATH = path.join(STATE_DIR, RUNNER_ID + '.sock');
const META_PATH = path.join(STATE_DIR, RUNNER_ID + '.json');
const EVENTS_PATH = path.join(STATE_DIR, RUNNER_ID + '.events');

const cmd = process.env.RUNNER_CMD ?? '/bin/bash';
// Wrap JSON.parse so a corrupted env var doesn't crash-loop the runner.
// Exit 0 so launchd KeepAlive(SuccessfulExit=false) does NOT respawn.
const rawArgsJson = process.env.RUNNER_ARGS_JSON;
let args: string[];
try {
  args = rawArgsJson ? (JSON.parse(rawArgsJson) as string[]) : [];
} catch (err) {
  process.stderr.write(`runner: failed to parse RUNNER_ARGS_JSON="${rawArgsJson}": ${err}\n`);
  process.exit(0);
}
const cwd = process.env.RUNNER_CWD ?? os.homedir();
const cols = Number(process.env.RUNNER_COLS ?? 300);
const rows = Number(process.env.RUNNER_ROWS ?? 50);

fs.mkdirSync(STATE_DIR, { recursive: true, mode: 0o700 });

// Duplicate-runner guard — the root fix for orphan/duplicate accumulation.
// If a runner for THIS id is already alive and serving the socket, a second
// instance (launchd KeepAlive race, a stray `launchctl kickstart`, a
// double-bootstrap) must NOT proceed: the unlink below would steal the live
// socket, and we'd spawn a DUPLICATE `claude` — two runners and two claude
// processes for one session, each eating CPU and fighting over the PTY. Probe
// the existing socket; if anything answers, exit 0 so launchd's
// KeepAlive(SuccessfulExit=false) leaves us dead instead of respawn-looping.
// A stale socket file from a real crash refuses the connection (ECONNREFUSED)
// → we fall through and rebind, so legitimate respawns are unaffected.
if (fs.existsSync(SOCK_PATH)) {
  const ownerAlive = await new Promise<boolean>((resolve) => {
    const probe = connect(SOCK_PATH);
    const finish = (v: boolean) => { try { probe.destroy(); } catch { /* ignore */ } resolve(v); };
    probe.once('connect', () => finish(true));
    probe.once('error', () => finish(false));
    setTimeout(() => finish(false), 1000);
  });
  if (ownerAlive) {
    console.error(`runner ${RUNNER_ID}: another instance already owns ${SOCK_PATH} — exiting to avoid a duplicate`);
    process.exit(0);
  }
}
// Stale socket from a previous crash → unlink before bind.
try { fs.unlinkSync(SOCK_PATH); } catch { /* not present */ }

const env: Record<string, string> = {
  ...(process.env as Record<string, string>),
  TERM: 'xterm-256color',
  COLORTERM: 'truecolor'
};
// Don't leak our control vars into the child's env.
delete env.RUNNER_ID;
delete env.RUNNER_CMD;
delete env.RUNNER_ARGS_JSON;
delete env.RUNNER_CWD;
delete env.RUNNER_COLS;
delete env.RUNNER_ROWS;
delete env.RUNNER_STATE_DIR;

// On a RESPAWN (Mac reboot, crash-restart via launchd KeepAlive), the
// session's Claude JSONL already exists on disk. Re-running the original
// `claude --session-id <uuid>` then dies immediately with "Session ID is
// already in use" and launchd crash-loops it — so a reboot would lose every
// Claude session instead of restoring it. Swap `--session-id` → `--resume`
// on respawn so Claude reattaches to the same conversation. The persistent
// events file already holding content is the reliable "this session has run
// before" signal (it's empty/absent on a genuinely fresh start). codex/shell
// sessions carry no --session-id, so they're untouched.
let spawnArgs = args;
// Set when the backing Claude JSONL is absent on a respawn attempt.
// Prevents pty.onExit from flipping sessionEnded=true, which would
// delete the events history we want to preserve.
let jsonlMissing = false;
try {
  const isRespawn = fs.existsSync(EVENTS_PATH) && fs.statSync(EVENTS_PATH).size > 0;
  const sidIdx = args.indexOf('--session-id');
  if (isRespawn && sidIdx >= 0) {
    spawnArgs = args.slice();
    spawnArgs[sidIdx] = '--resume';
    // Guard: verify the Claude JSONL for this session exists before
    // committing to --resume. If it's missing, Claude exits immediately
    // with an error and pty.onExit would normally delete our events file.
    // We still attempt --resume (best chance of success or graceful error),
    // but mark jsonlMissing so the history is not discarded on exit.
    const sessionUuid = args[sidIdx + 1];
    if (sessionUuid) {
      const claudeProjects = path.join(os.homedir(), '.claude', 'projects');
      let jsonlFound = false;
      if (fs.existsSync(claudeProjects)) {
        try {
          // Claude stores JSONL at ~/.claude/projects/<hash>/<uuid>.jsonl;
          // search one level deep since we don't know the project hash.
          for (const dir of fs.readdirSync(claudeProjects)) {
            if (fs.existsSync(path.join(claudeProjects, dir, sessionUuid + '.jsonl'))) {
              jsonlFound = true;
              break;
            }
          }
        } catch { /* scan errors are non-fatal */ }
      }
      if (!jsonlFound) jsonlMissing = true;
    }
  }
} catch { /* best effort — fall back to the original args */ }

const pty: IPty = spawn(cmd, spawnArgs, { name: 'xterm-256color', cols, rows, cwd, env });
const log = new EventLog();
const term = new Terminal({ cols, rows, scrollback: 5000, allowProposedApi: true });
const serialize = new SerializeAddon();
term.loadAddon(serialize);

// Persistent on-disk mirror so a Mac reboot (which kills the runner
// process) doesn't lose the buffer history. On startup we replay any
// existing records into both the EventLog and xterm-headless — the
// PTY itself is fresh (Claude/Codex/whatever just started), but the
// user sees the same scrollback they had before the reboot. They can
// `/resume` inside Claude to continue the actual conversation.
const persistent = PersistentLog.open(EVENTS_PATH);
const restored = PersistentLog.restoreFrom(EVENTS_PATH);
for (const ev of restored) {
  log.pushAt(ev.seq, ev.data);
  term.write(ev.data);
}
if (restored.length > 0) {
  // Make it visually obvious the buffer was replayed.
  const banner = `\r\n\x1b[2m[pretty-pty: replayed ${restored.length} events from disk · ${new Date().toISOString()}]\x1b[0m\r\n`;
  log.push(banner);
  term.write(banner);
  // Don't persist the banner — it's purely a UX artifact for this
  // restore. If we did, every restore would add another banner on top.
}
if (jsonlMissing) {
  // Warn the user in the scrollback that the backing JSONL was absent.
  // The --resume may fail or create a new session; events are preserved.
  // Not persisted — diagnostic only.
  const notice = `\r\n\x1b[33m[pretty-pty: backing Claude JSONL not found — attempted --resume may fail; events history is preserved]\x1b[0m\r\n`;
  log.push(notice);
  term.write(notice);
}

interface SessionMeta {
  id: string;
  cmd: string;
  args: string[];
  cwd: string;
  cols: number;
  rows: number;
  createdAt: number;
  pid: number;
  sockPath: string;
}
const meta: SessionMeta = {
  id: RUNNER_ID,
  cmd,
  args,
  cwd,
  cols,
  rows,
  createdAt: Date.now(),
  pid: pty.pid,
  sockPath: SOCK_PATH
};
fs.writeFileSync(META_PATH, JSON.stringify(meta, null, 2), { mode: 0o600 });

let exited = false;
let exitInfo: RunnerExit | null = null;
let recentBytes = 0;
// True iff the session has ended for good (PTY died via KILL frame, or
// the program inside the PTY exited on its own). Drives whether
// cleanupAndExit() drops the persistent event log. Stays false for
// SIGTERM-from-launchd shutdowns so the next run can replay history.
let sessionEnded = false;

const clients = new Set<Socket>();

function broadcastFrame(buf: Buffer): void {
  for (const c of clients) {
    if (!c.destroyed) {
      try { c.write(buf); } catch { /* client gone */ }
    }
  }
}

pty.onData((data) => {
  const ev = log.push(data);
  recentBytes += Buffer.byteLength(data, 'utf8');
  term.write(data);
  // Persist BEFORE broadcasting so a client that sees seq N is guaranteed
  // to find seq N on disk if they reconnect after a runner crash.
  try { persistent.append(ev.seq, ev.data); }
  catch (err) { console.error('persistent.append failed:', (err as Error).message); }
  broadcastFrame(encodeOutput(ev.seq, ev.data));
});

pty.onExit(({ exitCode, signal }) => {
  exited = true;
  // Only mark the session as permanently ended if the JSONL was present on
  // this respawn attempt. If it was missing, Claude may have exited right
  // away with an error; keeping sessionEnded=false preserves the events file
  // so the next run (or the user) can inspect what happened.
  if (!jsonlMissing) sessionEnded = true;
  exitInfo = {
    code: exitCode,
    signal: typeof signal === 'number' ? String(signal) : (signal ?? null),
    seq: log.currentSeq()
  };
  broadcastFrame(encodeFrame(FrameType.EXIT, JSON.stringify(exitInfo)));
  // Stay alive briefly so reconnecting clients can see the exit. The
  // idle-disconnect timer below will trigger eventual shutdown.
  scheduleIdleShutdown();
});

// ────────────────────────────────────────────────────────────────────────
// Activity decay (mirrors prettyd's old in-process behavior so the metadata
// file's "working" flag is accurate when an outside reader peeks at it).

setInterval(() => {
  recentBytes = Math.floor(recentBytes / 2);
}, 800).unref();

// ────────────────────────────────────────────────────────────────────────
// Unix socket: one accept loop, multiple clients OK (though typically
// prettyd is the only caller). Each client gets HELLO + replay stream
// from REPLAY_REQ if asked, otherwise just live frames going forward.

const helloPayload = (): Buffer => {
  const h: RunnerHello = {
    id: RUNNER_ID,
    cmd, args, cwd, cols: pty.cols, rows: pty.rows,
    createdAt: meta.createdAt,
    pid: pty.pid,
    currentSeq: log.currentSeq(),
    // Contract #3: daemon reads hello.protocolVersion ?? 0 (legacy) and
    // logs a mismatch warning but always attaches to stay backward-compat.
    protocolVersion: RUNNER_PROTOCOL_VERSION
  };
  return encodeFrame(FrameType.HELLO, JSON.stringify(h));
};

const server: Server = createServer((sock) => {
  clients.add(sock);
  const parser = new FrameParser();
  sock.on('data', (chunk) => {
    try {
      parser.push(chunk, (type, payload) => onClientFrame(sock, type, payload));
    } catch (err) {
      sock.destroy();
    }
  });
  sock.on('close', () => {
    clients.delete(sock);
    if (exited && clients.size === 0) scheduleIdleShutdown();
  });
  sock.on('error', () => { /* ignore — close handler runs */ });

  // Greet the client. They can ask for replay via REPLAY_REQ if they want it.
  sock.write(helloPayload());
  if (exited && exitInfo) {
    sock.write(encodeFrame(FrameType.EXIT, JSON.stringify(exitInfo)));
  }
});

// A bind failure (e.g. stale socket we couldn't unlink, permissions) must
// not leave the runner in a silent crashloop — log and exit non-zero so
// launchd sees the failure and can back off.
server.on('error', (err) => {
  console.error('runner socket error:', err.message);
  cleanupAndExit(1);
});

server.listen(SOCK_PATH, () => {
  try { fs.chmodSync(SOCK_PATH, 0o600); } catch { /* not fatal */ }
});

function onClientFrame(sock: Socket, type: FrameType, payload: Buffer): void {
  switch (type) {
    case FrameType.INPUT: {
      if (!exited) pty.write(payload.toString('utf8'));
      return;
    }
    case FrameType.RESIZE: {
      try {
        const { cols: c, rows: r } = JSON.parse(payload.toString('utf8'));
        if (typeof c === 'number' && typeof r === 'number') {
          if (!exited) pty.resize(c, r);
          term.resize(c, r);
          meta.cols = c;
          meta.rows = r;
        }
      } catch { /* malformed — ignore */ }
      return;
    }
    case FrameType.SNAPSHOT_REQ: {
      // Include 1000 rows of scrollback (plus the visible viewport).
      // The Pretty parser needs to see Claude's "● " message-start
      // markers to anchor blocks; on a long-running session those have
      // scrolled off-screen, so a viewport-only snapshot leaves the
      // parser with no anchors and it emits 0-2 trivial blocks.
      // 1000 is a balance: enough that recent messages (Claude turns
      // are typically 20-100 rows, tool outputs up to 500) stay
      // anchored, but small enough that the snapshot stays under
      // ~200KB and the Pretty view doesn't grow tens of thousands of
      // pixels tall.
      const snap = serialize.serialize({ scrollback: 1000 });
      sock.write(encodeFrame(FrameType.SNAPSHOT_RES, snap));
      return;
    }
    case FrameType.REPLAY_REQ: {
      if (payload.length < 4) return;
      const after = payload.readUInt32BE(0);
      const replay = log.since(after);
      for (const ev of replay.events) {
        sock.write(encodeOutput(ev.seq, ev.data));
      }
      sock.write(encodeFrame(FrameType.REPLAY_DONE));
      return;
    }
    case FrameType.KILL: {
      try { pty.kill(); } catch { /* already dead */ }
      return;
    }
    default:
      // Unknown frame — drop silently. Forward-compatibility headroom.
      return;
  }
}

// ────────────────────────────────────────────────────────────────────────
// Shutdown.

let idleTimer: NodeJS.Timeout | null = null;
const IDLE_SHUTDOWN_MS = 30_000;

function scheduleIdleShutdown(): void {
  if (!exited) return;
  if (idleTimer !== null) return;
  if (clients.size > 0) return;
  idleTimer = setTimeout(() => {
    cleanupAndExit(0);
  }, IDLE_SHUTDOWN_MS);
  idleTimer.unref();
}

// Two flavors of exit:
//   • sessionEnded === true — the PTY died (user `pretty kill`, Ctrl-D,
//     program exited). Drop the event log. The session is gone forever.
//   • sessionEnded === false — the OS is shutting us down (Mac reboot,
//     `launchctl bootout`, SIGTERM from a system housekeeping job). We
//     expect to be brought back up; the event log MUST survive so the
//     next run can replay it. Keep the .events file, drop everything
//     else (sock, meta, stdio log) — those will be re-created on the
//     next start.
function cleanupAndExit(code: number): void {
  try { server.close(); } catch { /* ignore */ }
  try { fs.unlinkSync(SOCK_PATH); } catch { /* ignore */ }
  try { fs.unlinkSync(META_PATH); } catch { /* ignore */ }
  if (sessionEnded) {
    try { persistent.unlink(); } catch { /* ignore */ }
  } else {
    try { persistent.close(); } catch { /* ignore */ }
  }
  // Belt-and-suspenders: kill the PTY child so it's never orphaned.
  // Normally the master fd close (implicit on process exit) sends SIGHUP,
  // but pty.kill() is more explicit. We call process.exit() immediately
  // after, so pty.onExit() never runs — sessionEnded is NOT mutated here,
  // and the events file decision above is final.
  try { pty.kill(); } catch { /* already dead or PTY not yet spawned */ }
  process.exit(code);
}

// SIGTERM is what `launchctl bootout` sends during system shutdown /
// reboot. cleanupAndExit(0) does call pty.kill() for belt-and-suspenders
// cleanup, but because process.exit() follows immediately, pty.onExit()
// never runs — sessionEnded stays false, and the persistent event log
// is closed (not deleted) so the next runner instance can replay it.
// Note: we do NOT set sessionEnded=true in the SIGTERM path, which is
// why the kill is safe inside cleanupAndExit rather than here.
//
// SIGINT is what tsx-watch sends on hot reload — runners must survive
// that, so we ignore it. SIGHUP fires when the parent dies; we're
// detached so we ignore it too.
//
// `pretty kill` does NOT come through here — it sends a KILL frame on
// the socket, which calls pty.kill() inside the runner. pty.onExit then
// fires (sessionEnded=true) and cleanupAndExit drops the events file.
process.on('SIGINT', () => { /* deliberately ignored */ });
process.on('SIGTERM', () => {
  cleanupAndExit(0);
});
process.on('SIGHUP', () => { /* parent died — keep running, we're detached */ });
