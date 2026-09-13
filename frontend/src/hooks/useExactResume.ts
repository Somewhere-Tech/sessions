import { useCallback, useRef, useState } from 'react';
import { useServers } from '../lib/servers';
import { useSessions } from '../store/sessions';
import type { SessionInfo } from '../types';

/** Resume one chosen conversation, keeping retries and machine switches scoped. */
export function useExactResume(onOpen: (id: string) => void): {
  resume: (session: SessionInfo, serverId?: string) => Promise<void>;
  notice: string | null;
  dismiss: () => void;
} {
  const pending = useRef(new Map<string, Promise<void>>());
  const [notice, setNotice] = useState<string | null>(null);
  const resume = useCallback((session: SessionInfo, targetServerId?: string): Promise<void> => {
    const serverId = targetServerId ?? useServers.getState().activeId;
    if (!serverId) {
      setNotice('Choose the computer that holds this conversation, then resume it.');
      return Promise.resolve();
    }
    const key = `${serverId}:${session.id}`;
    const existing = pending.current.get(key);
    if (existing) return existing;
    setNotice(null);
    const operation = (async (): Promise<void> => {
      try {
        const [{ resumeExactSession }, { adoptionWarning }] = await Promise.all([
          import('../lib/resumeExactSession'), import('../lib/adoptConversation')
        ]);
        const outcome = await resumeExactSession(session, undefined, undefined, serverId);
        await useSessions.getState().refresh(serverId);
        if (useServers.getState().activeId === serverId) onOpen(outcome.result.laneId);
        setNotice(adoptionWarning(outcome));
      } catch (error) {
        setNotice(error instanceof Error ? error.message : 'Could not resume this conversation. Try again after reconnecting.');
      }
    })().finally(() => pending.current.delete(key));
    pending.current.set(key, operation);
    return operation;
  }, [onOpen]);
  return { resume, notice, dismiss: () => setNotice(null) };
}
