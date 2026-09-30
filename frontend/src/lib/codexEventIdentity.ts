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
export function codexEventIdentity(event: ClaudeSessionEvent): string {
  if (event.uuid) return event.uuid;
  const value = canonicalEvent(event);
  let hash = 2166136261;
  for (let index = 0; index < value.length; index += 1) {
    hash = Math.imul(hash ^ value.charCodeAt(index), 16777619);
  }
  return `codex-event-${event.timestamp ?? ''}-${(hash >>> 0).toString(16)}`;
}

export function uniqueCodexEvents(events: ClaudeSessionEvent[]): ClaudeSessionEvent[] {
  const seen = new Set<string>();
  return events.filter((event) => {
    // Without a timestamp or UUID there is no durable event identity. Equal
    // delta strings may be intentional repetitions, so never infer duplicates.
    if (!event.uuid && !event.timestamp) return true;
    const key = canonicalEvent(event);
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
}
