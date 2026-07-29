import type { ClaudeSessionEvent } from './sessionFileWatcher.js';

export interface CodexNormalizeContext {
  rolloutBasename: string;
  lineIndex: number;
}

export interface CodexNormalization {
  events: ClaudeSessionEvent[];
  working: boolean | null;
}

interface ClaudeUsage {
  input_tokens?: number;
  output_tokens?: number;
  cache_creation_input_tokens?: number;
  cache_read_input_tokens?: number;
}

function isRecord(x: unknown): x is Record<string, unknown> {
  return typeof x === 'object' && x !== null && !Array.isArray(x);
}

function stringField(r: Record<string, unknown>, key: string): string | null {
  const v = r[key];
  return typeof v === 'string' ? v : null;
}

function numberField(r: Record<string, unknown>, keys: string[]): number | undefined {
  for (const key of keys) {
    const v = r[key];
    if (typeof v === 'number' && Number.isFinite(v)) return v;
  }
  return undefined;
}

function timestampOf(line: Record<string, unknown>): string | undefined {
  const ts = line.timestamp;
  return typeof ts === 'string' ? ts : undefined;
}

function stableUuid(ctx: CodexNormalizeContext, suffix?: string): string {
  return suffix
    ? `${ctx.rolloutBasename}:${ctx.lineIndex}:${suffix}`
    : `${ctx.rolloutBasename}:${ctx.lineIndex}`;
}

function textFromCodexContent(content: unknown, wantedType: 'input_text' | 'output_text'): string {
  if (typeof content === 'string') return content.trim();
  if (!Array.isArray(content)) return '';
  const parts: string[] = [];
  for (const raw of content) {
    if (!isRecord(raw)) continue;
    if (raw.type !== wantedType) continue;
    const text = raw.text;
    if (typeof text === 'string' && text.length > 0) parts.push(text);
  }
  return parts.join('\n\n').trim();
}

function isCodexUserPreamble(text: string): boolean {
  const t = text.trimStart();
  if (t.startsWith('<environment_context>')) return true;
  if (t.includes('<approval_policy>') && t.includes('<sandbox')) return true;
  if (t.includes('<sandbox_mode>') && t.includes('<cwd>')) return true;
  return false;
}

function parseToolInput(raw: unknown): Record<string, unknown> {
  if (typeof raw !== 'string') return {};
  try {
    const parsed = JSON.parse(raw) as unknown;
    return isRecord(parsed) ? parsed : {};
  } catch {
    return {};
  }
}

function outputToString(output: unknown): string {
  if (typeof output === 'string') return output;
  if (output == null) return '';
  try {
    return JSON.stringify(output);
  } catch {
    return String(output);
  }
}

function usageFromRecord(r: Record<string, unknown>): ClaudeUsage | null {
  const usage: ClaudeUsage = {};
  const inputTokens = numberField(r, ['input_tokens', 'inputTokens', 'input']);
  const outputTokens = numberField(r, ['output_tokens', 'outputTokens', 'output']);
  const cacheCreation = numberField(r, ['cache_creation_input_tokens', 'cacheCreationInputTokens']);
  const cacheRead = numberField(r, ['cache_read_input_tokens', 'cacheReadInputTokens', 'cached_input_tokens', 'cachedInputTokens']);
  if (inputTokens !== undefined) usage.input_tokens = inputTokens;
  if (outputTokens !== undefined) usage.output_tokens = outputTokens;
  if (cacheCreation !== undefined) usage.cache_creation_input_tokens = cacheCreation;
  if (cacheRead !== undefined) usage.cache_read_input_tokens = cacheRead;
  return Object.keys(usage).length > 0 ? usage : null;
}

function usageFromUnknown(x: unknown, depth: number = 0): ClaudeUsage | null {
  if (!isRecord(x) || depth > 3) return null;
  const direct = usageFromRecord(x);
  if (direct) return direct;
  const nestedKeys = [
    'usage',
    'tokens',
    'token_count',
    'tokenCount',
    'token_usage',
    'tokenUsage',
    'total_token_usage',
    'totalTokenUsage',
    'last_token_usage',
    'lastTokenUsage',
    'info'
  ];
  for (const key of nestedKeys) {
    const nested = usageFromUnknown(x[key], depth + 1);
    if (nested) return nested;
  }
  return null;
}

function normalizeMessage(payload: Record<string, unknown>, line: Record<string, unknown>, ctx: CodexNormalizeContext): ClaudeSessionEvent[] {
  const role = stringField(payload, 'role');
  if (role === 'developer') return [];

  if (role === 'assistant') {
    const text = textFromCodexContent(payload.content, 'output_text');
    if (!text) return [];
    return [{
      type: 'assistant',
      uuid: stableUuid(ctx),
      timestamp: timestampOf(line),
      message: {
        role: 'assistant',
        content: [{ type: 'text', text }]
      }
    }];
  }

  if (role === 'user') {
    const text = textFromCodexContent(payload.content, 'input_text');
    if (!text || isCodexUserPreamble(text)) return [];
    return [{
      type: 'user',
      uuid: stableUuid(ctx),
      timestamp: timestampOf(line),
      message: {
        role: 'user',
        content: [{ type: 'text', text }]
      }
    }];
  }

  return [];
}

function normalizeFunctionCall(payload: Record<string, unknown>, line: Record<string, unknown>): ClaudeSessionEvent[] {
  const callId = stringField(payload, 'call_id');
  const name = stringField(payload, 'name');
  if (!callId || !name) return [];
  return [{
    type: 'assistant',
    uuid: callId,
    timestamp: timestampOf(line),
    message: {
      role: 'assistant',
      content: [{
        type: 'tool_use',
        id: callId,
        name,
        input: parseToolInput(payload.arguments)
      }]
    }
  }];
}

function normalizeFunctionCallOutput(payload: Record<string, unknown>, line: Record<string, unknown>, ctx: CodexNormalizeContext): ClaudeSessionEvent[] {
  const callId = stringField(payload, 'call_id');
  if (!callId) return [];
  return [{
    type: 'user',
    uuid: stableUuid(ctx, `tool_result:${callId}`),
    timestamp: timestampOf(line),
    message: {
      role: 'user',
      content: [{
        type: 'tool_result',
        tool_use_id: callId,
        content: outputToString(payload.output)
      }]
    }
  }];
}

function normalizeTokenCount(payload: Record<string, unknown>, line: Record<string, unknown>, ctx: CodexNormalizeContext): ClaudeSessionEvent[] {
  const usage = usageFromUnknown(payload);
  if (!usage) return [];
  return [{
    type: 'assistant',
    uuid: stableUuid(ctx, 'usage'),
    timestamp: timestampOf(line),
    message: {
      role: 'assistant',
      content: [],
      usage
    }
  }];
}

function normalizeTaskComplete(line: Record<string, unknown>, ctx: CodexNormalizeContext): ClaudeSessionEvent[] {
  return [{
    type: 'assistant',
    uuid: stableUuid(ctx, 'task_complete'),
    timestamp: timestampOf(line),
    message: {
      role: 'assistant',
      content: [],
      stop_reason: 'end_turn'
    }
  }];
}

export function normalizeCodexRolloutLine(raw: unknown, ctx: CodexNormalizeContext): CodexNormalization {
  const empty: CodexNormalization = { events: [], working: null };
  if (!isRecord(raw)) return empty;
  const lineType = stringField(raw, 'type');
  const payload = raw.payload;
  if (!lineType || !isRecord(payload)) return empty;

  if (lineType === 'response_item') {
    const payloadType = stringField(payload, 'type');
    if (payloadType === 'message') {
      return { events: normalizeMessage(payload, raw, ctx), working: null };
    }
    if (payloadType === 'function_call') {
      return { events: normalizeFunctionCall(payload, raw), working: null };
    }
    if (payloadType === 'function_call_output') {
      return { events: normalizeFunctionCallOutput(payload, raw, ctx), working: null };
    }
    return empty;
  }

  if (lineType === 'event_msg') {
    const payloadType = stringField(payload, 'type');
    if (payloadType === 'task_started') {
      return { events: [], working: true };
    }
    if (payloadType === 'task_complete') {
      return { events: normalizeTaskComplete(raw, ctx), working: false };
    }
    if (payloadType === 'token_count') {
      return { events: normalizeTokenCount(payload, raw, ctx), working: null };
    }
  }

  return empty;
}
