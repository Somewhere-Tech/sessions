// CAPABILITY: a client-only phone paired with one host inherits that host's
// approved fleet without receiving or dialing another machine's credential.
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { httpBaseForServer, serverFetch } from '../../src/api/sessionsd/core';
import { refreshFleetServersFromHost } from '../../src/lib/fleetRelay';
import {
  useServers,
  type ServerConfig
} from '../../src/lib/servers';

const host: ServerConfig = {
  id: 'paired-host',
  machineId: 'machine-a',
  name: 'Mac A',
  systemName: 'Mac A',
  host: '192.168.1.10',
  port: 8897,
  scheme: 'http',
  token: 'phone-on-a',
  isDefault: false
};

function fleetResponse(machines: unknown[]): Response {
  return new Response(JSON.stringify({ machines }), {
    status: 200,
    headers: { 'content-type': 'application/json' }
  });
}

describe('capability: inherit a paired host fleet', () => {
  beforeEach(() => {
    window.localStorage.clear();
    useServers.setState({
      servers: [{ ...host }],
      activeId: host.id,
      pairingError: null,
      credentialError: null,
      tokenRequiredServerId: null
    });
    vi.restoreAllMocks();
  });

  it('keeps the paired host first and routes inherited machines through it', async () => {
    const fetchMock = vi.spyOn(window, 'fetch').mockResolvedValue(fleetResponse([
      {
        id: 'machine-b',
        name: 'Mac B',
        endpoint: 'https://mac-b.example.ts.net',
        transport: 'tailnet',
        lan_endpoint: 'http://192.168.1.20:8787',
        tailnet_endpoint: 'https://mac-b.example.ts.net',
        tailnet_ip_endpoint: 'http://100.100.20.30:8787',
        reachable: true
      }
    ]));

    const servers = await refreshFleetServersFromHost(host.id);

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [target, init] = fetchMock.mock.calls[0];
    expect(target).toBe('http://192.168.1.10:8897/api/fleet/machines');
    expect(init?.headers).toEqual({ Authorization: 'Bearer phone-on-a' });
    expect(servers.map((server) => server.name)).toEqual(['Mac A', 'Mac B']);
    const inherited = servers[1];
    expect(inherited).toMatchObject({
      machineId: 'machine-b',
      relayParentId: host.id,
      relayMachineId: 'machine-b',
      transport: 'tailnet',
      token: 'phone-on-a'
    });
    expect(httpBaseForServer(inherited)).toBe(
      'http://192.168.1.10:8897/api/fleet/machine-b'
    );
    expect(window.localStorage.getItem('sessions:servers')).toBeNull();
  });

  it('replaces the inherited set on refresh while leaving direct machines alone', async () => {
    const fetchMock = vi.spyOn(window, 'fetch');
    fetchMock.mockResolvedValueOnce(fleetResponse([
      { id: 'machine-b', name: 'Mac B', endpoint: 'http://10.0.0.2:8787', transport: 'lan', reachable: false }
    ]));
    await refreshFleetServersFromHost(host.id);
    fetchMock.mockResolvedValueOnce(fleetResponse([
      { id: 'machine-c', name: 'Mac C', endpoint: 'http://10.0.0.3:8787', transport: 'lan', reachable: true }
    ]));

    const refreshed = await refreshFleetServersFromHost(host.id);

    expect(refreshed.map((server) => server.machineId)).toEqual(['machine-a', 'machine-c']);
    expect(httpBaseForServer(refreshed[1])).toContain('/api/fleet/machine-c');
  });

  it('updates the displayed transport when the host falls back', async () => {
    const fetchMock = vi.spyOn(window, 'fetch');
    fetchMock.mockResolvedValueOnce(fleetResponse([
      { id: 'machine-b', name: 'Mac B', transport: 'lan', reachable: true }
    ]));
    await refreshFleetServersFromHost(host.id);
    fetchMock.mockResolvedValueOnce(fleetResponse([
      { id: 'machine-b', name: 'Mac B', transport: 'tailnet-ip', reachable: true }
    ]));

    await refreshFleetServersFromHost(host.id);

    expect(useServers.getState().servers[1]?.transport).toBe('tailnet-ip');
  });

  it('tries direct transports before the owner-hosted relay', async () => {
    const machine: ServerConfig = {
      ...host,
      id: 'fallback-machine',
      machineId: 'machine-fallback',
      token: 'device-token',
      transportCandidates: [
        { endpoint: 'http://192.168.1.44:8787', transport: 'lan' },
        { endpoint: 'https://machine.example.ts.net', transport: 'tailnet' },
        { endpoint: 'http://100.100.44.1:8787', transport: 'tailnet-ip' },
        { endpoint: 'https://relay.example/m/machine-fallback', transport: 'relay' }
      ]
    };
    useServers.setState({ servers: [machine], activeId: machine.id });
    const fetchMock = vi.spyOn(window, 'fetch')
      .mockRejectedValueOnce(new TypeError('LAN unavailable'))
      .mockRejectedValueOnce(new TypeError('tailnet DNS unavailable'))
      .mockRejectedValueOnce(new TypeError('tailnet address unavailable'))
      .mockResolvedValueOnce(new Response('{}', { status: 200 }))
      .mockResolvedValueOnce(new Response('{"ok":true}', { status: 200 }));

    const response = await serverFetch(machine, 'http://192.168.1.44:8787/api/health');

    expect(response.status).toBe(200);
    expect(fetchMock.mock.calls.map(([target]) => String(target))).toEqual([
      'http://192.168.1.44:8787/api/machine',
      'https://machine.example.ts.net/api/machine',
      'http://100.100.44.1:8787/api/machine',
      'https://relay.example/m/machine-fallback/api/machine',
      'https://relay.example/m/machine-fallback/api/health'
    ]);
    expect(useServers.getState().servers[0]?.transport).toBe('relay');
  });
});
