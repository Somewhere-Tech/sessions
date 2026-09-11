import type { SessionInfo } from '../types';

// What a session's last message actually is.
//
// Every list in the app answered this differently and all of them could be
// wrong: `lastSummary` is the last result the agent produced and deliberately
// survives the next turn, so a row kept showing it after the person had already
// sent something else; a provider fault used to be written into that same field,
// so an outage read as the agent's reply; and the timestamp beside it came from
// whatever the session last did, not from the message being shown.
//
// The daemon stamps who spoke and when at the input boundary
// (`lastHumanMessageAt`, `lastAgentMessageAt`) and records a failure with its
// own time. Those three are the only facts about ordering, so they decide what
// is newest, and the text is shown only for the side it belongs to.

export type LastMessageKind = 'agent' | 'you' | 'fault' | 'none';

export interface LastMessage {
  kind: LastMessageKind;
  /** Who spoke, for a row that shows a prefix. Empty for a fault or nothing. */
  speaker: string;
  /** The message itself, or the fault's own words. Empty when only a time is known. */
  text: string;
  /** When the thing being shown happened — not when the session last did anything. */
  at: number;
  /** True while the agent has been sent something it has not answered. */
  awaitingReply: boolean;
}

function firstLine(value: string | undefined): string {
  return value?.trim().split('\n')[0]?.trim() ?? '';
}

/**
 * The newest message in either direction, with who said it.
 *
 * `sending` is the composer's own optimistic state: a message this client has
 * submitted and not yet seen acknowledged. It is labelled as unconfirmed rather
 * than shown as delivered, because a receipt is what makes a send a fact.
 */
export function lastMessage(session: SessionInfo, sending?: string): LastMessage {
  if (sending?.trim()) {
    return {
      kind: 'you', speaker: 'You', text: firstLine(sending),
      at: Date.now(), awaitingReply: true
    };
  }

  const humanAt = session.lastHumanMessageAt ?? 0;
  const agentAt = session.lastAgentMessageAt ?? 0;
  const faultAt = session.failureKind ? session.failureAt ?? 0 : 0;

  // A fault is the newest thing that happened only if it happened after both
  // sides last spoke. It is never presented as a message.
  if (faultAt > 0 && faultAt >= humanAt && faultAt >= agentAt) {
    return {
      kind: 'fault', speaker: '', text: firstLine(session.failureDetail) || 'The provider reported a problem.',
      at: faultAt, awaitingReply: false
    };
  }

  if (humanAt > agentAt) {
    // The person spoke last. Their text is not in the session record — what is
    // true is that they sent something and no answer has arrived.
    return { kind: 'you', speaker: 'You', text: '', at: humanAt, awaitingReply: true };
  }

  if (agentAt > 0) {
    return {
      kind: 'agent', speaker: 'Agent', text: firstLine(session.lastSummary),
      at: agentAt, awaitingReply: false
    };
  }

  // Nothing has been stamped in either direction. A summary with no time behind
  // it is still the last thing the agent produced, and it is shown without
  // claiming to be newer than anything.
  const summary = firstLine(session.lastSummary);
  if (summary) {
    return { kind: 'agent', speaker: 'Agent', text: summary, at: session.lastDataAt ?? 0, awaitingReply: false };
  }
  return { kind: 'none', speaker: '', text: '', at: 0, awaitingReply: false };
}

/** One line for a list row: who spoke and what they said, or what is pending. */
export function lastMessageLine(session: SessionInfo, sending?: string): string {
  const message = lastMessage(session, sending);
  switch (message.kind) {
    case 'fault':
      return message.text;
    case 'you':
      if (message.text) return `You: ${message.text}${sending ? ' · sending…' : ''}`;
      return session.working ? 'You sent a message · working' : 'You sent a message · no reply yet';
    case 'agent':
      return message.text ? `Agent: ${message.text}` : 'Agent replied';
    default:
      return '';
  }
}
