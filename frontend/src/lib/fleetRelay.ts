import { useEffect } from 'react';
import {
  useServers,
  type ServerConfig
} from './servers';

export function useFleetRelayServers(enabled: boolean): void {
  const servers = useServers((state) => state.servers);
  const activeId = useServers((state) => state.activeId);
  const selected = servers.find((server) => server.id === activeId);
  const hostId = selected?.relayParentId ?? selected?.id ?? '';
  const host = servers.find((server) => server.id === hostId && !server.relayMachineId);
  const key = host
    ? [host.id, host.scheme ?? 'http', host.host, host.port, host.token ?? ''].join('|')
    : '';
  useEffect(() => {
    if (!enabled || !host) return;
    let stopped = false;
    const refresh = (): void => {
      if (!stopped) void refreshFleetServersFromHost(host.id).catch(() => {});
    };
    refresh();
    const interval = window.setInterval(refresh, 15_000);
    return () => {
      stopped = true;
      window.clearInterval(interval);
    };
  // key is the value-equal direct endpoint identity. Relay reachability must
  // not restart this reconnect loop.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [enabled, key]);
}

interface RelayedFleetMachine {
  id: string;
  name: string;
  transport?: 'lan' | 'tailnet' | 'tailnet-ip';
}

function directServerOrigin(server: ServerConfig): string {
  const host = server.host.includes(':') && !server.host.startsWith('[')
    ? `[${server.host}]`
    : server.host;
  return `${server.scheme ?? 'http'}://${host}:${server.port}`;
}

// Refresh the fleet inherited from one paired host. The phone keeps only the
// host's credential and sends every inherited request back through that host;
// endpoint is display metadata, never a direct connection target.
export async function refreshFleetServersFromHost(hostId: string): Promise<ServerConfig[]> {
  const state = useServers.getState();
  const host = state.servers.find((server) => server.id === hostId && !server.relayMachineId);
  if (!host) return state.servers;
  const response = await fetch(`${directServerOrigin(host)}/api/fleet/machines`, {
    headers: host.token ? { Authorization: `Bearer ${host.token}` } : {}
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
  if (!currentHost) return latest.servers;
  const rest = latest.servers.filter((server) => server.id !== hostId && server.relayParentId !== hostId);
  const directIDs = new Set(rest.map((server) => server.machineId));
  const relayed = body.machines.flatMap((machine): ServerConfig[] => {
    if (!machine?.id || !machine.name || machine.id === currentHost.machineId || directIDs.has(machine.id)) return [];
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
  const servers = [currentHost, ...relayed, ...rest];
  const activeId = servers.some((server) => server.id === latest.activeId)
    ? latest.activeId
    : currentHost.id;
  const signature = (items: ServerConfig[]): string => items
    .map((server) => [server.id, server.name, server.relayParentId, server.relayMachineId, server.transport].join(':'))
    .join('|');
  const unchanged = signature(servers) === signature(latest.servers);
  if (unchanged && activeId === latest.activeId) return latest.servers;
  useServers.setState({ servers, activeId });
  return servers;
}
