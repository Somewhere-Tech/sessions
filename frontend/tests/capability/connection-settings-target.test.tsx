import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  fetchFleetAccount, requestFleetMagicLink, verifyFleetMagicLink, logoutFleetAccount,
  fetchRemoteState, setRemoteAuto, fetchRelayState, setRelayURL, fetchLANState,
  setLANEnabled, requestLocalNetworkAccess, listPairedDevices, revokePairingTicket, forgetPairedDevice,
  fetchFleetDirectory, claimFleetDirectoryMachine
} from '../../src/api/sessionsd';
import { connectionSettingsTarget } from '../../src/lib/connectionSettingsTarget';
import { useServers } from '../../src/lib/servers';
import { useFakeMachines, type FakeMachine } from './fake-daemon';

const machines: FakeMachine[] = [
  { id: 'local', name: 'MacBook', host: 'localhost', port: 8787, isDefault: true, sessions: [] },
  { id: 'mini', name: 'Mini', host: '10.0.0.8', port: 8787, sessions: [] }
];

afterEach(() => {
  Reflect.deleteProperty(window, '__TAURI_INTERNALS__');
  vi.restoreAllMocks();
});

describe('capability: connection settings have an explicit owner', () => {
  it('keeps all desktop administration on loopback while viewing a remote chat', async () => {
    useFakeMachines(machines, 'mini');
    Object.defineProperty(window, '__TAURI_INTERNALS__', { configurable: true, value: {} });
    useServers.setState((state) => ({ servers: state.servers.map((server) => server.isDefault
      ? { ...server, token: 'local-token', machineId: 'macbook', transportCandidates: [{ endpoint: 'http://10.0.0.9:8787', transport: 'lan' as const }] }
      : server) }));
    const requests: Array<{ url: string; authorization: string | null }> = [];
    globalThis.fetch = async (input, init) => {
      requests.push({ url: String(input), authorization: new Headers(init?.headers).get('Authorization') });
      return new Response(JSON.stringify({ ok: true, devices: [], signed_in: false }), { status: 200 });
    };
    await Promise.all([
      fetchFleetAccount(), requestFleetMagicLink('test@example.test'), verifyFleetMagicLink('test-token'),
      logoutFleetAccount(), fetchRemoteState(), setRemoteAuto(false), fetchRelayState(), setRelayURL(''),
      fetchLANState(), setLANEnabled(false), requestLocalNetworkAccess(), listPairedDevices(),
      revokePairingTicket('ticket'), forgetPairedDevice('device'), fetchFleetDirectory(), claimFleetDirectoryMachine('mini')
    ]);
    expect(requests).toHaveLength(16);
    expect(requests.every((request) => new URL(request.url).origin === 'http://localhost:8787')).toBe(true);
    expect(requests.every((request) => request.authorization === 'Bearer local-token')).toBe(true);
    expect(useServers.getState().activeId).toBe('mini');
  });

  it('keeps a phone host-inheriting rather than inventing a local server', () => {
    useFakeMachines(machines, 'mini');
    Object.defineProperty(window, '__TAURI_INTERNALS__', { configurable: true, value: {} });
    vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue('iPhone');
    expect(connectionSettingsTarget().id).toBe('mini');
  });

  it('does not silently substitute another computer if local settings are unavailable', () => {
    useFakeMachines([machines[1]]);
    Object.defineProperty(window, '__TAURI_INTERNALS__', { configurable: true, value: {} });
    expect(() => connectionSettingsTarget()).toThrow('This computer’s connection is not ready');
  });
});
