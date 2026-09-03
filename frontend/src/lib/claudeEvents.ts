// Turn Claude Code's persisted session events (sourced from the JSONL
// file at ~/.claude/projects/<encoded-cwd>/<id>.jsonl, streamed via
// WS) into the message shape RemoteView consumes.
//
// Event types Claude emits (from the live rail-me capture):
//   - 'user'                  user input OR Claude's tool_result loop feedback
//   - 'assistant'             Claude's reply: text, thinking, tool_use blocks
//   - 'system'                hook results, suggestions, errors
//   - 'attachment'            tool-registration metadata (MCP/skill announcements)
//   - 'file-history-snapshot' file-backup metadata for edits Claude makes
//   - 'last-prompt'           duplicate of the latest user input — skip
//   - 'permission-mode'       mode changes ('bypassPermissions' etc.)
//   - 'queue-operation'       user input typed while Claude was busy → queued
//
// Content blocks (inside `assistant`):
//   - 'text'      chat-visible
//   - 'thinking'  internal reasoning (encrypted in recent Claude) — preserve
//                 presence metadata but don't display raw text
//   - 'tool_use'  Claude called a tool → surface as a chip with name + input
//
// Content blocks (inside `user`):
//   - string content → plain user message
//   - 'text'         user typed (sometimes "[Request interrupted by user]")
//   - 'tool_result'  Claude's loop feedback (skip — not user content)
//   - 'image'        user pasted/attached an image

import type { ClaudeSessionEvent, DispatchMessage, ToolCall } from '../types';
import { previewToolInput } from './toolPreview';

interface AnthropicContentBlock {
  type: string;
  text?: string;
  thinking?: string;
  // tool_use fields
  id?: string;
  name?: string;
  input?: Record<string, unknown>;
  // tool_result fields
  tool_use_id?: string;
  content?: unknown;
}

function isContentBlockArray(x: unknown): x is AnthropicContentBlock[] {
  return Array.isArray(x);
}

// Extract everything we care about from an assistant message's content
// array: visible text, tool calls, and whether thinking was present.
interface AssistantBreakdown {
  text: string;
  toolCalls: ToolCall[];
  hadThinking: boolean;
}
function breakdownAssistant(content: unknown): AssistantBreakdown {
  const result: AssistantBreakdown = { text: '', toolCalls: [], hadThinking: false };
  if (typeof content === 'string') {
    result.text = content;
    return result;
  }
  if (!isContentBlockArray(content)) return result;
  const textParts: string[] = [];
  for (const block of content) {
    if (block.type === 'text' && typeof block.text === 'string') {
      textParts.push(block.text);
    } else if (block.type === 'thinking') {
      result.hadThinking = true;
    } else if (block.type === 'tool_use' && block.id && block.name) {
      // inputFull is the prettyprinted JSON for the expanded view; the
      // preview is the one-line summary on the chip.
      let inputFull: string | undefined;
      if (block.input && typeof block.input === 'object') {
        try { inputFull = JSON.stringify(block.input, null, 2); } catch { /* skip */ }
      }
      result.toolCalls.push({
        id: block.id,
        name: block.name,
        inputPreview: previewToolInput(block.name, block.input),
        inputFull
      });
    }
  }
  result.text = textParts.join('\n\n').trim();
  return result;
}

// True if a user message's content is just tool_result blocks (not
// real user typing). Those events live in the JSONL because the API
// stream includes them, but they shouldn't render as user chat.
function isUserToolResultOnly(content: unknown): boolean {
  if (typeof content === 'string') return false;
  if (!isContentBlockArray(content)) return false;
  if (content.length === 0) return false;
  return content.every((b) => b.type === 'tool_result');
}

// Claude wraps a bunch of system-inserted "user" events around its
// own control flow:
//   <command-name>/compact</command-name>
//   <command-message>...</command-message>
//   <local-command-stdout>...</local-command-stdout>
//   <local-command-caveat>Caveat: The messages below were generated…
//   This session is being continued from a previous conversation that ran out of context…
//   <system-reminder>…</system-reminder>
//
// They're not user typing — they're system plumbing for /compact,
// --continue, --resume, etc. Rendering them as chat bubbles makes
// real user messages look lost in a wall of XML. Filter them out.
function isSystemUserPseudoMessage(text: string): boolean {
  const t = text.trimStart();
  if (t.startsWith('<command-name>')) return true;
  if (t.startsWith('<command-message>')) return true;
  if (t.startsWith('<command-args>')) return true;
  if (t.startsWith('<command-stdout>')) return true;
  if (t.startsWith('<local-command-stdout>')) return true;
  if (t.startsWith('<local-command-caveat>')) return true;
  if (t.startsWith('<system-reminder>')) return true;
  if (t.startsWith('This session is being continued from a previous conversation')) return true;
  if (t.startsWith('Caveat: The messages below were generated by the user while')) return true;
  return false;
}

// Extract user-typed text from a user message. Content can be a plain
// string (the typical case) OR an array containing text + image blocks.
function extractUserContent(content: unknown): { text: string; hasImage: boolean } {
  if (typeof content === 'string') return { text: content, hasImage: false };
  if (!isContentBlockArray(content)) return { text: '', hasImage: false };
  const parts: string[] = [];
  let hasImage = false;
  for (const block of content) {
    if (block.type === 'text' && typeof block.text === 'string') parts.push(block.text);
    else if (block.type === 'image') hasImage = true;
  }
  return { text: parts.join('\n').trim(), hasImage };
}

function timestampMs(ts: unknown): number {
  if (typeof ts !== 'string') return Date.now();
  const n = Date.parse(ts);
  return Number.isFinite(n) ? n : Date.now();
}

// Extract tool_result content from a user event whose content is an
// array of tool_result blocks. Returns a map of tool_use_id → result
// string. Claude's tool_result content can be a string OR an array of
// content blocks (typically a text block); we flatten to a single
// string for display.
function indexToolResultsFromUser(ev: ClaudeSessionEvent): Map<string, string> {
  const map = new Map<string, string>();
  const content = ev.message?.content;
  if (!isContentBlockArray(content)) return map;
  for (const block of content) {
    if (block.type !== 'tool_result') continue;
    const id = block.tool_use_id;
    if (typeof id !== 'string') continue;
    let result = '';
    if (typeof block.content === 'string') {
      result = block.content;
    } else if (Array.isArray(block.content)) {
      const parts: string[] = [];
      for (const c of block.content as Array<Record<string, unknown>>) {
        if (c && c.type === 'text' && typeof c.text === 'string') parts.push(c.text);
      }
      result = parts.join('\n');
    }
    map.set(id, result);
  }
  return map;
}

const RESULT_PREVIEW_LEN = 120;

type UnknownRecord = Record<string, unknown>;

function asRecord(value: unknown): UnknownRecord | null {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
    ? value as UnknownRecord
    : null;
}

function recordString(record: UnknownRecord | null, key: string): string {
  const value = record?.[key];
  return typeof value === 'string' ? value : '';
}

function recordNumber(record: UnknownRecord | null, key: string): number | undefined {
  const value = record?.[key];
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined;
}

function compactText(value: string, max = RESULT_PREVIEW_LEN): string {
  const compact = value.replace(/\s+/g, ' ').trim();
  return compact.length > max ? `${compact.slice(0, max - 1)}…` : compact;
}

function prettyUnknown(value: unknown): string {
  if (typeof value === 'string') return value;
  if (value == null) return '';
  try { return JSON.stringify(value, null, 2); } catch { return String(value); }
}

function humanItemType(type: string): string {
  if (!type) return 'Activity';
  const spaced = type.replace(/([a-z0-9])([A-Z])/g, '$1 $2').replace(/[-_]+/g, ' ');
  return spaced.charAt(0).toUpperCase() + spaced.slice(1);
}

function codexToolCall(item: UnknownRecord, previous?: ToolCall): ToolCall | null {
  const id = recordString(item, 'id');
  const kind = recordString(item, 'type');
  if (!id || !kind || kind === 'agentMessage' || kind === 'reasoning' || kind === 'userMessage') {
    return null;
  }

  const status = recordString(item, 'status') || previous?.status;
  const durationMs = recordNumber(item, 'durationMs') ?? previous?.durationMs;
  let name = humanItemType(kind);
  let inputPreview = '';
  let inputFull = '';
  let resultFull = '';

  if (kind === 'commandExecution') {
    name = 'Command';
    const command = recordString(item, 'command');
    const cwd = recordString(item, 'cwd');
    inputPreview = compactText(command);
    inputFull = [cwd ? `cwd: ${cwd}` : '', command ? `$ ${command}` : ''].filter(Boolean).join('\n');
    const output = recordString(item, 'aggregatedOutput');
    const exitCode = recordNumber(item, 'exitCode');
    resultFull = [
      output,
      exitCode != null ? `exit code: ${exitCode}` : '',
      durationMs != null ? `duration: ${durationMs} ms` : ''
    ].filter(Boolean).join(output ? '\n\n' : '\n');
  } else if (kind === 'fileChange') {
    name = 'File changes';
    const changes = Array.isArray(item['changes']) ? item['changes'] : [];
    const rows = changes.map((raw) => asRecord(raw)).filter((row): row is UnknownRecord => row !== null);
    inputPreview = rows.slice(0, 3).map((row) => {
      const changeKind = recordString(row, 'kind');
      const path = recordString(row, 'path');
      return [changeKind, path].filter(Boolean).join(' ');
    }).filter(Boolean).join(' · ');
    if (rows.length > 3) inputPreview += ` · +${rows.length - 3} more`;
    inputFull = rows.map((row) => [recordString(row, 'kind'), recordString(row, 'path')].filter(Boolean).join(' ')).join('\n');
    resultFull = rows.map((row) => {
      const header = [recordString(row, 'kind'), recordString(row, 'path')].filter(Boolean).join(' ');
      return [header, recordString(row, 'diff')].filter(Boolean).join('\n');
    }).join('\n\n');
  } else if (kind === 'mcpToolCall') {
    const server = recordString(item, 'server');
    const tool = recordString(item, 'tool');
    name = [server, tool].filter(Boolean).join(' · ') || 'MCP tool';
    inputFull = prettyUnknown(item['arguments']);
    inputPreview = compactText(inputFull);
    resultFull = prettyUnknown(item['result'] ?? item['error']);
  } else if (kind === 'dynamicToolCall') {
    const namespace = recordString(item, 'namespace');
    const tool = recordString(item, 'tool');
    name = [namespace, tool].filter(Boolean).join(' · ') || 'Tool';
    inputFull = prettyUnknown(item['arguments']);
    inputPreview = compactText(inputFull);
    resultFull = prettyUnknown(item['contentItems'] ?? item['success']);
  } else if (kind === 'collabAgentToolCall') {
    name = humanItemType(recordString(item, 'tool') || 'Agent task');
    inputFull = recordString(item, 'prompt');
    inputPreview = compactText(inputFull || prettyUnknown(item['receiverThreadIds']));
    resultFull = prettyUnknown(item['agentsStates']);
  } else if (kind === 'webSearch') {
    name = 'Web search';
    inputPreview = recordString(item, 'query');
    inputFull = prettyUnknown(item['action'] ?? item['query']);
  } else if (kind === 'imageView') {
    name = 'Viewed image';
    inputPreview = recordString(item, 'path');
    inputFull = inputPreview;
  } else if (kind === 'imageGeneration') {
    name = 'Generated image';
    inputPreview = recordString(item, 'savedPath') || recordString(item, 'status');
    resultFull = recordString(item, 'result');
  } else if (kind === 'plan') {
    name = 'Plan';
    inputFull = recordString(item, 'text');
    inputPreview = compactText(inputFull);
  } else {
    const likelyInput = item['arguments'] ?? item['input'] ?? item['query'] ?? item['path'] ?? item['prompt'];
    const likelyResult = item['result'] ?? item['output'] ?? item['error'];
    inputFull = prettyUnknown(likelyInput);
    inputPreview = compactText(inputFull);
    resultFull = prettyUnknown(likelyResult);
  }

  const resultPreview = resultFull ? compactText(resultFull) : (status ? humanItemType(status) : '');
  return {
    id,
    name,
    inputPreview: inputPreview || previous?.inputPreview || '',
    inputFull: inputFull || previous?.inputFull,
    resultPreview: resultPreview || previous?.resultPreview,
    resultFull: resultFull || previous?.resultFull,
    kind,
    status,
    durationMs
  };
}

interface CodexTurnProjection {
  message: DispatchMessage;
  itemText: Map<string, string>;
  itemPhase: Map<string, string>;
  itemOrder: string[];
  tools: Map<string, ToolCall>;
  reasoning: string[];
  completed: boolean;
}

function isCodexAppServerHistory(events: ClaudeSessionEvent[]): boolean {
  return events.some((event) => event.source === 'codex-app-server' || event.type === 'codex' || event.provider === 'codex');
}

function providerSystemMessage(event: ClaudeSessionEvent, index: number): DispatchMessage | null {
  if (event.type !== 'system') return null;
  const at = timestampMs(event.timestamp);
  if (event.subtype === 'provider_fault') {
    const detail = typeof event.detail === 'string' ? event.detail.trim() : '';
    if (!detail) return null;
    return {
      id: event.uuid ?? `provider-fault-${index}`,
      role: 'assistant',
      content: '',
      status: 'sent',
      createdAt: at,
      errorResponse: detail
    };
  }
  if (event.subtype !== 'provider_retry') return null;
  const attempt = typeof event.attempt === 'number' ? event.attempt : 0;
  const max = typeof event.max === 'number' ? event.max : 0;
  return {
    id: event.uuid ?? `provider-retry-${index}`,
    role: 'assistant',
    content: '',
    status: 'sent',
    createdAt: at,
    quietStatus: attempt > 0 && max > 0 ? `Retrying (${attempt} of ${max}) …` : 'Retrying …'
  };
}

function appendProviderSystemMessage(event: ClaudeSessionEvent, out: DispatchMessage[]): boolean {
  const message = providerSystemMessage(event, out.length);
  if (!message) return false;
  out.push(message);
  return true;
}

function codexEventsToMessages(events: ClaudeSessionEvent[]): DispatchMessage[] {
  const out: DispatchMessage[] = [];
  const turns = new Map<string, CodexTurnProjection>();
  const steeringByTurn = new Map<string, DispatchMessage[]>();
  let latestTurnID = '';
  const ensureTurn = (turnID: string, at: number): CodexTurnProjection | null => {
    if (!turnID) return null;
    const existing = turns.get(turnID);
    if (existing) return existing;
    const projection: CodexTurnProjection = {
      message: {
        id: `codex-turn-${turnID}`,
        role: 'assistant',
        content: '',
        status: 'sent',
        createdAt: at,
        blockId: turnID,
        streaming: true,
        turnStatus: 'inProgress'
      },
      itemText: new Map(),
      itemPhase: new Map(),
      itemOrder: [],
      tools: new Map(),
      reasoning: [],
      completed: false
    };
    turns.set(turnID, projection);
    out.push(projection.message);
    return projection;
  };

  const refreshTurn = (projection: CodexTurnProjection): void => {
    let finalText = '';
    const updates: string[] = [];
    for (const itemID of projection.itemOrder) {
      const text = projection.itemText.get(itemID)?.trim() ?? '';
      if (!text) continue;
      if (projection.itemPhase.get(itemID) === 'commentary') updates.push(text);
      else finalText = text;
    }
    projection.message.content = finalText;
    projection.message.updates = updates.length > 0 ? updates : undefined;
    projection.message.toolCalls = projection.tools.size > 0
      ? Array.from(projection.tools.values())
      : undefined;
    projection.message.hadThinking = projection.reasoning.length > 0 || undefined;
    projection.message.reasoningSummary = projection.reasoning.length > 0
      ? projection.reasoning.join('\n\n')
      : undefined;
    projection.message.streaming = !projection.completed;
  };

  const rememberItemText = (projection: CodexTurnProjection, itemID: string, text: string): void => {
    if (!itemID) return;
    if (!projection.itemOrder.includes(itemID)) projection.itemOrder.push(itemID);
    projection.itemText.set(itemID, text);
  };

  for (const event of events) {
    const at = timestampMs(event.timestamp);
    const subtype = event.subtype ?? '';
    if (appendProviderSystemMessage(event, out)) continue;
    if (event.source === 'sessions-continuation' && (event.type === 'user' || event.type === 'assistant')) {
      if (event.type === 'user' && event.message?.role === 'user') {
        const { text, hasImage } = extractUserContent(event.message.content);
        const content = text || (hasImage ? '[image attached]' : '');
        if (content) {
          out.push({
            id: event.uuid ?? `codex-import-user-${out.length}`,
            role: 'user',
            content,
            status: 'sent',
            createdAt: at,
            confirmedAt: at,
            blockId: event.uuid
          });
        }
      } else if (event.message?.role === 'assistant') {
        const imported = breakdownAssistant(event.message.content);
        if (imported.text) {
          out.push({
            id: event.uuid ?? `codex-import-assistant-${out.length}`,
            role: 'assistant',
            content: imported.text,
            status: 'sent',
            createdAt: at,
            confirmedAt: at,
            blockId: event.uuid
          });
        }
      }
      continue;
    }
    if (event.type === 'user' && event.message?.role === 'user') {
      const { text, hasImage } = extractUserContent(event.message.content);
      const content = text || (hasImage ? '[image attached]' : '');
      if (!content || isSystemUserPseudoMessage(content)) continue;
      const message: DispatchMessage = {
        id: event.uuid ?? `codex-user-${out.length}`,
        role: 'user',
        content,
        status: 'sent',
        createdAt: at,
        confirmedAt: at,
        blockId: event.uuid,
        author: event.author,
        queued: subtype === 'user_steer' || undefined
      };
      out.push(message);
      if (message.queued && event.turnId) {
        const queued = steeringByTurn.get(event.turnId) ?? [];
        queued.push(message);
        steeringByTurn.set(event.turnId, queued);
      }
      continue;
    }

    if (subtype === 'turn_started') {
      if (event.turnId) latestTurnID = event.turnId;
      ensureTurn(event.turnId ?? '', at);
      continue;
    }

    // A message the user typed while Codex was mid-turn. The runner refuses
    // it rather than queuing it, so the only honest thing the UI can do is
    // show that it was not sent. Handled here, ahead of the turn projection
    // below, because a rejection carries no turnId: routed through
    // ensureTurn it would either be folded into whichever turn happened to be
    // latest or dropped outright when none was. Mirrors the Claude branch in
    // eventsToMessages so a rejection reads the same for both providers.
    if (event.type === 'system' && subtype === 'input_rejected') {
      const rejection = (event as Record<string, unknown>).error;
      if (typeof rejection !== 'string' || !rejection.trim()) continue;
      const rejectedInput = (event as Record<string, unknown>).input;
      if (typeof rejectedInput === 'string' && rejectedInput.trim()) {
        out.push({
          id: event.uuid ?? `codex-input-rejected-${out.length}`,
          role: 'user',
          content: rejectedInput,
          status: 'failed',
          createdAt: at,
          failureReason: rejection
        });
        continue;
      }
      out.push({
        id: event.uuid ?? `codex-input-rejected-${out.length}`,
        role: 'assistant',
        content: '',
        status: 'sent',
        createdAt: at,
        errorResponse: rejection
      });
      continue;
    }

    if (event.turnId) latestTurnID = event.turnId;
    const projection = ensureTurn(event.turnId || latestTurnID, at);
    if (!projection) continue;

    if (subtype === 'agent_message_delta') {
      const itemID = event.itemId ?? '';
      const next = (projection.itemText.get(itemID) ?? '') + (event.delta ?? '');
      rememberItemText(projection, itemID, next);
      refreshTurn(projection);
      continue;
    }

    if (subtype === 'item_started' || subtype === 'item_completed') {
      const item = asRecord(event.item);
      if (!item) continue;
      const itemID = recordString(item, 'id');
      const itemType = recordString(item, 'type');
      if (itemType === 'agentMessage') {
        const text = recordString(item, 'text');
        if (text || !projection.itemText.has(itemID)) rememberItemText(projection, itemID, text);
        const phase = recordString(item, 'phase');
        if (phase) projection.itemPhase.set(itemID, phase);
      } else if (itemType === 'reasoning') {
        const summaries = Array.isArray(item['summary'])
          ? item['summary'].filter((value): value is string => typeof value === 'string' && value.trim().length > 0)
          : [];
        projection.reasoning = summaries;
      } else {
        const call = codexToolCall(item, projection.tools.get(itemID));
        if (call) projection.tools.set(call.id, call);
      }
      refreshTurn(projection);
      continue;
    }

    if (subtype === 'plan_updated') {
      projection.message.plan = Array.isArray(event.plan)
        ? event.plan.filter((step) => typeof step?.step === 'string' && typeof step?.status === 'string')
        : undefined;
      projection.message.planExplanation = typeof event.explanation === 'string'
        ? event.explanation
        : undefined;
      refreshTurn(projection);
      continue;
    }

    if (subtype === 'turn_completed') {
      projection.completed = true;
      projection.message.turnStatus = event.status || 'completed';
      projection.message.streaming = false;
      const error = event.error?.message;
      if (error) projection.message.errorResponse = error;
      refreshTurn(projection);
      for (const message of steeringByTurn.get(event.turnId ?? '') ?? []) {
        message.queued = false;
      }
    }
  }

  return out.filter((message) => {
    if (message.role === 'user') return true;
    return !!(
      message.content || message.toolCalls?.length || message.updates?.length ||
      message.reasoningSummary || message.plan?.length || message.streaming || message.errorResponse || message.quietStatus
    );
  });
}

// Public entry. Walks the event stream in order and produces the
// RemoteView message list. Handles every Claude JSONL event type we've
// seen in the wild; unknown types pass through silently (forward-
// compatible with new Claude releases).
//
// Key shape decision: multi-step assistant turns (Claude writes prose,
// calls tools, then writes more prose) arrive as a sequence of assistant
// events. Consecutive tool-only events become one activity entry at their
// exact position in the timeline. They must not be attached to the next
// text event: doing so visibly shifts every command group one message late.
// A trailing group remains visible while Claude is mid-turn. Its stable id
// comes from the first tool event so React does not re-key it on every tick.
export function eventsToMessages(events: ClaudeSessionEvent[]): DispatchMessage[] {
  if (isCodexAppServerHistory(events)) return codexEventsToMessages(events);
  // First pass: index every tool_result by tool_use_id. They live on
  // user events but logically belong to the assistant's tool_use that
  // requested them.
  const toolResults = new Map<string, string>();
  for (const ev of events) {
    if (ev.type === 'user' && ev.message?.role === 'user' && isUserToolResultOnly(ev.message?.content)) {
      for (const [id, result] of indexToolResultsFromUser(ev)) toolResults.set(id, result);
    }
  }
  const out: DispatchMessage[] = [];
  // Track pending tool calls accumulated across consecutive tool-only
  // assistant events. Drains into the next text-bearing assistant
  // event OR a synthetic "in progress" message at end-of-stream.
  let pendingTools: ToolCall[] = [];
  let pendingHadThinking = false;
  // Track the earliest timestamp/uuid of the pending batch so the ordered
  // activity entry keeps the time at which the commands actually started.
  let pendingFirstUuid: string | undefined;
  let pendingFirstCreatedAt: number | undefined;
  // queue-operation dedup (Claude sometimes enqueues the same text
  // twice; we drop replicas).
  const queuedContents = new Set<string>();

  const enrichToolCalls = (
    calls: ToolCall[]
  ): ToolCall[] => {
    return calls.map((t) => {
      const result = toolResults.get(t.id);
      if (!result) return t;
      const trimmed = result.length > RESULT_PREVIEW_LEN
        ? result.slice(0, RESULT_PREVIEW_LEN - 1) + '…'
        : result;
      return { ...t, resultPreview: trimmed.replace(/\s+/g, ' ').trim(), resultFull: result };
    });
  };

  const flushPendingTools = (): void => {
    if (pendingTools.length === 0) return;
    const collected = enrichToolCalls(pendingTools);
    out.push({
      id: `${pendingFirstUuid ?? `asst-tools-${out.length}`}:tools`,
      role: 'assistant',
      content: '',
      status: 'sent',
      createdAt: pendingFirstCreatedAt ?? Date.now(),
      blockId: pendingFirstUuid,
      toolCalls: collected.length > 0 ? collected : undefined,
      hadThinking: pendingHadThinking || undefined
    });
    pendingTools = [];
    pendingHadThinking = false;
    pendingFirstUuid = undefined;
    pendingFirstCreatedAt = undefined;
  };

  for (const ev of events) {
    if (providerSystemMessage(ev, out.length)) { flushPendingTools(); pendingHadThinking = false; appendProviderSystemMessage(ev, out); continue; }
    if (ev.type === 'user' && ev.message?.role === 'user') {
      const content = ev.message?.content;
      if (isUserToolResultOnly(content)) continue;
      const { text, hasImage } = extractUserContent(content);
      let body = text;
      if (!body && hasImage) body = '[image attached]';
      else if (!body) continue;
      // System-inserted control flow (compact, continue, system-reminders).
      // These are user-role events Claude writes for its own bookkeeping,
      // not human typing — skip them in the chat.
      if (isSystemUserPseudoMessage(body)) continue;

      flushPendingTools();
      pendingHadThinking = false;
      const interrupted = body === '[Request interrupted by user]';
      queuedContents.delete(body.trim());

      out.push({
        id: ev.uuid ?? `user-evt-${out.length}`,
        role: 'user',
        content: body,
        status: 'sent',
        createdAt: timestampMs(ev.timestamp),
        confirmedAt: timestampMs(ev.timestamp),
        blockId: ev.uuid,
        interrupted: interrupted || undefined,
        author: ev.author
      });
      continue;
    }

    if (ev.type === 'assistant' && ev.message?.role === 'assistant') {
      const breakdown = breakdownAssistant(ev.message?.content);
      // Tool-only assistant event (Claude called a tool, no text reply
      // yet) — buffer the tool calls and continue. The next
      // visible timeline boundary will flush them in place.
      if (!breakdown.text) {
        if (breakdown.toolCalls.length > 0) {
          const startsGroup = pendingTools.length === 0;
          pendingTools.push(...breakdown.toolCalls);
          if (startsGroup) {
            pendingFirstUuid = ev.uuid;
            pendingFirstCreatedAt = timestampMs(ev.timestamp);
          }
        }
        if (breakdown.hadThinking) pendingHadThinking = true;
        continue;
      }
      // The pending commands happened before this prose. Emit them first
      // instead of visually shifting them down onto this message.
      flushPendingTools();
      const collected = enrichToolCalls(breakdown.toolCalls);
      const hadThinkingAny = pendingHadThinking || breakdown.hadThinking;
      pendingHadThinking = false;
      pendingFirstUuid = undefined;
      pendingFirstCreatedAt = undefined;
      out.push({
        id: ev.uuid ?? `asst-evt-${out.length}`,
        role: 'assistant',
        content: breakdown.text,
        status: 'sent',
        createdAt: timestampMs(ev.timestamp),
        blockId: ev.uuid,
        toolCalls: collected.length > 0 ? collected : undefined,
        hadThinking: hadThinkingAny || undefined
      });
      continue;
    }

    if (ev.type === 'queue-operation') {
      const op = (ev as Record<string, unknown>).operation;
      const text = (ev as Record<string, unknown>).content;
      if (op !== 'enqueue' || typeof text !== 'string' || !text.trim()) continue;
      const trimmed = text.trim();
      if (queuedContents.has(trimmed)) continue;
      flushPendingTools();
      pendingHadThinking = false;
      queuedContents.add(trimmed);
      out.push({
        id: `queue-${out.length}-${trimmed.slice(-12)}`,
        role: 'user',
        content: text,
        status: 'sent',
        createdAt: timestampMs(ev.timestamp),
        queued: true
      });
      continue;
    }
    if (ev.type === 'system' && ev.subtype === 'input_rejected') {
      const message = (ev as Record<string, unknown>).error;
      if (typeof message !== 'string' || !message.trim()) continue;
      flushPendingTools();
      pendingHadThinking = false;
      out.push({
        id: ev.uuid ?? `input-rejected-${out.length}`,
        role: 'assistant',
        content: '',
        status: 'sent',
        createdAt: timestampMs(ev.timestamp),
        errorResponse: message
      });
      continue;
    }
    // permission-mode / system / attachment / file-history-snapshot /
    // last-prompt are bookkeeping; skipped.
  }

  // Drain a still-running final command group without changing its timestamp.
  flushPendingTools();

  // Final pass: drop queued entries already superseded by the real
  // user_input event further down the list.
  return out.filter((m, i) => {
    if (!m.queued) return true;
    for (let j = i + 1; j < out.length; j++) {
      const other = out[j];
      if (other && other.role === 'user' && !other.queued && other.content.trim() === m.content.trim()) {
        return false;
      }
    }
    return true;
  });
}
