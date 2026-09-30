import { useEffect, useState } from 'react';
import { fetchServerHealth } from '../api/sessionsd';
import { isLocalServer, serverDisplayName, useServers } from '../lib/servers';

export interface OtherMachine {
  id: string;
  name: string;
  reachability: 'checking' | 'reachable' | 'unreachable';
}

const PROBE_TIMEOUT_MS = 4_000;

// The other machines in Fleet and whether each answers right now. One probe
// per machine when the caller mounts; a dead machine cannot delay the answer
// for a live one, and a machine that is offline is reported as such rather
// than silently missing.
export function useOtherMachines(enabled: boolean): OtherMachine[] {
  const servers = useServers((state) => state.servers);
  const [machines, setMachines] = useState<OtherMachine[]>([]);
  const others = servers.filter((server) => !(server.isDefault && isLocalServer(server)));
  const key = others.map((server) => server.id).join(',');
  useEffect(() => {
    if (!enabled) return;
    let alive = true;
    const initial = others.map((server): OtherMachine => ({ id: server.id, name: serverDisplayName(server), reachability: 'checking' }));
    setMachines(initial);
    const controllers = others.map((server) => {
      const controller = new AbortController();
      const timer = window.setTimeout(() => controller.abort(), PROBE_TIMEOUT_MS);
      void fetchServerHealth(server, controller.signal)
        .then(() => 'reachable' as const, () => 'unreachable' as const)
        .then((reachability) => {
          window.clearTimeout(timer);
          if (!alive) return;
          setMachines((current) => current.map((machine) => machine.id === server.id ? { ...machine, reachability } : machine));
        });
      return controller;
    });
    return () => {
      alive = false;
      for (const controller of controllers) controller.abort();
    };
    // `key` stands in for the server list so a renamed machine does not re-probe.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [enabled, key]);
  return machines;
}
