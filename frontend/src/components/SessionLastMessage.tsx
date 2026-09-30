import type { SessionInfo } from '../types';
import { lastMessage, lastMessageLine } from '../lib/lastMessage';

// The newest thing that happened in a session, said plainly: who spoke and what
// they said, or that the person is waiting, or that the provider failed. A
// fault is styled as a fault so it cannot be mistaken for the agent's reply.
export function SessionLastMessage(
  { session, sending }: { session: SessionInfo; sending?: string }
): JSX.Element | null {
  const message = lastMessage(session, sending);
  const line = lastMessageLine(session, sending);
  if (!line) return null;
  return (
    <span
      className={`session-last-message is-${message.kind}`}
      title={message.kind === 'fault' ? 'The provider reported this; it is not a message from the agent.' : line}
    >
      {line}
    </span>
  );
}
