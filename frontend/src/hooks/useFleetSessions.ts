import { useEffect, useMemo, useState } from 'react';
import { listServerSessions } from '../api/sessionsd';
import type { ServerConfig } from '../lib/servers';
import type { SessionInfo } from '../types';

const POLL_INTERVAL_MS = 5_000;
const POLL_TIMEOUT_MS = 5_000;

export interface FleetSessionSnapshot {
  server: ServerConfig;
  sessions: SessionInfo[];
  loading: boolean;
  error: string | null;
}

interface SnapshotState {
  sessions: SessionInfo[];
  loading: boolean;
  error: string | null;
}

const EMPTY_SNAPSHOT: SnapshotState = {
  sessions: [],
  loading: true,
  error: null
};

// The operations navigator is allowed to read every machine the user already
// paired, but it does not create a new connection or share credentials between
// hosts. Each request goes directly to its configured sessionsd with that
// machine's existing revocable credential. Slow/offline machines poll
// independently so one computer cannot hold the rest of the inbox hostage.
export function useFleetSessions(
  servers: ServerConfig[],
  enabled: boolean
): FleetSessionSnapshot[] {
  const [snapshots, setSnapshots] = useState<Record<string, SnapshotState>>({});

  useEffect(() => {
    if (!enabled || servers.length === 0) return;

    let stopped = false;
    const controllers = new Map<string, AbortController>();
    const pollTimers = new Map<string, number>();

    setSnapshots((current) => {
      const next: Record<string, SnapshotState> = {};
      for (const server of servers) {
        next[server.id] = current[server.id] ?? EMPTY_SNAPSHOT;
      }
      return next;
    });

    for (const server of servers) {
      const poll = async (): Promise<void> => {
        if (stopped) return;
        const controller = new AbortController();
        controllers.set(server.id, controller);
        const timeout = window.setTimeout(() => controller.abort(), POLL_TIMEOUT_MS);

        try {
          const sessions = await listServerSessions(server, controller.signal);
          if (!stopped) {
            setSnapshots((current) => ({
              ...current,
              [server.id]: { sessions, loading: false, error: null }
            }));
          }
        } catch (reason) {
          if (!stopped) {
            setSnapshots((current) => ({
              ...current,
              [server.id]: {
                sessions: current[server.id]?.sessions ?? [],
                loading: false,
                error: reason instanceof Error ? reason.message : 'Session list unavailable'
              }
            }));
          }
        } finally {
          window.clearTimeout(timeout);
          controllers.delete(server.id);
          if (!stopped) {
            pollTimers.set(server.id, window.setTimeout(() => { void poll(); }, POLL_INTERVAL_MS));
          }
        }
      };

      void poll();
    }

    return () => {
      stopped = true;
      controllers.forEach((controller) => controller.abort());
      pollTimers.forEach((timer) => window.clearTimeout(timer));
    };
  }, [enabled, servers]);

  return useMemo(() => servers.map((server) => ({
    server,
    ...(snapshots[server.id] ?? EMPTY_SNAPSHOT)
  })), [servers, snapshots]);
}
