export type SessionTool = 'claude-code' | 'codex' | 'terminal';

export interface SessionInfo {
  id: string;
  // Optional user-facing label set at create time. This is distinct
  // from Claude's own titles: it is a caller-supplied job name.
  name?: string;
  cmd: string;
  args: string[];
  cwd: string;
  cols: number;
  rows: number;
  createdAt: number;
  pid: number;
  // Tool classification derived from cmd. Lets the UI tab strip show
  // the right icon for inactive sessions without polling each one.
  tool: SessionTool;
  // Cheap activity-based "working" signal so inactive tabs in the UI
  // can show a pulse without us running a parser per session. True if
  // the PTY emitted >=THRESHOLD bytes within the last WINDOW_MS (see
  // sessions.ts). Tool-agnostic — accurate enough that a Claude turn,
  // a bash log spam, or a `cat large.txt` all read as "working".
  working: boolean;
  lastDataAt: number; // ms epoch
  // When the user last sent a real message in this session (ms epoch),
  // derived from the Claude JSONL stream — actual typed input only, not
  // tool_results or system-inserted pseudo-messages. null for non-Claude
  // sessions or before the first user message is seen. Lets `pretty ls`
  // (and the UI) surface staleness so old sessions can be culled.
  lastUserMessageAt: number | null;
  // Set when the PTY exits. Sessions stick around for 30s after EXIT
  // so the UI / `pretty ls --include-exited` can show what happened.
  exited: boolean;
  exitCode: number | null;
  exitSignal: string | null;
  exitedAt: number | null; // ms epoch when EXIT was received
  // Latest Claude-side titles pulled from the JSONL. /rename writes a
  // {type:"custom-title",customTitle:"..."} event; the auto-generated
  // summary writes {type:"ai-title",aiTitle:"..."}. Both undefined for
  // non-Claude sessions or before Claude has emitted them. Frontend
  // prefers customTitle > aiTitle > cwd-basename for the tab label,
  // and the user's manual rename overrides all of these.
  claudeCustomTitle?: string;
  claudeAiTitle?: string;
  // Optional shell command to fire on observed working true → false edges.
  onIdle?: string;
  // Spawn-time controls requested through the tool's native arguments.
  model?: string;
  effort?: string;
  fast?: boolean;
}

export function classifyTool(cmd: string): SessionTool {
  const c = cmd.toLowerCase();
  // Match either bare name (`claude`) or path-suffix (`/opt/homebrew/bin/claude`).
  if (c === 'claude' || c.endsWith('/claude')) return 'claude-code';
  if (c === 'codex' || c.endsWith('/codex')) return 'codex';
  return 'terminal';
}

export interface CreateSessionRequest {
  cmd?: string;
  args?: string[];
  cwd?: string;
  cols?: number;
  rows?: number;
  env?: Record<string, string>;
  name?: string;
  onIdle?: string;
  waitReady?: boolean;
}

// Wire-protocol version. Bump whenever message shapes change so a
// client can detect mismatch and refuse to render rather than silently
// rendering wrong data.
export const PROTOCOL_VERSION = 2;

// Client → server WS messages.
//
// Two connection modes share these:
//   • single-session (`/ws?sessionId=…`) — input/resize apply to the
//     URL's session; sessionId on the message is ignored. Used by the
//     CLI (`pretty attach` / `tail -f`) and older clients.
//   • multiplexed (`/ws?mux=1`) — ONE socket carries every session the
//     client attaches (tmux-style: N sessions, 1 connection). attach/
//     detach manage subscriptions; input/resize MUST carry sessionId.
export type ClientMsg =
  | { type: 'input'; data: string; sessionId?: string; requestId?: string }
  | { type: 'resize'; cols: number; rows: number; sessionId?: string }
  // outputReplay=false suppresses raw PTY bytes entirely for this attach
  // (no replay, no live output frames). Used by clients that only render
  // the structured claudeEvent stream (Pretty view): replaying a 4MB
  // output ring × dozens of sessions through one socket on page load is
  // hundreds of MB the client immediately discards — and input frames
  // queue behind it ("can't type"). Terminal views attach with output on
  // (default) and a snapshot-prefill lastSeq, so their replay is tiny.
  // claudeReplay=false suppresses the on-attach replay of Claude JSONL
  // *history*. A page with N mounted
  // sessions otherwise replays N×INITIAL_REPLAY_CAP events through the one
  // socket on load — 32 sessions × ~300 ≈ 20MB into the main thread at
  // once, which freezes the page and makes typing lag. Only the session
  // the user is actually viewing asks for history; the rest backfill when
  // activated.
  // claudeLive=false also suppresses live claudeEvent frames. Hidden
  // sessions use this because they discard those frames client-side and
  // backfill from an HTTP tail when activated.
  | { type: 'attach'; sessionId: string; lastSeq?: number; claudeEventsSince?: number; outputReplay?: boolean; claudeReplay?: boolean; claudeLive?: boolean }
  | { type: 'detach'; sessionId: string }
  | { type: 'snapshot'; requestId: string; sessionId: string; cols?: number }
  | { type: 'events'; requestId: string; sessionId: string; since?: number; tail?: number }
  // Application-level keepalive: the browser can't send WS protocol pings,
  // so the client sends {type:'ping'} and the daemon replies {type:'pong'}.
  // Lets the client detect a silently-dead TCP link and force a reconnect.
  | { type: 'ping' };

// Server → client WS messages.
// Phase 2: every output chunk carries a monotonic seq#. Clients persist
// the last seen seq and reconnect with ?lastSeq=N to resume.
// In mux mode every message carries `sessionId` so the client can route
// it to the right terminal; absent in single-session mode.
export type ServerMsg =
  | {
      type: 'hello';
      protocol: number;
      session: SessionInfo;
      currentSeq: number;
      resumedFromSeq: number | null;
      // Server-side claudeEventLog length at hello time. Clients use
      // this to compute incremental indices for ?claudeEventsSince= on
      // their next reconnect.
      claudeEventsCount: number;
      // Index of the first event the server is about to replay. On
      // fresh connects against a long session, the server caps the
      // initial replay to the tail; the client should treat its local
      // claudeEvents counter as starting at this value, not 0.
      claudeReplayStart: number;
      sessionId?: string;
    }
  | { type: 'output'; seq: number; data: string; sessionId?: string }
  | { type: 'gap'; oldestAvailableSeq: number; currentSeq: number; sessionId?: string }
  // reason='runner-lost' marks a synthetic exit the daemon emits when a
  // runner disconnects without a clean EXIT frame (crash), so clients
  // unfreeze instead of hanging on a dead session.
  | { type: 'exit'; code: number | null; signal: string | null; seq: number; sessionId?: string; reason?: string }
  | { type: 'error'; message: string; sessionId?: string }
  | { type: 'rpcError'; requestId: string; message: string; code?: string; sessionId?: string }
  | { type: 'snapshot'; requestId: string; text: string; seq: number; sessionId: string }
  | {
      type: 'events';
      requestId: string;
      events: Record<string, unknown>[];
      nextIndex: number;
      totalCount: number;
      sessionId: string;
    }
  | { type: 'inputAck'; requestId: string; ok: boolean; sessionId: string }
  | { type: 'pong' }
  // Claude Code's structured session events, sourced from
  // ~/.claude/projects/<encoded-cwd>/<id>.jsonl. Frontend consumers
  // (currently Remote view) opt in to these instead of parsing the
  // PTY byte stream. Schema is the Anthropic API persistence format
  // — kept as a passthrough record because event shapes vary by
  // type ('user', 'assistant', 'system', etc.) and are decoded on
  // the frontend side.
  | { type: 'claudeEvent'; event: Record<string, unknown>; sessionId?: string };
