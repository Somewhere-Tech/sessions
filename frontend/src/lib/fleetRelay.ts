import { useEffect, useState } from 'react';
import { httpBaseForServer, serverFetch } from '../api/sessionsd/core';
import {
  useServers,
  type ServerConfig
} from './servers';

export function useFleetRelayServers(enabled: boolean): string[] {
  const servers = useServers((state) => state.servers);
  const [errors, setErrors] = useState<string[]>([]);
  const hosts = servers.filter((server) => !server.relayMachineId && !server.directoryOnly).sort((a, b) => a.id.localeCompare(b.id));
  const key = JSON.stringify(hosts.map((host) => [host.id, host.scheme, host.host, host.port, host.token, host.machineId]));
  useEffect(() => {
    if (!enabled) return;
    let stopped = false;
    let timer: number | undefined;
    const controller = new AbortController();
    const refresh = async (): Promise<void> => {
      const failures: string[] = [];
      // Sequential and completion-scheduled: a slow/offline machine cannot
      // accumulate overlapping discovery sweeps every fifteen seconds.
      for (const host of hosts) {
        if (stopped) return;
        try {
          await refreshFleetServersFromHost(host.id, controller.signal);
        } catch (error) {
          failures.push(`${host.name}: ${error instanceof Error ? error.message : 'Could not refresh computers.'}`);
        }
      }
      if (!stopped) {
        setErrors(failures);
        timer = window.setTimeout(() => { void refresh(); }, 15_000);
      }
    };
    void refresh();
    return () => {
      stopped = true;
      controller.abort();
      window.clearTimeout(timer);
    };
  // key is the value-equal direct endpoint identity. Relay reachability must
  // not restart this reconnect loop.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [enabled, key]);
  return errors;
}

interface RelayedFleetMachine {
  id: string;
  name: string;
  transport?: 'lan' | 'tailnet' | 'tailnet-ip';
}

// Refresh the fleet inherited from one paired host. The phone keeps only the
// host's credential and sends every inherited request back through that host;
// endpoint is display metadata, never a direct connection target.
export async function refreshFleetServersFromHost(hostId: string, signal?: AbortSignal): Promise<ServerConfig[]> {
  const state = useServers.getState();
  const host = state.servers.find((server) => server.id === hostId && !server.relayMachineId);
  if (!host) return state.servers;
  const response = await serverFetch(host, `${httpBaseForServer(host)}/api/fleet/machines`, {
    signal: signal ? AbortSignal.any([signal, AbortSignal.timeout(10_000)]) : AbortSignal.timeout(10_000)
  });
  if (!response.ok) {
    throw new Error(`Sessions could not refresh the host fleet (HTTP ${response.status}).`);
  }
  const body = await response.json() as { machines?: RelayedFleetMachine[] };
  if (!Array.isArray(body.machines)) {
    throw new Error('Sessions received an invalid host fleet response.');
  }

  const latest = useServers.getState();
  const currentHost = latest.servers.find((server) => server.id === hostId && !server.relayMachineId);
  if (!currentHost || signal?.aborted || currentHost.token !== host.token || currentHost.machineId !== host.machineId) return latest.servers;
  const rest = latest.servers.filter((server) => server.id !== hostId && server.relayParentId !== hostId);
  const directIDs = new Set(rest.map((server) => server.machineId));
  const relayed = body.machines.flatMap((machine): ServerConfig[] => {
    if (typeof machine?.id !== 'string' || !machine.id || typeof machine.name !== 'string' || !machine.name || machine.id === currentHost.machineId || directIDs.has(machine.id)) return [];
    directIDs.add(machine.id);
    return [{
      id: `fleet:${hostId}:${machine.id}`,
      machineId: machine.id,
      systemName: machine.name,
      name: machine.name,
      host: currentHost.host,
      port: currentHost.port,
      scheme: currentHost.scheme,
      token: currentHost.token,
      isDefault: false,
      relayParentId: hostId,
      relayMachineId: machine.id,
      transport: machine.transport
    }];
  });
  const servers = latest.servers.filter((server) => server.relayParentId !== hostId)
    .flatMap((server) => server.id === hostId ? [currentHost, ...relayed] : [server]);
  const activeId = servers.some((server) => server.id === latest.activeId)
    ? latest.activeId
    : currentHost.id;
  const signature = (items: ServerConfig[]): string => items
    .map((server) => JSON.stringify([server.id, server.name, server.host, server.port, server.scheme, server.token, server.relayParentId, server.relayMachineId, server.transport]))
    .join('|');
  const unchanged = signature(servers) === signature(latest.servers);
  if (unchanged && activeId === latest.activeId) return latest.servers;
  useServers.setState({ servers, activeId });
  return servers;
}
