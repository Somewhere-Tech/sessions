// Resolving which Codex rollout JSONL belongs to a prettyd session.
//
// Codex writes rollouts under ~/.codex/sessions/YYYY/MM/DD as
// rollout-<iso-ts>-<uuid>.jsonl. Unlike Claude, fresh prettyd Codex
// launches do not pin the rollout id at spawn, so fresh-session
// resolution is necessarily best-effort: match cwd in the bounded date
// window first, then choose the nearest timestamp on a full-scan fallback.

import * as fs from 'node:fs';
import * as path from 'node:path';
import * as os from 'node:os';

const CODEX_SESSIONS_DIR = path.join(os.homedir(), '.codex', 'sessions');
// session_meta can include enough launch context to exceed 16 KiB. Keep
// reads bounded while leaving room for the metadata written by Codex.
const FIRST_LINE_BYTES = 65_536;

export type CodexResolveReason =
  | 'resume-match'
  | 'resume-missing'
  | 'fresh-match'
  | 'fresh-match-fullscan'
  | 'no-dir'
  | 'empty-dir'
  | 'no-cwd-match'
  | 'no-after-spawn';

export interface CodexResolution {
  path: string | null;
  reason: CodexResolveReason;
  ambiguousCount?: number;
}

interface RolloutCandidate {
  path: string;
  mtimeMs: number;
}

interface SessionMeta {
  cwd: string;
  timestampMs: number;
}

function isRecord(x: unknown): x is Record<string, unknown> {
  return typeof x === 'object' && x !== null && !Array.isArray(x);
}

function dateDir(d: Date): string {
  const yyyy = String(d.getFullYear());
  const mm = String(d.getMonth() + 1).padStart(2, '0');
  const dd = String(d.getDate()).padStart(2, '0');
  return path.join(CODEX_SESSIONS_DIR, yyyy, mm, dd);
}

function dateAncestorDirs(d: Date): string[] {
  const yyyy = String(d.getFullYear());
  const mm = String(d.getMonth() + 1).padStart(2, '0');
  const dd = String(d.getDate()).padStart(2, '0');
  return [
    CODEX_SESSIONS_DIR,
    path.join(CODEX_SESSIONS_DIR, yyyy),
    path.join(CODEX_SESSIONS_DIR, yyyy, mm),
    path.join(CODEX_SESSIONS_DIR, yyyy, mm, dd)
  ];
}

function unique(xs: string[]): string[] {
  return Array.from(new Set(xs));
}

function todayAndYesterday(now: Date = new Date()): Date[] {
  const yesterday = new Date(now);
  yesterday.setDate(yesterday.getDate() - 1);
  return [now, yesterday];
}

function sessionDates(now: Date, createdAt?: number): Date[] {
  const dates = todayAndYesterday(now);
  if (createdAt !== undefined && Number.isFinite(createdAt)) {
    dates.push(new Date(createdAt));
  }
  return dates;
}

export function codexFreshSessionDirs(now: Date = new Date(), createdAt?: number): string[] {
  return unique(sessionDates(now, createdAt).map(dateDir));
}

export function codexWatchDirs(now: Date = new Date(), createdAt?: number): string[] {
  return unique(sessionDates(now, createdAt).flatMap(dateAncestorDirs));
}

export function extractCodexResumeId(args: string[]): string | undefined {
  for (let i = 0; i < args.length; i++) {
    const flag = args[i];
    if (typeof flag !== 'string') continue;
    if (flag === 'resume' || flag === '--resume') {
      const v = args[i + 1];
      if (typeof v === 'string' && /^[0-9a-f-]{8,}$/i.test(v)) return v;
    }
    if (flag.startsWith('--resume=')) {
      const v = flag.slice('--resume='.length);
      if (/^[0-9a-f-]{8,}$/i.test(v)) return v;
    }
  }
  return undefined;
}

function listRolloutsInDir(dir: string): RolloutCandidate[] {
  let names: string[];
  try {
    names = fs.readdirSync(dir).filter((n) => n.startsWith('rollout-') && n.endsWith('.jsonl'));
  } catch {
    return [];
  }
  const out: RolloutCandidate[] = [];
  for (const name of names) {
    const p = path.join(dir, name);
    try {
      const st = fs.statSync(p);
      if (st.isFile()) out.push({ path: p, mtimeMs: st.mtimeMs });
    } catch {
      // Ignore files that disappear while scanning.
    }
  }
  return out;
}

function listRolloutsRecursive(root: string): RolloutCandidate[] {
  const out: RolloutCandidate[] = [];
  const stack = [root];
  while (stack.length > 0) {
    const dir = stack.pop();
    if (!dir) continue;
    let entries: fs.Dirent[];
    try {
      entries = fs.readdirSync(dir, { withFileTypes: true });
    } catch {
      continue;
    }
    for (const entry of entries) {
      const p = path.join(dir, entry.name);
      if (entry.isDirectory()) {
        stack.push(p);
      } else if (entry.isFile() && entry.name.startsWith('rollout-') && entry.name.endsWith('.jsonl')) {
        try {
          const st = fs.statSync(p);
          out.push({ path: p, mtimeMs: st.mtimeMs });
        } catch {
          // Ignore races with cleanup/rotation.
        }
      }
    }
  }
  return out;
}

function readFirstLine(filePath: string): string | null {
  let fd: number | null = null;
  try {
    fd = fs.openSync(filePath, 'r');
    const buf = Buffer.alloc(FIRST_LINE_BYTES);
    const bytesRead = fs.readSync(fd, buf, 0, buf.length, 0);
    if (bytesRead === 0) return null;
    const text = buf.subarray(0, bytesRead).toString('utf8');
    const nl = text.indexOf('\n');
    return (nl === -1 ? text : text.slice(0, nl)).trim();
  } catch {
    return null;
  } finally {
    if (fd !== null) {
      try { fs.closeSync(fd); } catch { /* ignore */ }
    }
  }
}

function readSessionMeta(filePath: string): SessionMeta | null {
  const line = readFirstLine(filePath);
  if (!line) return null;
  let parsed: unknown;
  try {
    parsed = JSON.parse(line) as unknown;
  } catch {
    return null;
  }
  if (!isRecord(parsed) || parsed.type !== 'session_meta' || !isRecord(parsed.payload)) return null;
  const cwd = parsed.payload.cwd;
  const payloadTs = parsed.payload.timestamp;
  const lineTs = parsed.timestamp;
  const timestamp = typeof payloadTs === 'string'
    ? payloadTs
    : (typeof lineTs === 'string' ? lineTs : null);
  if (typeof cwd !== 'string' || !timestamp) return null;
  const timestampMs = Date.parse(timestamp);
  if (!Number.isFinite(timestampMs)) return null;
  return { cwd, timestampMs };
}

function resolveResumed(args: string[]): CodexResolution | null {
  const resumeId = extractCodexResumeId(args);
  if (!resumeId) return null;
  const matches = listRolloutsRecursive(CODEX_SESSIONS_DIR)
    .filter((c) => path.basename(c.path).includes(resumeId));
  if (matches.length === 0) return { path: null, reason: 'resume-missing' };
  matches.sort((a, b) => {
    const mtime = b.mtimeMs - a.mtimeMs;
    return mtime !== 0 ? mtime : b.path.localeCompare(a.path);
  });
  return { path: matches[0]!.path, reason: 'resume-match' };
}

export function resolveCodexRolloutPath(opts: {
  cwd: string;
  args: string[];
  createdAt: number;
}): CodexResolution {
  const resumed = resolveResumed(opts.args);
  if (resumed) return resumed;

  // Reattached runners can be much older than the daemon process. Include
  // the runner's own start date while retaining today/yesterday for fresh
  // rollouts and midnight races. This stays bounded to at most three days.
  const dirs = codexFreshSessionDirs(new Date(), opts.createdAt);
  let sawDir = false;
  let sawFile = false;
  let sawCwdMatch = false;
  const matches: Array<RolloutCandidate & SessionMeta> = [];

  for (const dir of dirs) {
    if (fs.existsSync(dir)) sawDir = true;
    const files = listRolloutsInDir(dir);
    if (files.length > 0) sawFile = true;
    for (const file of files) {
      const meta = readSessionMeta(file.path);
      if (!meta || meta.cwd !== opts.cwd) continue;
      sawCwdMatch = true;
      if (meta.timestampMs < opts.createdAt) continue;
      matches.push({ ...file, ...meta });
    }
  }

  if (matches.length > 0) {
    matches.sort((a, b) => {
      const ts = a.timestampMs - b.timestampMs;
      return ts !== 0 ? ts : a.path.localeCompare(b.path);
    });
    return {
      path: matches[0]!.path,
      reason: 'fresh-match',
      ambiguousCount: matches.length > 1 ? matches.length : undefined
    };
  }

  if (!sawDir) return { path: null, reason: 'no-dir' };
  if (!sawFile) return { path: null, reason: 'empty-dir' };
  if (!sawCwdMatch) {
    const fullScanMatches: Array<RolloutCandidate & SessionMeta> = [];
    for (const file of listRolloutsRecursive(CODEX_SESSIONS_DIR)) {
      const meta = readSessionMeta(file.path);
      if (!meta || meta.cwd !== opts.cwd) continue;
      fullScanMatches.push({ ...file, ...meta });
    }

    if (fullScanMatches.length > 0) {
      fullScanMatches.sort((a, b) => {
        const distance = Math.abs(a.timestampMs - opts.createdAt)
          - Math.abs(b.timestampMs - opts.createdAt);
        return distance !== 0 ? distance : a.path.localeCompare(b.path);
      });
      return {
        path: fullScanMatches[0]!.path,
        reason: 'fresh-match-fullscan',
        ambiguousCount: fullScanMatches.length > 1 ? fullScanMatches.length : undefined
      };
    }

    return { path: null, reason: 'no-cwd-match' };
  }
  return { path: null, reason: 'no-after-spawn' };
}
