import type { ClaudeSessionEvent } from '../types';

function canonicalEvent(event: ClaudeSessionEvent): string {
  return JSON.stringify(event, (_key, value: unknown) => {
    if (!value || typeof value !== 'object' || Array.isArray(value)) return value;
    const record = value as Record<string, unknown>;
    return Object.fromEntries(Object.keys(record).sort().map((key) => [key, record[key]]));
  });
}

// Normalized Codex history has no UUID. Keep identities tied to the durable
// event, not its position in a history window that can grow at either end.
export function codexEventIdentity(event: ClaudeSessionEvent, occurrences: Map<string, number>): string {
  if (event.uuid) return event.uuid;
  const value = canonicalEvent(event);
  let hash = 2166136261;
  for (let index = 0; index < value.length; index += 1) {
    hash = Math.imul(hash ^ value.charCodeAt(index), 16777619);
  }
  const base = `codex-event-${event.timestamp ?? ''}-${(hash >>> 0).toString(16)}`;
  const occurrence = (occurrences.get(base) ?? 0) + 1;
  occurrences.set(base, occurrence);
  return occurrence === 1 ? base : `${base}-occurrence-${occurrence}`;
}

export function uniqueCodexEvents(events: ClaudeSessionEvent[]): ClaudeSessionEvent[] {
  const seen = new Map<string, ClaudeSessionEvent[]>();
  return events.filter((event) => {
    // Without a timestamp or UUID there is no durable event identity. Equal
    // delta strings may be intentional repetitions, so never infer duplicates.
    if (!event.uuid && !event.timestamp) return true;
    // Submission time is not an operation ID. Identical authored messages
    // without UUIDs can be separate sends, even if their timestamps match.
    if (!event.uuid && (event.type === 'user' || event.type === 'assistant')) return true;
    // Bucket by small metadata, never by serialized transcript/tool output.
    // A key collision is only a candidate: exact equality decides duplicates.
    const key = JSON.stringify([event.uuid, event.timestamp, event.type, event.subtype,
      event.conversationId, event.turnId, event.itemId]);
    const bucket = seen.get(key) ?? [];
    if (bucket.some((previous) => equalJSON(previous, event))) return false;
    bucket.push(event);
    seen.set(key, bucket);
    return true;
  });
}

function equalJSON(left: unknown, right: unknown): boolean {
  if (Object.is(left, right)) return true;
  if (!left || !right || typeof left !== 'object' || typeof right !== 'object') return false;
  if (Array.isArray(left) !== Array.isArray(right)) return false;
  const leftRecord = left as Record<string, unknown>;
  const rightRecord = right as Record<string, unknown>;
  const keys = Object.keys(leftRecord);
  if (keys.length !== Object.keys(rightRecord).length) return false;
  return keys.every((key) => Object.prototype.hasOwnProperty.call(rightRecord, key)
    && equalJSON(leftRecord[key], rightRecord[key]));
}
