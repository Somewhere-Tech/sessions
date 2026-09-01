import type { SessionInfo } from '../types';
import { adoptConversationWithRepair, type AdoptOutcome } from './adoptConversation';
import { providerConversationId } from './sessionStatus';

/**
 * Resume the exact ended row the caller already chose.
 *
 * The global Resume surface is a browser for choosing a conversation. Once a
 * caller already has a SessionInfo, opening that browser again is both slower
 * and ambiguous. This helper preserves the source runtime link and falls back
 * to Sessions-authored history only when the older runner has no provider id.
 */
export function resumeExactSession(
  session: SessionInfo,
  destinationProvider?: 'claude' | 'codex',
  runtimeMode?: 'rich' | 'terminal'
): Promise<AdoptOutcome> {
  const providerId = providerConversationId(session);
  return adoptConversationWithRepair(
    providerId ?? session.id,
    session.id,
    providerId ? undefined : session.id,
    destinationProvider,
    providerId ? runtimeMode : 'rich'
  );
}
