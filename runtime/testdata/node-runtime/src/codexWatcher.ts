// Tail Codex rollout JSONL and normalize each rollout item into the
// canonical Claude-shaped event stream consumed by the rest of prettyd.
//
// Read-only: input still goes through the PTY. This watcher only follows
// ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl and emits normalized
// events through the same `claudeEvent` plumbing Claude Code uses.

import { EventEmitter } from 'node:events';
import * as fs from 'node:fs';
import * as fsp from 'node:fs/promises';
import * as path from 'node:path';
import { StringDecoder } from 'node:string_decoder';
import type { SessionFileWatcher, ClaudeSessionEvent } from './sessionFileWatcher.js';
import { normalizeCodexRolloutLine } from './codexNormalize.js';
import { codexWatchDirs, resolveCodexRolloutPath } from './codexResolver.js';

interface WatcherOptions {
  cwd: string;
  args: string[];
  createdAt: number;
  // Deterministic path override for isolated watcher tests. Production
  // callers leave this unset and resolve the rollout from cwd/args.
  rolloutPath?: string;
  initialDelayMs?: number;
  pollIntervalMs?: number;
}

const BACKFILL_LINE_LIMIT = 2_000;
const READ_BYTE_LIMIT = 16 * 1024 * 1024;

function boundedBackfillStart(buf: Buffer, windowStart: number): number {
  let usableStart = 0;

  // A capped read can begin in the middle of a JSONL record. Drop that
  // partial record rather than feeding malformed content to the parser.
  if (windowStart > 0) {
    const firstNewline = buf.indexOf(0x0a);
    if (firstNewline === -1) return buf.length;
    usableStart = firstNewline + 1;
  }

  // Count physical lines backwards, treating an unterminated final record
  // as a line. The returned slice contains at most BACKFILL_LINE_LIMIT lines.
  let lines = buf.length > usableStart && buf[buf.length - 1] !== 0x0a ? 1 : 0;
  for (let i = buf.length - 1; i >= usableStart; i -= 1) {
    if (buf[i] !== 0x0a) continue;
    lines += 1;
    if (lines > BACKFILL_LINE_LIMIT) return i + 1;
  }
  return usableStart;
}

async function readRange(
  fh: fsp.FileHandle,
  start: number,
  length: number
): Promise<Buffer> {
  const buf = Buffer.allocUnsafe(length);
  let total = 0;
  while (total < length) {
    const { bytesRead } = await fh.read(buf, total, length - total, start + total);
    if (bytesRead === 0) break;
    total += bytesRead;
  }
  return total === length ? buf : buf.subarray(0, total);
}

export async function watchCodexRollout(opts: WatcherOptions): Promise<SessionFileWatcher> {
  const emitter = new EventEmitter();
  emitter.setMaxListeners(64);

  const initialDelayMs = opts.initialDelayMs ?? 800;
  const pollIntervalMs = opts.pollIntervalMs ?? 2000;

  let closed = false;
  let currentPath: string | null = null;
  let currentIno: number | null = null;
  let readOffset = 0;
  let lineBuffer = '';
  let decoder = new StringDecoder('utf8');
  let lineIndex = 0;
  let readingInFlight = false;
  let pendingRead = false;

  const dirWatchers = new Map<string, fs.FSWatcher>();
  let fileWatcher: fs.FSWatcher | null = null;
  let pollTimer: NodeJS.Timeout | null = null;
  let tickScheduled = false;
  let ambiguityWarnedFor: string | null = null;

  const detachFileWatcher = (): void => {
    try { fileWatcher?.close(); } catch { /* ignore */ }
    fileWatcher = null;
  };

  const emitNormalized = (ev: ClaudeSessionEvent): void => {
    emitter.emit('event', ev);
  };

  // Backfill and live bytes both enter here, so parsing, normalization,
  // working-state updates, and event emission cannot drift between paths.
  const consumeBytes = (buf: Buffer): void => {
    lineBuffer += decoder.write(buf);
    if (!currentPath) return;
    const rolloutBasename = path.basename(currentPath);
    let nl: number;
    while ((nl = lineBuffer.indexOf('\n')) !== -1) {
      const line = lineBuffer.slice(0, nl);
      lineBuffer = lineBuffer.slice(nl + 1);
      const thisLineIndex = lineIndex;
      lineIndex += 1;
      if (!line.trim()) continue;
      let parsed: unknown;
      try {
        parsed = JSON.parse(line) as unknown;
      } catch {
        continue;
      }
      const normalized = normalizeCodexRolloutLine(parsed, {
        rolloutBasename,
        lineIndex: thisLineIndex
      });
      for (const ev of normalized.events) emitNormalized(ev);
      if (normalized.working !== null) emitter.emit('working', normalized.working);
    }
  };

  const resetReadState = (): void => {
    readOffset = 0;
    lineBuffer = '';
    lineIndex = 0;
    decoder = new StringDecoder('utf8');
  };

  const readFrom = async (): Promise<void> => {
    if (closed || !currentPath) return;
    if (readingInFlight) { pendingRead = true; return; }
    readingInFlight = true;
    const targetPath = currentPath;
    try {
      let fh: fsp.FileHandle;
      try {
        fh = await fsp.open(targetPath, 'r');
      } catch {
        if (currentPath === targetPath) {
          detachFileWatcher();
          currentPath = null;
          currentIno = null;
          resetReadState();
        }
        return;
      }
      try {
        const st = await fh.stat();
        if (closed || currentPath !== targetPath) return;

        const needsBackfill = currentIno === null
          || st.ino !== currentIno
          || st.size < readOffset;

        if (needsBackfill) {
          // Snapshot the attach boundary, replay only its bounded tail, then
          // advance to that exact byte. Any append after the snapshot is read
          // by the serialized live pass below, so replay and tail cannot
          // overlap or leave a gap.
          const snapshotEnd = st.size;
          const windowStart = Math.max(0, snapshotEnd - READ_BYTE_LIMIT);
          const window = await readRange(fh, windowStart, snapshotEnd - windowStart);
          if (closed || currentPath !== targetPath) return;

          resetReadState();
          currentIno = st.ino;
          readOffset = windowStart + window.length;
          const replayStart = boundedBackfillStart(window, windowStart);
          consumeBytes(window.subarray(replayStart));

          // A short read means the file changed under us. Re-enter through
          // the same serialized reader to reconcile with its current size.
          if (readOffset < snapshotEnd) pendingRead = true;
          return;
        }

        if (st.size <= readOffset) return;
        const liveEnd = Math.min(st.size, readOffset + READ_BYTE_LIMIT);
        const buf = await readRange(fh, readOffset, liveEnd - readOffset);
        if (closed || currentPath !== targetPath) return;
        readOffset += buf.length;
        consumeBytes(buf);
        if (readOffset < st.size) pendingRead = true;
      } finally {
        await fh.close();
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
    resetReadState();
    ensureDirWatch(path.dirname(p));
    try {
      fileWatcher = fs.watch(p, { persistent: false }, (eventType) => {
        if (closed) return;
        if (eventType === 'rename') {
          detachFileWatcher();
          scheduleTick();
          return;
        }
        void readFrom();
      });
    } catch {
      // Polling still tails the file if fs.watch cannot attach.
    }
    void readFrom();
  };

  const ensureDirWatch = (dir: string): void => {
    if (closed || dirWatchers.has(dir)) return;
    try {
      const watcher = fs.watch(dir, { persistent: false }, () => {
        if (closed) return;
        scheduleTick();
      });
      dirWatchers.set(dir, watcher);
    } catch {
      // Directory may not exist yet; poll will retry after Codex creates it.
    }
  };

  const ensureDirWatches = (): void => {
    for (const dir of codexWatchDirs(new Date(), opts.createdAt)) ensureDirWatch(dir);
    if (currentPath) ensureDirWatch(path.dirname(currentPath));
  };

  const tick = async (): Promise<void> => {
    if (closed) return;
    ensureDirWatches();
    if (opts.rolloutPath) {
      attachFile(opts.rolloutPath);
      await readFrom();
      return;
    }
    const res = resolveCodexRolloutPath({
      cwd: opts.cwd,
      args: opts.args,
      createdAt: opts.createdAt
    });
    if (res.path) {
      if (res.ambiguousCount && ambiguityWarnedFor !== res.path) {
        ambiguityWarnedFor = res.path;
        console.warn(
          `[codex-rollout] ambiguous fresh session for ${opts.cwd}; using ${path.basename(res.path)} among ${res.ambiguousCount} cwd matches`
        );
      }
      if (currentPath === null) {
        attachFile(res.path);
      } else if (currentPath !== res.path && res.reason === 'resume-match') {
        attachFile(res.path);
      }
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
    for (const watcher of dirWatchers.values()) {
      try { watcher.close(); } catch { /* ignore */ }
    }
    dirWatchers.clear();
    detachFileWatcher();
    if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
  };

  setTimeout(() => { if (!closed) void tick(); }, initialDelayMs);
  pollTimer = setInterval(() => { if (!closed) void tick(); }, pollIntervalMs);
  pollTimer.unref();

  return {
    emitter,
    path: () => currentPath,
    close
  };
}
