// Claude Code persists every session as a JSONL file in
// ~/.claude/projects/<encoded-cwd>/<claude-session-id>.jsonl. Each
// appended line is a typed event from the Anthropic API stream —
// user message, assistant message, tool call, tool result, system
// notice, etc. — with stable UUIDs and structured content.
//
// This watcher locates the JSONL for a running prettyd session, tails
// new lines, parses them, and emits the events for downstream
// consumers. Way more reliable than scraping the rendered TUI — every
// parsing bug we hit in lib/parser.ts ("Wraysbury misparsed as Bash
// tool", "❯ picker option as user_input", "API error not rendered")
// simply doesn't exist with JSONL — role, content and error fields are
// explicit.
//
// Read-only: we only consume events; input dispatch still goes through
// the PTY (which is the only writable side).
//
// Robustness (the hard-won part): the file we want is not static for a
// session's lifetime. Claude rotates/replaces the JSONL (resume, clear,
// compaction), the file may appear seconds after spawn, or the user may
// have resumed a pre-existing conversation so our pinned id never
// becomes a filename at all. The previous version attached once, gave
// up after 30s if the file wasn't there, and closed forever on the
// first `rename` — which silently froze the Pretty view for any session
// that rotated or whose file appeared late. This version keeps
// re-resolving (poll backstop + dir watch), re-attaches across
// rotations, and de-dupes by event uuid so re-reads never double-emit.

import { EventEmitter } from 'node:events';
import * as fs from 'node:fs';
import * as fsp from 'node:fs/promises';
import { StringDecoder } from 'node:string_decoder';
import { resolveJsonlPath, projectDirFor } from './jsonlResolver.js';

// Anthropic's persisted event format. Conservatively typed — we only
// pluck out the fields downstream actually uses; everything else is
// passed through as opaque.
export interface ClaudeSessionEvent {
  type: string;            // 'user' | 'assistant' | 'system' | 'permission-mode' | ...
  uuid?: string;           // stable message id (used for de-dup)
  parentUuid?: string | null;
  timestamp?: string;      // ISO 8601
  sessionId?: string;      // Claude's session id (NOT prettyd's)
  message?: {
    role?: string;
    content?: unknown;     // string OR array of typed blocks
    model?: string;
    stop_reason?: string;
    usage?: unknown;
  };
  // pass-through for anything else (snapshot events, permission-mode
  // changes, file-history snapshots, queue operations, etc.)
  [key: string]: unknown;
}

interface WatcherOptions {
  cwd: string;
  // Claude session UUID we pinned via `--session-id <uuid>` (fresh) or
  // `--resume <uuid>` (resume) at spawn. Normally the JSONL filename,
  // but resolution falls back gracefully when it isn't (see
  // jsonlResolver.resolveJsonlPath). May be undefined.
  claudeSessionId?: string;
  // Delay before the first resolution attempt — Claude takes ~1s to
  // write its initial JSONL after spawn. Default 800ms.
  initialDelayMs?: number;
  // Backstop poll interval for re-resolution + tail. Default 2000ms.
  // fs.watch gives low-latency updates; this guarantees liveness even
  // when fs.watch misses an event (it does, across rotations).
  pollIntervalMs?: number;
}

export interface SessionFileWatcher {
  readonly emitter: EventEmitter;
  readonly path: () => string | null;
  close(): void;
}

// Cap on the de-dup set so a very long conversation doesn't grow it
// without bound. Trimmed back to TRIM_TO (insertion order preserved by
// Set) when exceeded.
const EMIT_CAP = 60_000;
const EMIT_TRIM_TO = 40_000;

export async function watchSessionFile(opts: WatcherOptions): Promise<SessionFileWatcher> {
  const emitter = new EventEmitter();
  emitter.setMaxListeners(64);

  const projectDir = projectDirFor(opts.cwd);
  const initialDelayMs = opts.initialDelayMs ?? 800;
  const pollIntervalMs = opts.pollIntervalMs ?? 2000;

  let closed = false;
  let currentPath: string | null = null;
  let currentIno: number | null = null;
  let readOffset = 0;
  let lineBuffer = '';
  // Decode bytes through a StringDecoder so a multibyte UTF-8 char split
  // across two reads (the stat lands mid-codepoint) isn't corrupted into
  // U+FFFD — it holds the partial bytes until the next read completes them.
  let decoder = new StringDecoder('utf8');
  let readingInFlight = false;
  let pendingRead = false;

  let dirWatcher: fs.FSWatcher | null = null;
  let fileWatcher: fs.FSWatcher | null = null;
  let pollTimer: NodeJS.Timeout | null = null;
  let tickScheduled = false;
  let unresolvedNotified = false;

  // uuids already emitted — makes re-reads (rotation, truncation,
  // switching back to a file) idempotent so claudeEventLog never
  // double-counts.
  const emitted = new Set<string>();
  const trimEmitted = (): void => {
    if (emitted.size <= EMIT_CAP) return;
    const keep = Array.from(emitted).slice(emitted.size - EMIT_TRIM_TO);
    emitted.clear();
    for (const u of keep) emitted.add(u);
  };

  const detachFileWatcher = (): void => {
    try { fileWatcher?.close(); } catch { /* ignore */ }
    fileWatcher = null;
  };

  // Tail currentPath from readOffset, emitting each new JSONL line.
  // Handles append (same inode, size grew), atomic replace (inode
  // changed → re-read from 0), truncation (size shrank → re-read from
  // 0), and disappearance (stat fails → drop currentPath so the next
  // tick re-resolves). Coalesces overlapping calls.
  const readFrom = async (): Promise<void> => {
    if (closed || !currentPath) return;
    if (readingInFlight) { pendingRead = true; return; }
    readingInFlight = true;
    try {
      let st: fs.Stats;
      try {
        st = await fsp.stat(currentPath);
      } catch {
        // File vanished (rotation). Drop it; tick() re-resolves.
        detachFileWatcher();
        currentPath = null; currentIno = null; readOffset = 0; lineBuffer = '';
        decoder = new StringDecoder('utf8');
        return;
      }
      if ((currentIno !== null && st.ino !== currentIno) || st.size < readOffset) {
        // Replaced or truncated — start over (de-dup guards re-emit).
        readOffset = 0;
        lineBuffer = '';
        decoder = new StringDecoder('utf8');
      }
      currentIno = st.ino;
      if (st.size <= readOffset) return;
      const buf = Buffer.alloc(st.size - readOffset);
      const fh = await fsp.open(currentPath, 'r');
      try {
        await fh.read(buf, 0, buf.length, readOffset);
      } finally {
        await fh.close();
      }
      readOffset = st.size;
      lineBuffer += decoder.write(buf);
      let nl: number;
      while ((nl = lineBuffer.indexOf('\n')) !== -1) {
        const line = lineBuffer.slice(0, nl);
        lineBuffer = lineBuffer.slice(nl + 1);
        if (!line.trim()) continue;
        let parsed: ClaudeSessionEvent;
        try {
          parsed = JSON.parse(line) as ClaudeSessionEvent;
        } catch {
          continue; // ignore malformed lines
        }
        const uuid = typeof parsed.uuid === 'string' ? parsed.uuid : null;
        if (uuid) {
          if (emitted.has(uuid)) continue;
          emitted.add(uuid);
          trimEmitted();
        }
        emitter.emit('event', parsed);
      }
    } finally {
      readingInFlight = false;
      if (pendingRead && !closed) {
        pendingRead = false;
        void readFrom();
      }
    }
  };

  const attachFile = (p: string): void => {
    if (currentPath === p) return;
    detachFileWatcher();
    currentPath = p;
    currentIno = null;
    readOffset = 0;
    lineBuffer = '';
    decoder = new StringDecoder('utf8');
    try {
      fileWatcher = fs.watch(p, { persistent: false }, (eventType) => {
        if (closed) return;
        if (eventType === 'rename') {
          // Renamed/removed/replaced — detach and let tick() re-resolve.
          detachFileWatcher();
          scheduleTick();
          return;
        }
        void readFrom();
      });
    } catch {
      // Couldn't attach a file watcher; the poll backstop still tails it.
    }
    void readFrom();
  };

  const ensureDirWatch = (): void => {
    if (dirWatcher || closed) return;
    try {
      dirWatcher = fs.watch(projectDir, { persistent: false }, () => {
        if (closed) return;
        scheduleTick();
      });
    } catch {
      // Project dir doesn't exist yet — poll will retry and attach later.
    }
  };

  // Core loop step: keep the dir watch alive, decide which file to
  // follow (with stickiness so we never drop a working file just because
  // the dir momentarily looks ambiguous), then tail it.
  const tick = async (): Promise<void> => {
    if (closed) return;
    ensureDirWatch();
    const res = resolveJsonlPath(projectDir, opts.claudeSessionId);
    if (res.path) {
      if (currentPath === null) {
        attachFile(res.path);
      } else if (currentPath !== res.path && res.reason === 'exact') {
        // The authoritative pinned file appeared (or changed) — prefer
        // it over whatever we fell back to.
        attachFile(res.path);
      }
      // else: stay on the file we're already following.
    } else if (currentPath === null && !unresolvedNotified) {
      // Genuinely can't identify the conversation (e.g. pinned id absent
      // and multiple JSONLs in the folder). Surface once; staying empty
      // is the correct failure mode — better than the wrong conversation.
      unresolvedNotified = true;
      emitter.emit(
        'error',
        new Error(`unresolved JSONL for ${opts.claudeSessionId ?? '(no id)'} in ${projectDir}: ${res.reason}`)
      );
    }
    await readFrom();
  };

  const scheduleTick = (): void => {
    if (tickScheduled || closed) return;
    tickScheduled = true;
    setTimeout(() => {
      tickScheduled = false;
      void tick();
    }, 100);
  };

  const close = (): void => {
    if (closed) return;
    closed = true;
    try { dirWatcher?.close(); } catch { /* ignore */ }
    dirWatcher = null;
    detachFileWatcher();
    if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
  };

  // Kick off: first attempt after the initial delay, then the poll
  // backstop. Never gives up — a session whose JSONL appears minutes
  // later (or rotates) still gets picked up.
  setTimeout(() => { if (!closed) void tick(); }, initialDelayMs);
  pollTimer = setInterval(() => { if (!closed) void tick(); }, pollIntervalMs);
  pollTimer.unref();

  return {
    emitter,
    path: () => currentPath,
    close
  };
}
