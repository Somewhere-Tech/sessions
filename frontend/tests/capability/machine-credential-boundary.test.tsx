import { expect, it, vi } from 'vitest';
import { syncNativeAgentMachineAccess, useServers } from '../../src/lib/servers';

const sync = vi.hoisted(() => vi.fn().mockResolvedValue(undefined));
vi.mock('../../src/lib/tauriBridge', () => ({
  isTauri: () => true,
  syncNativeAgentMachines: sync,
  loadNativeMachineCredentials: vi.fn(),
  saveNativeMachineCredentials: vi.fn()
}));

it('exports paired device credentials but not manually supplied host-admin tokens', async () => {
  const base = { host: 'mini.local', port: 8787, name: 'Mini', isDefault: false, machineId: 'mini' };
  useServers.setState({ servers: [
    { ...base, id: 'manual', token: 'host-admin-token' },
    { ...base, id: 'paired', deviceId: 'paired-device', token: 'device-token' }
  ] });
  await syncNativeAgentMachineAccess();
  expect(sync).toHaveBeenCalledWith([expect.objectContaining({ deviceId: 'paired-device', token: 'device-token' })]);
  expect(JSON.stringify(sync.mock.calls)).not.toContain('host-admin-token');
});
