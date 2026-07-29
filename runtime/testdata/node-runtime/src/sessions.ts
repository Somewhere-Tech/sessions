import { randomUUID } from 'node:crypto';
import { EventEmitter } from 'node:events';
import { fileURLToPath } from 'node:url';
import { createRequire } from 'node:module';
import { spawn, spawnSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { config } from './config.js';
import { RUNNER_PROTOCOL_VERSION } from './runnerProtocol.js';
import { EventLog, type OutputEvent } from './eventLog.js';
import { RunnerClient } from './runnerClient.js';
import { classifyTool } from './types.js';
import { bootstrapRunner, bootoutRunner, cleanupOrphanPlists } from './launchd.js';
import type { CreateSessionRequest, SessionInfo } from './types.js';
import { watchSessionFile, type SessionFileWatcher, type ClaudeSessionEvent } from './sessionFileWatcher.js';
import { claudeWorkingFromSnapshot } from './claudeActivity.js';
import { watchCodexRollout } from './codexWatcher.js';
import { sendPush } from './push.js';

// Same trick runner.ts uses to bypass @xterm/headless's broken ESM facade.
const xtermRequire = createRequire(import.meta.url);
const { Terminal } = xtermRequire('@xterm/headless') as typeof import('@xterm/headless');
const { SerializeAddon } = xtermRequire('@xterm/addon-serialize') as typeof import('@xterm/addon-serialize');

type HeadlessTerminal = InstanceType<typeof Terminal>;
type HeadlessSerializeAddon = InstanceType<typeof SerializeAddon>;

interface SessionInternal {
  info: SessionInfo;
  client: RunnerClient;
  emitter: EventEmitter;
  log: EventLog;
  // Server-side xterm-headless mirror, fed from the same OUTPUT stream
  // the EventLog consumes. Snapshots serialize from this so the result
  // includes scrollback even when the runner subprocess is locked at
  // scrollback:0 by an old code version (e.g. fit-furniture's runner
  // started before the snapshot:scrollback bump). Memory cost is one
  // additional headless terminal per session; with scrollback 5000 ×
  // ~120-300 cols that's a few MB worst case.
  mirrorTerm: HeadlessTerminal;
  mirrorSerialize: HeadlessSerializeAddon;
  exited: boolean;
  exitCode: number | null;
  exitSignal: string | null;
  exitSeq: number | null;
  recentBytes: number;
  // JSONL watcher for Claude Code sessions and Codex rollouts. Reads
  // structured events and re-emits the canonical Claude-shaped stream
  // through `emitter.emit('claudeEvent', ev)`. Null for terminal
  // sessions or before the backing file is resolved.
  fileWatcher: SessionFileWatcher | null;
  // Codex's rollout lifecycle is the source of truth once observed.
  // Until then, fall back to the old byte-rate signal so sessions still
  // show activity while the rollout file is being created.
  codexLifecycleWorking: boolean | null;
  // History of events seen so far on this session. Lets new WS
  // connections replay everything Claude has emitted since this
  // prettyd started, same shape as how OutputEvent replay works.
  claudeEventLog: ClaudeSessionEvent[];
  // Number of events evicted from the FRONT of claudeEventLog by the
  // 5000-cap trim. claudeEventLog[i] has absolute index claudeEventBase+i.
  // WS ?claudeEventsSince= / HTTP ?since= are absolute indices, so without
  // this offset a reconnect after a trim would silently skip events.
  claudeEventBase: number;
  // Push notifications should only fire on observed working true → false
  // edges. The first detector sample after daemon start/register is just
  // initialization, not a completion event.
  pushWorkingObserved: boolean;
  // Wall-clock start of the current working stretch. Used only when the
  // matching working → idle edge fires, then cleared.
  workingStartedAt: number | null;
}

export type { OutputEvent };
export type SessionHandle = SessionInternal;

const STATE_DIR = process.env.PRETTYD_STATE_DIR
  ?? path.join(os.homedir(), '.local', 'state', 'pretty-PTY', 'runners');
const IDLE_DIR = path.join(os.homedir(), '.local', 'state', 'pretty-PTY', 'idle');
const GLOBAL_HOOKS_FILE = path.join(os.homedir(), '.config', 'pretty', 'hooks.json');

interface GlobalHooksConfig {
  onIdle?: string;
}

export type IdleOutcome = 'done' | 'blocked' | 'error';

interface IdleClassification {
  outcome: IdleOutcome;
  line: string | null;
}

interface IdleHookContext {
  summary: string | null;
  outcome: IdleOutcome;
  durationMs: number;
}

function loadGlobalHooks(): GlobalHooksConfig {
  if (!fs.existsSync(GLOBAL_HOOKS_FILE)) return {};
  try {
    const parsed: unknown = JSON.parse(fs.readFileSync(GLOBAL_HOOKS_FILE, 'utf8'));
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
      throw new Error('expected an object');
    }
    const onIdle = (parsed as { onIdle?: unknown }).onIdle;
    if (onIdle !== undefined && (typeof onIdle !== 'string' || onIdle.trim().length === 0)) {
      throw new Error('onIdle must be a non-empty string');
    }
    return typeof onIdle === 'string' ? { onIdle } : {};
  } catch (err) {
    console.warn(`[hooks] ignoring malformed ${GLOBAL_HOOKS_FILE}: ${(err as Error).message}`);
    return {};
  }
}

const globalHooks = loadGlobalHooks();

const sessions = new Map<string, SessionInternal>();

const WORKING_BYTES_THRESHOLD = 80;
const WORKING_DECAY_MS = 800;
const READY_WAIT_MS = 30_000;
const READY_SETTLE_MS = 800;
// How long an exited session stays visible to /api/sessions before being
// dropped. Lets `pretty ls --include-exited` and the UI tab strip show
// "exit code 0" briefly without showing ghost sessions forever.
const EXITED_GRACE_MS = 30_000;

function sessionDisplayLabel(info: SessionInfo): string {
  if (info.name) return info.name;
  if (info.claudeCustomTitle) return info.claudeCustomTitle;
  if (info.claudeAiTitle) return info.claudeAiTitle;
  const cwdBase = info.cwd.split('/').filter(Boolean).pop();
  if (cwdBase) return cwdBase;
  if (info.cmd) return info.cmd;
  return info.id.slice(0, 8);
}

function removeIdleSentinel(id: string): void {
  try { fs.unlinkSync(path.join(IDLE_DIR, id)); } catch { /* absent or unreadable — non-fatal */ }
}

function writeIdleSentinel(info: SessionInfo): void {
  try {
    fs.mkdirSync(IDLE_DIR, { recursive: true, mode: 0o700 });
    const body = {
      id: info.id,
      name: sessionDisplayLabel(info),
      at: new Date().toISOString()
    };
    fs.writeFileSync(path.join(IDLE_DIR, info.id), JSON.stringify(body) + '\n', { mode: 0o600 });
  } catch {
    // Completion markers are best-effort. Never let filesystem state
    // interfere with session activity tracking.
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function conciseText(text: string, maxLength: number = 100): string | null {
  const cleaned = text
    .replace(/```[^\n]*\n?/g, ' ')
    .replace(/!\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/\[([^\]]+)\]\([^)]*\)/g, '$1')
    .replace(/^\s{0,3}(?:#{1,6}|>|[-+*]|\d+[.)])\s+/gm, '')
    .replace(/<[^>]+>/g, ' ')
    .replace(/[*_~`]+/g, '')
    .replace(/\s+/g, ' ')
    .trim();
  if (!cleaned) return null;

  const sentenceMatch = /[.!?](?=\s|$)/.exec(cleaned);
  const firstSentence = sentenceMatch
    ? cleaned.slice(0, sentenceMatch.index + 1)
    : cleaned;
  if (firstSentence.length <= maxLength) return firstSentence;

  const prefix = firstSentence.slice(0, Math.max(1, maxLength - 1));
  const lastSpace = prefix.lastIndexOf(' ');
  const cutAt = lastSpace >= Math.floor(maxLength * 0.6) ? lastSpace : prefix.length;
  return `${prefix.slice(0, cutAt).trimEnd()}…`;
}

function assistantText(event: ClaudeSessionEvent): string | null {
  if (event.type !== 'assistant' || event.message?.role !== 'assistant') return null;
  const content = event.message.content;
  if (typeof content === 'string') return content;
  if (!Array.isArray(content)) return null;

  const parts: string[] = [];
  for (const block of content) {
    if (!isRecord(block) || block.type !== 'text' || typeof block.text !== 'string') continue;
    parts.push(block.text);
  }
  return parts.length > 0 ? parts.join(' ') : null;
}

export function finalAssistantSummary(log: readonly ClaudeSessionEvent[]): string | null {
  try {
    for (let i = log.length - 1; i >= 0; i--) {
      const text = assistantText(log[i]!);
      if (!text) continue;
      const summary = conciseText(text);
      if (summary) return summary;
    }
  } catch {
    // Structured session events are best-effort input. A malformed event
    // must never interfere with the working-state transition.
  }
  return null;
}

function stripTerminalControls(text: string): string {
  return text
    .replace(/\u001B\][^\u0007]*(?:\u0007|\u001B\\)/g, '')
    .replace(/\u001B[P^_][\s\S]*?\u001B\\/g, '')
    .replace(/\u001B\[[0-?]*[ -/]*[@-~]/g, '')
    .replace(/\u001B[@-_]/g, '')
    .replace(/[^\x09\x0A\x0D\x20-\uFFFF]/g, '');
}

function snapshotLines(snapshot: string): string[] {
  return stripTerminalControls(snapshot)
    .replace(/\r/g, '')
    .split('\n')
    .map((line) => line
      .replace(/^[\s│┃║╎╏┆┊]+/, '')
      .replace(/[\s│┃║╎╏┆┊]+$/, '')
      .replace(/[ \t]+/g, ' '))
    .filter((line) => line.length > 0)
    .slice(-20);
}

function displayLine(line: string): string {
  const shortened = line.length > 180 ? `${line.slice(0, 179).trimEnd()}…` : line;
  return shortened.replace(/^\s*[❯›]\s*/, '').trim();
}

const INPUT_PROMPT_RE = /\b(?:y\/n|yes\/no|do you want)\b|\[[yn]\/[yn]\]|\b(?:continue|proceed)\s*\?|\?\s*$/i;
const PERMISSION_PROMPT_RE = /^\s*[❯›]\s*(?:approve|allow|trust)\b|\b(?:approve|allow|trust)\b.*(?:\?|:)\s*$/i;
const CHOICE_PROMPT_RE = /\b(?:which|select|choose)\b.*(?:\?|:)\s*$/i;
const NUMBERED_CHOICE_RE = /^\s*(?:[>❯›^]\s*)?\d+[.)]\s+\S/;
const SELECTED_NUMBERED_CHOICE_RE = /^\s*[>❯›^]\s*\d+[.)]\s+\S/;
const SELECTED_CHOICE_RE = /^\s*[❯›]\s+\S/;
const OTHER_CHOICE_RE = /^\s*(?:[○◯●◉]|\[[ x]\])\s+\S/i;
const ERROR_RE = /\b(?:error|failed|exception|panic|traceback|fatal)\b/i;
const BENIGN_ERROR_RE = /\b(?:0\s+(?:errors?|fail(?:ed|ures?)?)|no\s+(?:errors?|failures?))\b/i;
const RESOLUTION_RE = /\b(?:resolved|recovered|fixed|succeeded|successful|passed|completed|all checks pass|done)\b/i;

function classifySnapshot(snapshot: string): IdleClassification {
  const lines = snapshotLines(snapshot);
  const trailing = lines.slice(-12);

  for (let i = trailing.length - 1; i >= 0; i--) {
    const line = trailing[i]!;
    if (INPUT_PROMPT_RE.test(line) || PERMISSION_PROMPT_RE.test(line) || CHOICE_PROMPT_RE.test(line)) {
      return { outcome: 'blocked', line: displayLine(line) };
    }
  }

  const numberedChoices = trailing.filter((line) => NUMBERED_CHOICE_RE.test(line));
  const selectedNumberedChoice = trailing.find((line) => SELECTED_NUMBERED_CHOICE_RE.test(line));
  if (numberedChoices.length >= 2 && selectedNumberedChoice) {
    const prompt = [...trailing]
      .reverse()
      .find((line) => !NUMBERED_CHOICE_RE.test(line) && /[:?]\s*$/.test(line));
    return { outcome: 'blocked', line: displayLine(prompt ?? selectedNumberedChoice) };
  }

  const selectedChoice = trailing.find((line) => SELECTED_CHOICE_RE.test(line));
  if (selectedChoice && trailing.some((line) => line !== selectedChoice && OTHER_CHOICE_RE.test(line))) {
    return { outcome: 'blocked', line: displayLine(selectedChoice) };
  }

  for (let i = trailing.length - 1; i >= 0; i--) {
    const line = trailing[i]!;
    if (!ERROR_RE.test(line)) continue;
    if (BENIGN_ERROR_RE.test(line)) continue;
    const followingLines = trailing.slice(i + 1);
    if (followingLines.some((following) => RESOLUTION_RE.test(following))) {
      return { outcome: 'done', line: null };
    }
    return { outcome: 'error', line: displayLine(line) };
  }

  return { outcome: 'done', line: null };
}

function mirrorSnapshot(internal: Pick<SessionInternal, 'mirrorSerialize'>): string | null {
  try {
    return internal.mirrorSerialize.serialize({ scrollback: 1000 });
  } catch {
    return null;
  }
}

export function classifyIdleReason(
  internal: Pick<SessionInternal, 'mirrorSerialize'>,
  capturedSnapshot?: string | null
): IdleOutcome {
  try {
    const snapshot = capturedSnapshot === undefined ? mirrorSnapshot(internal) : capturedSnapshot;
    return snapshot === null ? 'done' : classifySnapshot(snapshot).outcome;
  } catch {
    return 'done';
  }
}

function mirrorTailSummary(snapshot: string | null): string | null {
  if (snapshot === null) return null;
  try {
    const lines = snapshotLines(snapshot);
    for (let i = lines.length - 1; i >= 0; i--) {
      const line = lines[i]!;
      if (!/[\p{L}\p{N}]/u.test(line)) continue;
      if (/\b(?:esc to interrupt|shift\+tab|for shortcuts|context left|bypass permissions|accept edits)\b/i.test(line)) continue;
      const summary = conciseText(line.replace(/^\s*[⏺●•✻✽✶❯›]+\s*/, ''));
      if (summary) return summary;
    }
  } catch {
    // Mirror snapshots are a fallback only.
  }
  return null;
}

function hookEnvironment(info: SessionInfo, context: IdleHookContext): NodeJS.ProcessEnv {
  return {
    ...process.env,
    PRETTY_SESSION_ID: info.id,
    PRETTY_SESSION_NAME: sessionDisplayLabel(info),
    PRETTY_SESSION_TOOL: info.tool,
    PRETTY_SESSION_CWD: info.cwd,
    PRETTY_FINAL_MESSAGE: context.summary ?? '',
    PRETTY_OUTCOME: context.outcome,
    PRETTY_DURATION_MS: String(context.durationMs)
  };
}

function runOnIdleHook(info: SessionInfo, context: IdleHookContext): void {
  if (!info.onIdle) return;
  try {
    const child = spawn('/bin/sh', ['-c', info.onIdle], {
      cwd: info.cwd,
      detached: true,
      stdio: 'ignore',
      env: hookEnvironment(info, context)
    });
    child.on('error', () => { /* fire-and-forget hook failed to spawn */ });
    child.unref();
  } catch {
    // Hooks are user-supplied and must not throw into session handling.
  }
}

function runGlobalOnIdleHook(info: SessionInfo, context: IdleHookContext): void {
  if (!globalHooks.onIdle) return;
  try {
    const child = spawn('/bin/sh', ['-c', globalHooks.onIdle], {
      cwd: info.cwd,
      detached: true,
      stdio: 'ignore',
      env: hookEnvironment(info, context)
    });
    const killTimer = setTimeout(() => {
      try { child.kill(); } catch { /* hook already exited */ }
    }, 30_000);
    killTimer.unref();
    child.once('exit', () => clearTimeout(killTimer));
    child.once('error', (err) => {
      clearTimeout(killTimer);
      console.warn(`[hooks] global onIdle failed to spawn: ${err.message}`);
    });
    child.unref();
  } catch (err) {
    console.warn(`[hooks] global onIdle failed to spawn: ${(err as Error).message}`);
  }
}

function setWorkingState(internal: SessionInternal, nextWorking: boolean): void {
  const previous = internal.info.working;
  internal.info.working = nextWorking;
  if (!previous && nextWorking) {
    internal.workingStartedAt = Date.now();
    removeIdleSentinel(internal.info.id);
  }
  if (!internal.pushWorkingObserved) {
    internal.pushWorkingObserved = true;
    return;
  }
  if (previous && !nextWorking) {
    const idleAt = Date.now();
    const durationMs = Math.max(0, idleAt - (internal.workingStartedAt ?? idleAt));
    internal.workingStartedAt = null;
    if (internal.exited) return;

    const label = sessionDisplayLabel(internal.info);
    const snapshot = mirrorSnapshot(internal);
    const outcome = classifyIdleReason(internal, snapshot);
    const classification = snapshot === null
      ? { outcome: 'done', line: null } satisfies IdleClassification
      : classifySnapshot(snapshot);
    const summary = finalAssistantSummary(internal.claudeEventLog) ?? mirrorTailSummary(snapshot);
    const context: IdleHookContext = { summary, outcome, durationMs };
    const title = outcome === 'blocked'
      ? `🟡 ${label} — needs you`
      : outcome === 'error'
        ? `🔴 ${label} — hit an error`
        : `🟢 ${label} — done`;
    const body = outcome === 'blocked'
      ? classification.line ?? summary ?? 'waiting for input'
      : outcome === 'error'
        ? classification.line ?? summary ?? 'error detected'
        : summary ?? 'finished';

    writeIdleSentinel(internal.info);
    runOnIdleHook(internal.info, context);
    runGlobalOnIdleHook(internal.info, context);
    void sendPush({
      title,
      body,
      data: { sessionId: internal.info.id }
    }).catch(() => { /* push delivery is best-effort */ });
  }
}

function normalizedOptionalString(value: string | undefined): string | undefined {
  if (typeof value !== 'string') return undefined;
  const trimmed = value.trim();
  return trimmed.length > 0 ? trimmed : undefined;
}

function argValue(args: string[], names: string[]): string | undefined {
  for (let i = 0; i < args.length - 1; i++) {
    if (names.includes(args[i]!)) return args[i + 1];
  }
  return undefined;
}

function configArgValue(args: string[], key: string): string | undefined {
  for (let i = 0; i < args.length - 1; i++) {
    if (args[i] !== '-c' && args[i] !== '--config') continue;
    const value = args[i + 1]!;
    if (!value.startsWith(key + '=')) continue;
    return value.slice(key.length + 1).replace(/^(["'])(.*)\1$/, '$2');
  }
  return undefined;
}

function spawnControls(tool: SessionInfo['tool'], args: string[]): Pick<SessionInfo, 'model' | 'effort' | 'fast'> {
  const model = argValue(args, ['--model', '-m']);
  const effort = tool === 'codex'
    ? configArgValue(args, 'model_reasoning_effort')
    : argValue(args, ['--effort']);
  const fast = tool === 'codex' && configArgValue(args, 'service_tier') === 'priority';
  return {
    ...(model !== undefined ? { model } : {}),
    ...(effort !== undefined ? { effort } : {}),
    ...(fast ? { fast: true } : {})
  };
}

function structuredEventsCanSignalReady(info: SessionInfo): boolean {
  return info.tool === 'claude-code' || info.tool === 'codex';
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function waitForSessionReady(internal: SessionInternal): Promise<void> {
  if (internal.claudeEventLog.length > 0 || !structuredEventsCanSignalReady(internal.info)) {
    await sleep(READY_SETTLE_MS);
    return;
  }

  await new Promise<void>((resolve) => {
    let done = false;
    const finish = (): void => {
      if (done) return;
      done = true;
      clearTimeout(settleTimer);
      clearTimeout(capTimer);
      internal.emitter.off('claudeEvent', finish);
      internal.emitter.off('exit', finish);
      internal.emitter.off('runner-lost', finish);
      resolve();
    };
    const settleTimer = setTimeout(finish, READY_SETTLE_MS);
    const capTimer = setTimeout(finish, READY_WAIT_MS);
    internal.emitter.on('claudeEvent', finish);
    internal.emitter.on('exit', finish);
    internal.emitter.on('runner-lost', finish);
    if (internal.claudeEventLog.length > 0 || internal.exited) finish();
  });
}

setInterval(() => {
  for (const s of sessions.values()) {
    if (s.exited) continue;
    s.recentBytes = Math.floor(s.recentBytes / 2);
    const byteWorking = s.recentBytes >= WORKING_BYTES_THRESHOLD;
    let nextWorking: boolean;
    // Byte-rate lies for Claude Code: a custom statusline (e.g. the
    // user's `/goal active (3d)◎`) repaints continuously while idle, so
    // the PTY never goes quiet and `working` would be pinned true. JSONL
    // append-rate lies the other way (silent for minutes mid-turn). The
    // honest signal is Claude's own "· esc to interrupt" footer, which we
    // read off the headless mirror we already feed for snapshots. Fall
    // back to byte-rate only if the mirror serialize hiccups. Codex
    // uses rollout task_started/task_complete once those events are seen.
    if (s.info.tool === 'claude-code') {
      if (s.recentBytes <= 0) {
        // No PTY output since the last tick → the screen can't have changed,
        // so the "esc to interrupt" footer can't have appeared: it's idle.
        // Skip the serialize entirely. This is the big saving — across many
        // background sessions, the 800ms loop was serializing every Claude
        // mirror (incl. the active 254×127 one) every tick regardless of
        // whether anything happened. A truly-working session keeps emitting
        // its spinner, so recentBytes stays > 0 and we still serialize it.
        nextWorking = false;
      } else {
        try {
          nextWorking = claudeWorkingFromSnapshot(s.mirrorSerialize.serialize({ scrollback: 0 }));
        } catch {
          nextWorking = byteWorking;
        }
      }
    } else if (s.info.tool === 'codex') {
      nextWorking = s.codexLifecycleWorking ?? byteWorking;
    } else {
      nextWorking = byteWorking;
    }
    setWorkingState(s, nextWorking);
  }
}, WORKING_DECAY_MS).unref();

// Locate the runner program arguments. Both dev (tsx) and prod (node)
// paths use process.execPath as argv[0] — that's the absolute path to
// the running node binary, which works under launchd's minimal PATH.
//
// Preference order:
//   1. runner.js next to this file (prod build — running from dist/).
//   2. ../dist/runner.js (dev: prettyd is launched via tsx-watch on
//      src/, but dist/ already has compiled artifacts from a prior
//      `npm run build`). Only used when dist is at least as fresh as
//      src — tsx-watch auto-rebuilds tsx code on save, so falling back
//      to a stale dist would launch new sessions on old code.
//   3. tsx + runner.ts — true dev, no build artifacts. Slow because
//      each new session pays the tsx cold-start cost (~30-60s).
// Pull the Claude session UUID out of the command-line args, looking
// for either `--session-id <uuid>` (we set this for fresh sessions)
// or `--resume <uuid>` (set when resuming a specific session). The
// uuid IS the JSONL filename, so the watcher can lock onto the right
// file deterministically.
// True when a JSONL user event carries text the human actually typed —
// not tool_result loop feedback, and not the system-inserted pseudo
// messages Claude writes for its own bookkeeping (<command-name>,
// <system-reminder>, compact/continue banners, interrupt sentinels).
// Used to maintain SessionInfo.lastUserMessageAt.
function isRealUserMessage(ev: ClaudeSessionEvent): boolean {
  if (ev.type !== 'user' || ev.message?.role !== 'user') return false;
  const c = ev.message.content;
  let text: string | null = null;
  if (typeof c === 'string') {
    text = c;
  } else if (Array.isArray(c)) {
    const blocks = c as Array<{ type?: string; text?: string }>;
    if (blocks.length === 0) return false;
    if (blocks.every((b) => b?.type === 'tool_result')) return false;
    const t = blocks.find((b) => b?.type === 'text' && typeof b.text === 'string');
    // No text block but e.g. an image attachment still counts as the user
    // acting; require SOME non-tool_result block, which we already have.
    text = t?.text ?? '';
  } else {
    return false;
  }
  const tt = (text ?? '').trimStart();
  if (tt.startsWith('<')) return false; // <command-name>/<system-reminder>/…
  if (tt.startsWith('Caveat:')) return false;
  if (tt.startsWith('This session is being continued')) return false;
  if (tt.startsWith('[Request interrupted')) return false;
  return true;
}

function extractClaudeSessionId(args: string[]): string | undefined {
  for (let i = 0; i < args.length - 1; i++) {
    const flag = args[i];
    if (flag === '--session-id' || flag === '--resume') {
      const v = args[i + 1];
      if (typeof v === 'string' && /^[0-9a-f-]{8,}$/i.test(v)) return v;
    }
  }
  return undefined;
}

function recordStructuredSessionEvent(internal: SessionInternal, ev: ClaudeSessionEvent): void {
  internal.claudeEventLog.push(ev);
  // Bound the log so a long-running session doesn't grow unbounded.
  // 5000 events ≈ several days of typical use. Track how many we drop
  // from the front so absolute indices (used by WS replay + HTTP
  // /events?since=) stay correct across the trim.
  if (internal.claudeEventLog.length > 5000) {
    const removed = internal.claudeEventLog.length - 5000;
    internal.claudeEventLog.splice(0, removed);
    internal.claudeEventBase += removed;
  }
  // Track when the user last actually typed something — the staleness
  // signal `pretty ls` surfaces so old sessions can be culled.
  // Timestamps come from the event itself (watchers replay history from
  // byte 0 on attach, so Date.now() would stamp every historical
  // message as "just now").
  if (isRealUserMessage(ev) && typeof ev.timestamp === 'string') {
    const ts = Date.parse(ev.timestamp);
    if (Number.isFinite(ts) && ts > (internal.info.lastUserMessageAt ?? 0)) {
      internal.info.lastUserMessageAt = ts;
    }
  }
  // Surface Claude's own session titles. Codex simply won't emit these.
  const t = (ev as { type?: string }).type;
  if (t === 'custom-title') {
    const v = (ev as { customTitle?: string }).customTitle;
    if (typeof v === 'string' && v.length > 0) internal.info.claudeCustomTitle = v;
  } else if (t === 'ai-title') {
    const v = (ev as { aiTitle?: string }).aiTitle;
    if (typeof v === 'string' && v.length > 0) internal.info.claudeAiTitle = v;
  }
  internal.emitter.emit('claudeEvent', ev);
}

function resolveRunnerProgramArguments(): string[] {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const sideBySide = path.join(here, 'runner.js');
  if (fs.existsSync(sideBySide)) {
    // Use /usr/bin/env node rather than process.execPath so the plist
    // survives `brew upgrade node`. process.execPath bakes the versioned
    // Cellar path (/opt/homebrew/Cellar/node/22.x.y/bin/node) into every
    // plist; after a major version bump that path is gone and launchd can
    // no longer restart any session on reboot.
    return ['/usr/bin/env', 'node', sideBySide];
  }
  const tsPath = path.join(here, 'runner.ts');
  const distSibling = path.join(here, '..', 'dist', 'runner.js');
  if (fs.existsSync(distSibling) && fs.existsSync(tsPath)) {
    try {
      const distMtime = fs.statSync(distSibling).mtimeMs;
      const srcMtime = fs.statSync(tsPath).mtimeMs;
      if (distMtime >= srcMtime) {
        return ['/usr/bin/env', 'node', distSibling];
      }
    } catch {
      // fall through to tsx
    }
  }
  if (fs.existsSync(tsPath)) {
    const tsxBin = path.join(here, '..', 'node_modules', '.bin', 'tsx');
    if (!fs.existsSync(tsxBin)) {
      throw new Error(`runner needs tsx but ${tsxBin} not found; run npm install in prettyd/`);
    }
    // tsx is a local JS script so `node tsxBin` works fine.
    // Still avoid process.execPath here for the same brew-upgrade reason.
    return ['/usr/bin/env', 'node', tsxBin, tsPath];
  }
  throw new Error(`runner not found near ${here}`);
}

// Attempt to reconnect to a runner that disconnected unexpectedly (crash /
// KeepAlive-triggered restart). Uses a short fixed back-off sequence and
// aborts without reaping if all attempts fail — sessions-are-sacred.
// Idempotent: the first check inside each attempt bails out if the session
// was already re-registered (e.g. by discoverRunners on a concurrent start).
function scheduleRunnerReconnect(id: string, sockPath: string, delays: number[]): void {
  if (delays.length === 0) return; // budget exhausted; leave it for next prettyd start
  const [delay, ...rest] = delays;
  setTimeout(async () => {
    if (sessions.has(id)) return; // already re-registered by another path
    if (!fs.existsSync(sockPath)) {
      // Socket not yet recreated by the respawned runner; retry later.
      scheduleRunnerReconnect(id, sockPath, rest);
      return;
    }
    try {
      await registerRunner(sockPath);
      console.log(`[reconnect] runner ${id} reattached after unexpected disconnect`);
    } catch {
      // Not ready yet; try again with the remaining delay budget.
      scheduleRunnerReconnect(id, sockPath, rest);
    }
  }, delay).unref();
}

// Skip-permissions is the product default for BOTH agent tools, on EVERY
// entry path (CLI, web UI, raw API). The CLI and dialog already inject the
// right flags, but a bare POST {cmd:"codex"} used to spawn codex with its
// per-command approval layer ON — sessions that silently hang on every
// action. This is the daemon-side guarantee: if the command IS a known
// tool and the caller expressed no explicit approval/sandbox choice,
// inject that tool's full-access default. Callers that pass any explicit
// mode flag are respected untouched.
const TOOL_DEFAULT_ARGS: Record<string, string[]> = {
  claude: ['--dangerously-skip-permissions'],
  codex: ['-c', 'check_for_update_on_startup=false', '--dangerously-bypass-approvals-and-sandbox']
};
const EXPLICIT_MODE_FLAGS = new Set([
  '--dangerously-bypass-approvals-and-sandbox',
  '--dangerously-skip-permissions',
  '--sandbox', '-s',
  '--ask-for-approval', '-a',
  '--full-auto'
]);

function withToolDefaultArgs(cmd: string, args: string[]): string[] {
  const base = cmd.split('/').pop()?.toLowerCase() ?? '';
  const defaults = TOOL_DEFAULT_ARGS[base];
  if (!defaults) return args;
  if (args.some((a) => EXPLICIT_MODE_FLAGS.has(a))) return args;
  // APPEND the defaults: codex's flags are accepted after a subcommand
  // (`codex resume <uuid> --dangerously-bypass…` — verified) but NOT
  // reliably before one, so defaults-last is the order that works for
  // both `codex` and `codex resume …` (and is harmless for claude).
  return [...args, ...defaults];
}

export async function createSession(req: CreateSessionRequest): Promise<SessionInfo> {
  const id = randomUUID();
  const cmd = req.cmd ?? config.defaultShell;
  const args = withToolDefaultArgs(cmd, req.args ?? []);
  const cwd = req.cwd ?? config.defaultCwd;
  const cols = req.cols ?? config.defaultCols;
  const rows = req.rows ?? config.defaultRows;
  const name = normalizedOptionalString(req.name);
  const onIdle = normalizedOptionalString(req.onIdle);

  // Surface a useful error early when the user typed a cwd that doesn't
  // exist (deleted folder, moved project, typo). Without this guard,
  // launchd happily starts the runner, the runner's spawn() of bash/
  // claude/etc. fails because the cwd is invalid, the runner exits
  // before binding its socket, and the user sees a useless 15-second
  // "runner did not create socket" timeout — exactly how I just lost
  // 15 minutes hunting for the wrong bug.
  try {
    const st = fs.statSync(cwd);
    if (!st.isDirectory()) {
      throw new Error(`cwd is not a directory: ${cwd}`);
    }
  } catch (err) {
    const e = err as NodeJS.ErrnoException;
    if (e.code === 'ENOENT') {
      throw new Error(`cwd does not exist: ${cwd}`);
    }
    throw err;
  }

  fs.mkdirSync(STATE_DIR, { recursive: true, mode: 0o700 });
  const sockPath = path.join(STATE_DIR, id + '.sock');
  const logPath = path.join(STATE_DIR, id + '.log');

  // launchd starts the runner with a minimal env. We pass everything the
  // runner expects via EnvironmentVariables in the plist. Crucially we
  // include a sane PATH so the actual PTY command (e.g. `claude`) can be
  // found by node-pty's spawn — without this, brew-installed binaries
  // fall off the path under launchd.
  // launchd's minimal env also drops things sessions genuinely need that
  // the old in-process node-pty path inherited for free: the ssh-agent
  // socket (so `git push` over ssh works inside a session), proxy / CA
  // settings on corporate networks, and any Anthropic auth/base-url
  // overrides. Forward them when present; the explicit vars and req.env
  // below still win.
  const PASSTHROUGH_ENV = [
    'SSH_AUTH_SOCK', 'ANTHROPIC_API_KEY', 'ANTHROPIC_AUTH_TOKEN', 'ANTHROPIC_BASE_URL',
    'HTTP_PROXY', 'HTTPS_PROXY', 'NO_PROXY', 'ALL_PROXY',
    'http_proxy', 'https_proxy', 'no_proxy', 'all_proxy',
    'NODE_EXTRA_CA_CERTS', 'GIT_SSH_COMMAND'
  ];
  const passthrough: Record<string, string> = {};
  for (const k of PASSTHROUGH_ENV) {
    const v = process.env[k];
    if (typeof v === 'string' && v.length > 0) passthrough[k] = v;
  }

  // Build a PATH that guarantees both Homebrew install locations are present
  // so bare command names ('claude', 'codex', 'node') resolve under launchd's
  // minimal environment on both Apple Silicon (/opt/homebrew/bin) and Intel
  // (/usr/local/bin) Macs. If they're already in the inherited PATH, don't
  // duplicate them; just prepend the ones that are missing.
  const BREW_PATHS = ['/opt/homebrew/bin', '/usr/local/bin'];
  const basePath = process.env.PATH ?? '/usr/bin:/bin:/usr/sbin:/sbin';
  const pathSegments = basePath.split(':');
  const missingBrewPaths = BREW_PATHS.filter((p) => !pathSegments.includes(p));
  const launchdPath = missingBrewPaths.length > 0
    ? [...missingBrewPaths, ...pathSegments].join(':')
    : basePath;

  // Sanitize caller-supplied env: strip RUNNER_* (internal runner config that
  // must not be overridden) and known process-injection keys. All other vars
  // are passed through so callers can set e.g. a per-session ANTHROPIC_API_KEY
  // or custom HOME. They are spread before the locked RUNNER_* block below so
  // those keys always win.
  const CALLER_STRIP_RE = /^RUNNER_/i;
  const CALLER_STRIP_SET = new Set(['NODE_OPTIONS', 'DYLD_INSERT_LIBRARIES', 'DYLD_LIBRARY_PATH', 'LD_PRELOAD']);
  const safeCallerEnv: Record<string, string> = {};
  for (const [k, v] of Object.entries(req.env ?? {})) {
    if (CALLER_STRIP_RE.test(k) || CALLER_STRIP_SET.has(k)) continue;
    safeCallerEnv[k] = v;
  }

  const launchdEnv: Record<string, string> = {
    ...passthrough,
    HOME: process.env.HOME || os.homedir(),
    USER: process.env.USER || '',
    PATH: launchdPath,
    LANG: process.env.LANG || 'en_US.UTF-8',
    SHELL: process.env.SHELL || '/bin/bash',
    // Caller env after defaults so it can override HOME/PATH/LANG/SHELL,
    // but comes before the RUNNER_* block which must always win.
    ...safeCallerEnv,
    // These keys are always forced to the values prettyd controls. A caller
    // cannot override them even after the safeCallerEnv spread above because
    // they appear after it in this literal.
    TERM: 'xterm-256color',
    RUNNER_ID: id,
    RUNNER_STATE_DIR: STATE_DIR,
    RUNNER_CMD: cmd,
    RUNNER_ARGS_JSON: JSON.stringify(args),
    RUNNER_CWD: cwd,
    RUNNER_COLS: String(cols),
    RUNNER_ROWS: String(rows),
  };

  const programArguments = resolveRunnerProgramArguments();
  const ok = bootstrapRunner({
    id,
    programArguments,
    env: launchdEnv,
    cwd,
    logPath
  });
  if (!ok) {
    throw new Error(`launchctl bootstrap failed for ${id} — see ${logPath}`);
  }

  // launchd's bootstrap is synchronous in the load step but the runner
  // process initializes asynchronously. The cold-start budget includes
  // tsx-loader + node-pty + xterm-headless + the actual `claude`
  // binary's own startup. With a built dist/runner.js (preferred path
  // above) this lands in 2-3s; falling back to tsx adds another
  // 20-40s easily on a cold cache. 60s gives both paths headroom and
  // mirrors what `pretty new` accepts.
  const RUNNER_BOOT_MS = 60_000;
  const usingTsx = programArguments.some((p) => p.endsWith('/tsx'));
  const deadline = Date.now() + RUNNER_BOOT_MS;
  while (!fs.existsSync(sockPath)) {
    if (Date.now() > deadline) {
      // Don't immediately bootout — leave the plist behind so we can
      // inspect what launchd is doing. (Caller can launchctl print to
      // see the service state, view the runner log, etc.) The orphan-
      // cleanup pass on next prettyd start will reap it eventually.
      // Hint at the actual bottleneck so the user knows to rebuild.
      const hint = usingTsx
        ? ' Try `npm --prefix prettyd run build` so future sessions skip tsx cold-start.'
        : '';
      throw new Error(
        `runner did not create socket within ${RUNNER_BOOT_MS / 1000}s: ${sockPath} (see ${logPath}).${hint}`
      );
    }
    await new Promise((r) => setTimeout(r, 30));
  }

  const session = await registerRunner(sockPath);
  if (name !== undefined) session.info.name = name;
  if (onIdle !== undefined) session.info.onIdle = onIdle;
  if (req.waitReady === true) {
    await waitForSessionReady(session);
  }
  return session.info;
}

// Connect to an existing runner socket and register it as a session.
// Used both for newly-spawned runners (createSession) and for survivors
// discovered on prettyd startup.
async function registerRunner(sockPath: string): Promise<SessionInternal> {
  const client = new RunnerClient(sockPath);
  const hello = await client.connect();

  // Contract #3: log a protocol version mismatch but ALWAYS attach.
  // Runners built before protocolVersion was introduced send no field
  // (treat as v0). The 11 live runners are in this category; dropping
  // them on version skew would destroy real sessions.
  const runnerVersion = hello.protocolVersion ?? 0;
  if (runnerVersion !== RUNNER_PROTOCOL_VERSION) {
    console.log(
      `[protocol] runner ${hello.id} reports v${runnerVersion}, daemon expects v${RUNNER_PROTOCOL_VERSION}; attaching anyway`
    );
  }

  // Mirror the runner's event log locally so WS replay is in-memory.
  // Pull whatever the runner already has via REPLAY_REQ(0) before going
  // live. New OUTPUT frames append automatically.
  const log = new EventLog();
  const emitter = new EventEmitter();
  emitter.setMaxListeners(64);
  // Server-side xterm mirror (see SessionInternal). Sized to the PTY's
  // hello-reported cols/rows + 5000 rows scrollback (matches the runner
  // for parity). Built before client.on('output') is wired so the very
  // first replay event lands in both EventLog AND mirror.
  const mirrorTerm = new Terminal({
    cols: hello.cols,
    rows: hello.rows,
    scrollback: 5000,
    allowProposedApi: true
  });
  const mirrorSerialize = new SerializeAddon();
  mirrorTerm.loadAddon(mirrorSerialize);
  const tool = classifyTool(hello.cmd);
  const internal: SessionInternal = {
    info: {
      id: hello.id,
      cmd: hello.cmd,
      args: hello.args,
      cwd: hello.cwd,
      cols: hello.cols,
      rows: hello.rows,
      createdAt: hello.createdAt,
      pid: hello.pid,
      tool,
      ...spawnControls(tool, hello.args),
      working: false,
      lastDataAt: Date.now(),
      lastUserMessageAt: null,
      exited: false,
      exitCode: null,
      exitSignal: null,
      exitedAt: null
    },
    client,
    emitter,
    log,
    mirrorTerm,
    mirrorSerialize,
    exited: false,
    exitCode: null,
    exitSignal: null,
    exitSeq: null,
    recentBytes: 0,
    fileWatcher: null,
    codexLifecycleWorking: null,
    claudeEventLog: [],
    claudeEventBase: 0,
    pushWorkingObserved: false,
    workingStartedAt: null
  };
  sessions.set(hello.id, internal);

  // Spin up the JSONL watcher for Claude Code sessions. The cwd is
  // the only thing needed to locate the project dir; the watcher
  // finds the actual .jsonl by most-recent mtime. Best-effort — if
  // it fails (not a Claude session, no JSONL written yet, etc.) we
  // silently keep going. The existing parser-derived path is still
  // available as a fallback.
  if (internal.info.tool === 'claude-code' && hello.cwd) {
    // Extract the Claude session UUID we explicitly pinned at spawn
    // (`--session-id <uuid>` for fresh, `--resume <uuid>` for resume).
    // This is now the ONLY way the watcher locates the JSONL — no
    // mtime/birthtime fallback. If it's missing, the watcher gives
    // up cleanly and Pretty view stays empty (which is the correct
    // failure mode for a session we don't have an id for).
    const claudeSessionId = extractClaudeSessionId(hello.args);
    void watchSessionFile({
      cwd: hello.cwd,
      claudeSessionId
    })
      .then((watcher) => {
        if (internal.exited) {
          watcher.close();
          return;
        }
        internal.fileWatcher = watcher;
        watcher.emitter.on('event', (ev: ClaudeSessionEvent) => {
          recordStructuredSessionEvent(internal, ev);
        });
        watcher.emitter.on('error', () => { /* swallow — non-fatal */ });
      })
      .catch(() => { /* swallow — non-fatal */ });
  }

  // Keep this in registerRunner: both createSession() and discoverRunners()
  // enter through this shared path, so reattached Codex sessions get the
  // same rollout watcher as newly-created sessions.
  if (internal.info.tool === 'codex' && hello.cwd) {
    void watchCodexRollout({
      cwd: hello.cwd,
      args: hello.args,
      createdAt: hello.createdAt
    })
      .then((watcher) => {
        if (internal.exited) {
          watcher.close();
          return;
        }
        internal.fileWatcher = watcher;
        watcher.emitter.on('event', (ev: ClaudeSessionEvent) => {
          recordStructuredSessionEvent(internal, ev);
        });
        watcher.emitter.on('working', (working: boolean) => {
          internal.codexLifecycleWorking = working;
          setWorkingState(internal, working);
        });
        watcher.emitter.on('error', () => { /* swallow — non-fatal */ });
      })
      .catch(() => { /* swallow — non-fatal */ });
  }

  // Wire runner events.
  client.on('output', (ev) => {
    // Push to local mirror with the runner's seq so replays line up.
    log.pushAt(ev.seq, ev.data);
    // Feed the headless mirror so snapshot() can serialize w/ scrollback.
    try { mirrorTerm.write(ev.data); } catch { /* mirror write failed — non-fatal */ }
    internal.recentBytes += Buffer.byteLength(ev.data, 'utf8');
    internal.info.lastDataAt = Date.now();
    // NB: do NOT set info.working here. The 800ms interval owns it — for
    // claude-code it reads the "esc to interrupt" footer, not byte rate.
    // Setting it from byte rate on every frame would re-pin idle Claude
    // sessions to working:true between ticks (the statusline-repaint
    // false positive the footer check exists to kill).
    emitter.emit('output', { seq: ev.seq, data: ev.data, ts: internal.info.lastDataAt } satisfies OutputEvent);
  });
  client.on('exit', (e) => {
    internal.exited = true;
    internal.exitCode = e.code;
    internal.exitSignal = e.signal;
    internal.exitSeq = e.seq;
    internal.info.exited = true;
    internal.info.exitCode = e.code;
    internal.info.exitSignal = e.signal;
    internal.info.exitedAt = Date.now();
    internal.info.working = false;
    emitter.emit('exit', { code: e.code, signal: e.signal, seq: e.seq });
    // Take launchd off the plist so it doesn't auto-restart this
    // session on next reboot. Best effort — if launchctl errors we
    // proceed; cleanupOrphanPlists on next prettyd start will catch it.
    try { bootoutRunner(hello.id); } catch { /* best effort */ }
    // Keep the session in the map for EXITED_GRACE_MS so `pretty ls
    // --include-exited`, `pretty ls --json`, and the UI's tab strip can
    // show what happened. The default `pretty ls` and the frontend's
    // tab list still hide them so kill feels immediate.
    setTimeout(() => {
      sessions.delete(hello.id);
      try { mirrorTerm.dispose(); } catch { /* ignore */ }
      try { internal.fileWatcher?.close(); } catch { /* ignore */ }
    }, EXITED_GRACE_MS).unref();
    client.disconnect();
  });
  client.on('disconnect', () => {
    // Socket dropped without a clean EXIT (runner crashed, or we got
    // here via the exit handler above). If we already saw EXIT, the
    // grace-period timer above will clean up. If not, handle the crash.
    if (!internal.exited) {
      // Contract #4: emit 'runner-lost' so the WS layer can forward an
      // exit frame to browser clients. Without this, a runner crash or
      // KeepAlive-triggered restart leaves every open WS view frozen
      // indefinitely since no EXIT frame is ever sent.
      internal.emitter.emit('runner-lost');
      sessions.delete(hello.id);
      try { mirrorTerm.dispose(); } catch { /* ignore */ }
      // Close the JSONL watcher too — otherwise a crash-disconnect (no
      // EXIT frame) leaks its dir watcher, file watcher, and 2s poll
      // interval, and they keep appending to the orphaned event log.
      try { internal.fileWatcher?.close(); } catch { /* ignore */ }
      // Schedule bounded reconnect attempts in case launchd
      // KeepAlive(SuccessfulExit=false) respawns the runner after the
      // crash. Delays: 1s, 3s, 10s. Conservative and idempotent: each
      // attempt checks sessions.has(id) first to avoid a duplicate entry,
      // and never reaps on failure (sessions-are-sacred).
      scheduleRunnerReconnect(hello.id, sockPath, [1_000, 3_000, 10_000]);
    }
  });

  // Backfill from the runner's existing buffer. Don't wait forever: a
  // runner that dies (or whose socket drops) mid-replay must not hang
  // registerRunner — discoverRunners() awaits this before server.listen(),
  // so a single wedged socket would otherwise stop prettyd from ever
  // binding its port. Resolve on REPLAY_DONE, on disconnect, or after a
  // timeout; the disconnect handler above already cleans up a dead one.
  client.requestReplay(0);
  await new Promise<void>((resolve) => {
    const finish = (): void => {
      client.off('replayDone', finish);
      client.off('disconnect', finish);
      clearTimeout(timer);
      resolve();
    };
    const timer = setTimeout(finish, 10_000);
    client.on('replayDone', finish);
    client.on('disconnect', finish);
  });

  return internal;
}

// True while discoverRunners() is reattaching on startup. server.ts now
// listens BEFORE discovery so the daemon is reachable immediately; this
// lets /api/health report that sessions are still loading (the list can be
// partial until discovery finishes).
let discovering = false;
export function isDiscovering(): boolean { return discovering; }

// Scan the runners state directory and reconnect to any that are still
// alive. Called once on prettyd startup. Stale .json + .sock pairs (where
// connect fails) are unlinked, and orphan launchd plists (whose state
// files are gone) are booted out so they don't auto-start next reboot.
export async function discoverRunners(): Promise<void> {
  discovering = true;
  try {
    await discoverRunnersInner();
  } finally {
    discovering = false;
  }
}

async function discoverRunnersInner(): Promise<void> {
  // Boot out any launchd plists that point at runners with no state
  // files — those are leftovers from a runner that died unclean.
  cleanupOrphanPlists(STATE_DIR);
  if (!fs.existsSync(STATE_DIR)) return;
  let entries: string[];
  try { entries = fs.readdirSync(STATE_DIR); }
  catch { return; }
  const sockFiles = entries.filter((n) => n.endsWith('.sock'));
  for (const name of sockFiles) {
    const sockPath = path.join(STATE_DIR, name);
    const id = name.replace(/\.sock$/, '');
    const metaPath = path.join(STATE_DIR, id + '.json');
    // Try connecting up to 3 times with a small delay. The runner is
    // launchd-managed with KeepAlive(SuccessfulExit=false), so a dead
    // runner should be respawning RIGHT NOW. Give launchd a beat to
    // bring it back before we declare the session dead and bootout
    // its plist. Without this, every prettyd restart that races a
    // runner restart would nuke the plist and lose the session.
    let connected = false;
    for (let attempt = 0; attempt < 3; attempt++) {
      try {
        await registerRunner(sockPath);
        connected = true;
        break;
      } catch {
        if (attempt < 2) await new Promise((r) => setTimeout(r, 800));
      }
    }
    if (!connected) {
      // Connect failed 3×. That is NOT proof the session is dead: under a
      // rapid prettyd restart (tsx-watch saves back-to-back) with dozens of
      // runners reconnecting, a perfectly healthy runner can lose this race
      // — and reaping it here SIGTERMs a live session (this destroyed 4
      // real sessions on 2026-06-11). Only clean up when the session's
      // process is provably gone; otherwise leave every file in place and
      // let the next discovery pass pick it up.
      let alivePid: number | null = null;
      try {
        const meta = JSON.parse(fs.readFileSync(metaPath, 'utf8')) as { pid?: number };
        if (typeof meta.pid === 'number' && meta.pid > 0) {
          process.kill(meta.pid, 0); // throws ESRCH if the process is gone
          // pid is still alive. Best-effort guard against PID reuse after a
          // reboot: verify the process argv mentions runner.js or this session
          // id so we don't conserve a completely unrelated process that
          // happened to reuse the same pid. If ps fails or returns nothing
          // (which is unexpected given kill(0) succeeded), stay conservative
          // and do NOT reap — sessions-are-sacred.
          const ps = spawnSync('ps', ['-p', String(meta.pid), '-o', 'args='], {
            encoding: 'utf8', timeout: 2_000
          });
          const cmdline = (ps.stdout ?? '').trim();
          if (cmdline.length > 0 &&
              !cmdline.includes('runner.js') &&
              !cmdline.includes('runner.ts') &&
              !cmdline.includes(id)) {
            // Cmdline is readable but clearly unrelated — PID was reused by
            // a different process after a reboot. The session is gone.
            console.error(`[discover] runner ${id} pid ${meta.pid} is PID reuse (${cmdline.slice(0, 60)}) — treating as dead`);
          } else {
            // Either argv confirms it's our runner, or we couldn't determine
            // (empty cmdline / ps error) — stay conservative.
            alivePid = meta.pid;
          }
        }
      } catch { /* unreadable meta or dead pid → reap below */ }
      if (alivePid !== null) {
        console.error(`[discover] runner ${id} unreachable but pid ${alivePid} alive — leaving it alone`);
        continue;
      }
      // Genuinely dead after retries. Clean up so future starts don't
      // chase the same orphan.
      try { fs.unlinkSync(sockPath); } catch { /* ignore */ }
      try { fs.unlinkSync(metaPath); } catch { /* ignore */ }
      try { bootoutRunner(id); } catch { /* ignore */ }
    }
  }
}

export function listSessions(opts: { includeExited?: boolean } = {}): SessionInfo[] {
  const all = [...sessions.values()].map((s) => s.info);
  return opts.includeExited ? all : all.filter((s) => !s.exited);
}

// Daemon-side per-session facts for /api/health/deep + `pretty doctor`.
// (QoS/spawn-path facts are filesystem/process state, read CLI-side.)
export function deepSessionDiagnostics(): Array<Record<string, unknown>> {
  const now = Date.now();
  return [...sessions.values()].map((s) => ({
    id: s.info.id,
    tool: s.info.tool,
    cols: s.info.cols,
    rows: s.info.rows,
    pid: s.info.pid,
    working: s.info.working,
    exited: s.info.exited,
    claudeEvents: s.claudeEventBase + s.claudeEventLog.length,
    lastDataAgeMs: now - s.info.lastDataAt
  }));
}

export function getSession(id: string): SessionInternal | undefined {
  return sessions.get(id);
}

export function killSession(id: string): boolean {
  const s = sessions.get(id);
  if (!s) return false;
  s.client.kill();
  // Don't drop the session here — wait for the runner's EXIT frame.
  return true;
}

export function writeInput(id: string, data: string): boolean {
  const s = sessions.get(id);
  if (!s || s.exited) return false;
  s.client.send(data);
  return true;
}

export function resize(id: string, cols: number, rows: number): boolean {
  const s = sessions.get(id);
  if (!s || s.exited) return false;
  s.client.resize(cols, rows);
  // Keep the prettyd-side mirror in lockstep with the PTY. snapshot(),
  // the claude "working" footer detector, and the multi-choice picker
  // detector all read this mirror; if it stays at the create-time size
  // while the TUI redraws for a new width, cursor-positioning writes are
  // interpreted at the wrong column and the mirror paints garbage.
  try { s.mirrorTerm.resize(cols, rows); } catch { /* ignore */ }
  s.info.cols = cols;
  s.info.rows = rows;
  return true;
}

export interface SnapshotResult {
  text: string;
  // Server seq# the snapshot represents — clients use this to resume
  // the WS subscription with `?lastSeq=N` so they only receive deltas
  // and don't re-process every frame from scratch through xterm.
  seq: number;
}

export async function snapshot(id: string, opts?: { cols?: number }): Promise<SnapshotResult | null> {
  const s = sessions.get(id);
  if (!s) return null;
  // Serialize from the prettyd-side mirror, not the runner. The mirror
  // is fed the same OUTPUT stream and stays at its own scrollback (5000),
  // so the result includes recent history regardless of what serialize
  // options the runner subprocess happens to use. This is what lets a
  // long-lived runner started with old code (e.g. fit-furniture's, on
  // scrollback:0) still produce useful Pretty-view snapshots.
  const raw = s.mirrorSerialize.serialize({ scrollback: 1000 });
  // The seq this snapshot represents = the latest seq mirrored. Subtle:
  // we read it AFTER serialize() so the value definitely reflects the
  // bytes that just got serialized; reading before could under-count
  // if a frame landed during serialization.
  const seq = s.log.currentSeq();
  if (opts?.cols && opts.cols > 0) {
    const { reflowAnsi } = await import('./reflow.js');
    return { text: reflowAnsi(raw, { width: opts.cols }), seq };
  }
  return { text: raw, seq };
}
