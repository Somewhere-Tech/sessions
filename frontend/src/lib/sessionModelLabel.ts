import type { ClaudeSessionEvent } from '../types';

/** Explicit next-message selection wins; otherwise show observed provider identity. */
export function observedSessionModel(selected: string | undefined, events: ClaudeSessionEvent[]): string | undefined {
  if (selected) return selected;
  for (let i = events.length - 1; i >= 0; i--) {
    const event = events[i];
    if (event?.type !== 'assistant') continue;
    const model = event.message?.model;
    if (model && model !== '<synthetic>') return model;
  }
  return undefined;
}
