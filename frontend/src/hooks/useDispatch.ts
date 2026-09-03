import { useCallback, useEffect, useRef, useState } from 'react';
import { useServers } from '../lib/servers';
import type { DispatchMessage } from '../types';

export type { DispatchMessage, MessagePlanStep, ToolCall } from '../types';

const STORAGE_PREFIX = 'sessions:dispatch:';
const MAX_PER_SESSION = 200;
const LEGACY_FALSE_FAILURE = 'no matching user event appeared within 6s';

function storageKey(serverId: string, sessionId: string): string {
  return `${STORAGE_PREFIX}${serverId}:${sessionId}`;
}

function readStored(serverId: string, sessionId: string): DispatchMessage[] {
  try {
    const raw = window.localStorage.getItem(storageKey(serverId, sessionId));
    if (!raw) return [];
    const parsed = JSON.parse(raw) as Array<Omit<DispatchMessage, 'status'> & { status?: string }>;
    if (!Array.isArray(parsed)) return [];
    return parsed.slice(-MAX_PER_SESSION).map((message) => {
      // Older builds recorded a message only after sessionsd acknowledged the
      // send, but then downgraded it to failed when transcript mirroring took
      // longer than six seconds. Preserve the acknowledged fact on upgrade.
      if (message.status === 'pending' || (
        message.status === 'failed' && message.failureReason === LEGACY_FALSE_FAILURE
      )) {
        return {
          ...message,
          status: 'accepted' as const,
          failureReason: undefined
        };
      }
      return message as DispatchMessage;
    });
  } catch {
    return [];
  }
}

function writeStored(serverId: string, sessionId: string, messages: DispatchMessage[]): void {
  try {
    window.localStorage.setItem(
      storageKey(serverId, sessionId),
      JSON.stringify(messages.slice(-MAX_PER_SESSION))
    );
  } catch {
    // Private mode or a full browser quota must never block a message send.
  }
}

interface Args {
  sessionId: string;
  // Occurrence count for exact user text in authoritative provider history.
  // Counts, rather than a set, distinguish repeated identical messages.
  eventUserContentCounts?: ReadonlyMap<string, number>;
}

export interface DispatchAPI {
  messages: DispatchMessage[];
  // Called only after sessionsd acknowledges the atomic text + Enter submit.
  recordSent: (content: string, queued?: boolean) => void;
  restoreDraft: (id: string) => void;
  remove: (id: string) => void;
  resetLog: () => void;
}

export function useDispatch({ sessionId, eventUserContentCounts }: Args): DispatchAPI {
  const activeServerId = useServers((state) => state.activeId!);
  const [messages, setMessages] = useState<DispatchMessage[]>(() =>
    readStored(activeServerId, sessionId)
  );
  const messagesRef = useRef(messages);
  const eventCountsRef = useRef(eventUserContentCounts);

  useEffect(() => {
    setMessages(readStored(activeServerId, sessionId));
  }, [activeServerId, sessionId]);

  useEffect(() => {
    messagesRef.current = messages;
    writeStored(activeServerId, sessionId, messages);
  }, [activeServerId, messages, sessionId]);

  useEffect(() => {
    eventCountsRef.current = eventUserContentCounts;
    if (!eventUserContentCounts || eventUserContentCounts.size === 0) return;
    setMessages((previous) => {
      let changed = false;
      const next = previous.map((message) => {
        if (message.role !== 'user' || (message.status !== 'accepted' && message.status !== 'queued')) return message;
        const count = eventUserContentCounts.get(message.content.trim()) ?? 0;
        if (count <= (message.confirmBaseline ?? 0)) return message;
        changed = true;
        return {
          ...message,
          status: 'sent' as const,
          queued: false,
          confirmedAt: Date.now()
        };
      });
      return changed ? next : previous;
    });
  }, [eventUserContentCounts]);

  const recordSent = useCallback((content: string, queued = false): void => {
    if (!content.trim()) return;
    const now = Date.now();
    const previous = messagesRef.current;
    const trimmed = content.trim();
    const providerCount = eventCountsRef.current?.get(trimmed) ?? 0;
    const acceptedAhead = previous.filter((message) =>
      message.role === 'user'
      && (message.status === 'accepted' || message.status === 'queued')
      && message.content.trim() === trimmed
    ).length;
    const message: DispatchMessage = {
      id: `user-${now}-${Math.random().toString(36).slice(2, 8)}`,
      role: 'user',
      content,
      status: queued ? 'queued' : 'accepted',
      createdAt: now,
      confirmedAt: now,
      queued: queued || undefined,
      confirmBaseline: providerCount + acceptedAhead
    };
    setMessages((current) => [...current, message]);
  }, []);

  const restoreDraft = useCallback((id: string): void => {
    setMessages((previous) => previous.map((message) =>
      message.id === id ? { ...message, createdAt: Date.now() } : message
    ));
  }, []);

  const remove = useCallback((id: string): void => {
    setMessages((previous) => previous.filter((message) => message.id !== id));
  }, []);

  const resetLog = useCallback((): void => {
    setMessages([]);
  }, []);

  return { messages, recordSent, restoreDraft, remove, resetLog };
}
