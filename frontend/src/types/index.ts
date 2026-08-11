// Mirror of the legacy contract in runtime/testdata/node-runtime/src/types.ts.
// Kept duplicated for now to avoid
// bundling backend code into the browser; Phase 4 will move shared
// protocol types into a shared/ package once the daemon goes prod.

export const PROTOCOL_VERSION = 2;

export type SessionTool = 'claude-code' | 'codex' | 'terminal';

export interface SessionInfo {
  id: string;
  name?: string;
  description?: string;
  tags?: Record<string, string>;
  kind?: string;
  cmd: string;
  args: string[];
  cwd: string;
  // Named provider-login boundary. Empty means the provider's normal
  // default account; a value selects Sessions' private Claude/Codex home.
  profile?: string;
  configDir?: string;
  worktreePath?: string;
  branch?: string;
  base?: string;
  sourceRepo?: string;
  cols: number;
  rows: number;
  createdAt: number;
  pid: number;
  runnerProtocol?: number;
  runnerVersion?: string;
  tool: SessionTool;
  working: boolean;
  lastDataAt: number;
  // Latest provider-transcript user-role record. Provider-internal injections
  // may move this value, so it is not proof that a person returned.
  lastUserMessageAt: number | null;
  // Input-boundary timestamps recorded by Sessions. These distinguish a
  // person using Sessions from another session relaying work.
  lastHumanMessageAt?: number | null;
  lastAgentMessageAt?: number | null;
  idleReason?: 'never-started' | 'completed' | 'needs-input' | 'failed';
  idleDetail?: string;
  idleSince?: number | null;
  lastSummary?: string;
  exited: boolean;
  exitCode: number | null;
  exitSignal: string | null;
  exitReason?: string;
  exitedAt: number | null;
  // The daemon lost its connection to the runner but did not observe the
  // process exit. This is recoverable connectivity state, never an ending.
  unreachable?: boolean;
  unreachableReason?: string;
  unreachableSince?: number | null;
  // Claude-side session titles, surfaced from the JSONL by sessionsd.
  // claudeCustomTitle: set by Claude's /rename slash command.
  // claudeAiTitle: Claude's own auto-generated summary.
  // Used by the tab strip when the user hasn't manually renamed in
  // sessions itself (manual override always wins).
  claudeCustomTitle?: string;
  claudeAiTitle?: string;
  // Structured-provider controls resolved by the daemon at spawn time.
  // These are display truth for the current durable session; changing them
  // requires an explicit provider control path, never a browser-only toggle.
  model?: string;
  effort?: string;
  fast?: boolean;
  conversationId?: string;
  remoteEndpoint?: string;
  onIdle?: string;
  claudeSessionId?: string;
  continuedFromHistoryId?: string;
  continuedFromProvider?: 'claude' | 'codex';
  continuationMode?: 'native-import' | 'linked-search';
  importedMessageCount?: number;
  // Trusted daemon provenance. These values come from the append-only
  // creator ledger; the UI uses them for the manager/child tree but never
  // guesses missing parentage from cwd, timestamps, or titles.
  creatorKind?: string;
  creatorId?: string;
  parentSessionId?: string;
  // Whether a child was explicitly delegated by a person or created by an
  // agent session. Legacy children omit this and stay fully visible.
  delegationKind?: 'user' | 'agent';
  permissions?: 'constrained' | 'full';
  lifecycle?: 'task' | 'session';
  // User-controlled visual grouping. Undefined preserves trusted creator
  // lineage; an empty string deliberately promotes the session to a root.
  displayParentSessionId?: string;
  // Daemon-owned working-set organization. A live session remains running,
  // searchable, countable, and CLI-visible while set aside.
  setAsideAt?: number | null;
  // Daemon-owned workbench mark. A pinned session sorts first everywhere and
  // any future automatic cleanup policy must leave it alone. The daemon always
  // sends it, so undefined means an older daemon that never learned the field.
  pinned?: boolean;
  creatorAncestry?: string[];
  rootCreatorKind?: string;
  rootCreatorId?: string;
  provenanceStatus?: string;
  reopenedAs?: string;
  resumedFrom?: string;
  movedToEndpoint?: string;
  movedToSessionId?: string;
  movedFromEndpoint?: string;
  movedFromSessionId?: string;
  endedByKind?: string;
  endedById?: string;
  endedByName?: string;
  endedByClient?: string;
  endReason?: string;
  endOperationId?: string;
}

export interface CreateSessionRequest {
  cmd?: string;
  args?: string[];
  cwd?: string;
  cols?: number;
  rows?: number;
  env?: Record<string, string>;
  name?: string;
  description?: string;
  tags?: Record<string, string>;
  profile?: string;
  worktree?: boolean;
  base?: string;
  kind?: string;
  onIdle?: string;
  waitReady?: boolean;
  providerTerminal?: boolean;
  claude?: ClaudeSessionOptions;
  // Frontend-only transport hint. api/sessionsd.ts removes this from the
  // JSON body and sends it through the daemon's trusted creator header.
  creatorSessionId?: string;
  delegationKind?: 'user' | 'agent';
  permissions?: 'inherit' | 'constrained' | 'full';
  lifecycle?: 'task' | 'session';
}

export type ClaudeToggle = 'inherit' | 'on' | 'off';
export type ClaudePermissionMode = 'inherit' | 'manual' | 'acceptEdits' | 'auto' | 'plan' | 'dontAsk' | 'bypassPermissions';
export type ClaudeSomewhereMCP = 'inherit' | 'ensure';

export interface ClaudeSettings {
  remoteControl: ClaudeToggle;
  permissionMode: ClaudePermissionMode;
  model: string;
  effort: 'inherit' | 'low' | 'medium' | 'high' | 'xhigh' | 'max';
  chrome: ClaudeToggle;
  somewhereMcp: ClaudeSomewhereMCP;
  remoteControlNamePrefix: string;
}

export interface ClaudeSessionOptions {
  remoteControl?: ClaudeToggle;
  permissionMode?: ClaudePermissionMode;
  model?: string;
  effort?: ClaudeSettings['effort'];
  chrome?: ClaudeToggle;
  somewhereMcp?: ClaudeSomewhereMCP;
  remoteControlNamePrefix?: string;
}

export interface DirectoryCandidate {
  path: string;
  label: string;
  kind: 'home' | 'common' | 'project' | 'somewhere';
}

export type ServerMsg =
  | {
      type: 'hello';
      protocol: number;
      session: SessionInfo;
      currentSeq: number;
      resumedFromSeq: number | null;
      // Index into the server's claudeEventLog at hello time. Clients
      // increment locally for each live claudeEvent received and pass
      // the running total as ?claudeEventsSince= on reconnect to skip
      // events they already have.
      claudeEventsCount: number;
      // Index of the first event in the upcoming initial replay. Use
      // this (not 0) as the starting point for the local counter —
      // on long sessions the server caps initial replay to the tail.
      claudeReplayStart: number;
      // Present in mux mode: which session this message belongs to.
      sessionId?: string;
    }
  | { type: 'output'; seq: number; data: string; sessionId?: string }
  | { type: 'gap'; oldestAvailableSeq: number; currentSeq: number; sessionId?: string }
  | { type: 'exit'; code: number | null; signal: string | null; seq: number; sessionId?: string }
  | { type: 'unreachable'; reason: string; seq: number; sessionId?: string }
  | { type: 'error'; message: string; sessionId?: string }
  | { type: 'rpcError'; requestId: string; message: string; code?: string; sessionId?: string }
  | { type: 'snapshot'; requestId: string; text: string; seq: number; sessionId: string }
  | {
      type: 'events';
      requestId: string;
      events: ClaudeSessionEvent[];
      nextIndex: number;
      totalCount: number;
      sessionId: string;
    }
  | { type: 'inputAck'; requestId: string; ok: boolean; sessionId: string }
  | { type: 'submitAck'; requestId: string; ok: boolean; sessionId: string }
  // Claude Code's structured session events. Sourced server-side from
  // ~/.claude/projects/<encoded-cwd>/<id>.jsonl. RemoteView consumes
  // these instead of the parser-derived blocks — far more reliable
  // because the schema is the Anthropic API persistence format
  // (stable UUIDs, typed roles, structured content) rather than
  // regex-scraped TUI rendering.
  | { type: 'claudeEvent'; event: ClaudeSessionEvent; sessionId?: string };

// Client → server messages on the multiplexed socket (`/ws?mux=1`): one
// connection per window, with every attached session tagged by sessionId.
export type MuxClientMsg =
  // outputReplay=false suppresses raw PTY bytes (replay AND live) for
  // this attach — Sessions-only sessions don't consume them, and replaying
  // every session's 4MB ring through one socket on page load wedges the
  // browser for minutes.
  // claudeReplay=false suppresses the on-attach replay of Claude JSONL
  // history. Hidden sessions attach this way so page load doesn't replay
  // every session's conversation through the one socket at once (32
  // sessions × ~300 events ≈ 20MB → frozen page, laggy typing).
  // claudeLive=false suppresses live claudeEvent frames too; hidden views
  // backfill from HTTP tail pages when activated.
  | { type: 'attach'; sessionId: string; lastSeq?: number; claudeEventsSince?: number; outputReplay?: boolean; claudeReplay?: boolean; claudeLive?: boolean }
  | { type: 'detach'; sessionId: string }
  | { type: 'input'; data: string; sessionId: string; requestId?: string }
  | { type: 'submit'; data: string; sessionId: string; requestId: string }
  | { type: 'resize'; cols: number; rows: number; sessionId: string }
  | { type: 'snapshot'; requestId: string; sessionId: string; cols?: number }
  | { type: 'events'; requestId: string; sessionId: string; since?: number; tail?: number };

export interface StructuredPlanStep {
  step: string;
  status: string;
}

export interface StructuredThreadItem extends Record<string, unknown> {
  id?: string;
  type?: string;
  text?: string;
  phase?: string | null;
  status?: string;
}

export interface StructuredTokenUsageBreakdown {
  cachedInputTokens?: number;
  inputTokens?: number;
  outputTokens?: number;
  reasoningOutputTokens?: number;
  totalTokens?: number;
}

export interface StructuredTokenUsage {
  last?: StructuredTokenUsageBreakdown;
  total?: StructuredTokenUsageBreakdown;
  modelContextWindow?: number | null;
}

export interface MessageAuthor {
  kind: 'session';
  id: string;
  name: string;
  client: string;
}

// Canonical session event stream. Claude JSONL records and Codex app-server
// notifications share this transport; provider-specific additions stay
// optional so newer event types remain forward-compatible in older clients.
export interface StructuredSessionEvent {
  type: string;            // 'user' | 'assistant' | 'system' | …
  uuid?: string;
  parentUuid?: string | null;
  timestamp?: string;
  sessionId?: string;      // Claude's id, NOT sessionsd's
  message?: {
    role?: string;
    content?: unknown;     // string OR array of typed blocks
    model?: string;
    stop_reason?: string;
    usage?: unknown;
  };
  source?: string;
  subtype?: string;
  conversationId?: string;
  turnId?: string;
  itemId?: string;
  delta?: string;
  item?: StructuredThreadItem;
  usage?: StructuredTokenUsage;
  status?: string;
  error?: { message?: string } | null;
  explanation?: string | null;
  plan?: StructuredPlanStep[];
  author?: MessageAuthor;
  [key: string]: unknown;
}

// Wire fields retain their historical claudeEvent naming for protocol v2.
// Keep this alias until the next protocol-version bump; UI code should use
// StructuredSessionEvent.
export type ClaudeSessionEvent = StructuredSessionEvent;
