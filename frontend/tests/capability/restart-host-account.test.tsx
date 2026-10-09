import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { RestartConversationDialog } from '../../src/components/RestartConversationDialog';
import { syncNativeAgentMachineAccess, useServers } from '../../src/lib/servers';
import { useSessions } from '../../src/store/sessions';
import { makeSession } from './fake-daemon';

const mocks = vi.hoisted(() => ({ sync: vi.fn(), preview: vi.fn(), restart: vi.fn(), identity: vi.fn() }));
vi.mock('../../src/lib/tauriBridge', () => ({ isTauri: () => true, syncNativeAgentMachines: mocks.sync }));
vi.mock('../../src/api/sessionsd/restart', () => ({ previewRestart: mocks.preview, restartConversation: mocks.restart }));
vi.mock('../../src/api/sessionsd/sessions', () => ({ fetchServerMachineIdentity: mocks.identity }));

const local = { id: 'local', name: 'MacBook', host: 'localhost', port: 8787, isDefault: true };
const mini = { id: 'mini', machineId: 'machine-mini', name: 'Mac mini', host: '192.168.1.20', port: 8787, token: 'already-approved', isDefault: false };
const source = makeSession({ id: 'source', name: 'New PM', tool: 'claude-code', args: ['--remote-control'], cwd: '/project', profile: '', exited: false });

describe('capability: restart preserves source host and explains account scope', () => {
  beforeEach(() => {
    useServers.setState({ servers: [local, mini], activeId: 'local' });
    useSessions.setState({ serverId: 'mini', sessions: [source], hydrated: true });
    mocks.sync.mockReset().mockResolvedValue(undefined);
    mocks.identity.mockReset().mockResolvedValue({ machineId: 'machine-mini', name: 'Mac mini', deviceId: 'paired-mini' });
    mocks.preview.mockReset().mockResolvedValue({ sourceSessionId: 'source', accountChanged: true, savedLoginEmail: 'new@example.test', warning: 'Saved login differs from original Remote Control owner.' });
    mocks.restart.mockReset().mockResolvedValue({ ok: true, sourceEnded: true, laneId: 'replacement' });
  });

  it('recovers the existing paired identity before exporting, without inferring authority from a stored token', async () => {
    useServers.setState({ servers: [local, mini, { ...mini, id: 'draft', machineId: undefined }, { ...mini, id: 'open', machineId: 'machine-open', token: undefined }, { ...mini, id: 'relay', relayMachineId: 'elsewhere' }] });
    await syncNativeAgentMachineAccess();
    expect(mocks.identity).toHaveBeenCalledTimes(1);
    expect(mocks.identity).toHaveBeenCalledWith(mini, expect.any(AbortSignal));
    expect(useServers.getState().servers.find((server) => server.id === 'mini')?.deviceId).toBe('paired-mini');
    expect(mocks.sync).toHaveBeenCalledWith([{ machineId: mini.machineId, name: 'Mac mini', endpoint: 'http://192.168.1.20:8787', deviceId: 'paired-mini', token: mini.token }]);
  });

  it.each([
    ['administrator', { machineId: 'machine-mini', name: 'Mac mini' }],
    ['wrong machine', { machineId: 'other', name: 'Other', deviceId: 'paired-other' }]
  ])('does not export %s credentials as paired access', async (_name, identity) => {
    mocks.identity.mockResolvedValue(identity);
    await syncNativeAgentMachineAccess();
    expect(mocks.sync).toHaveBeenCalledWith([]);
    expect(useServers.getState().servers.find((server) => server.id === 'mini')?.deviceId).toBeUndefined();
  });

  it('does not repair a connection whose credentials changed during the identity request', async () => {
    mocks.identity.mockImplementation(async () => {
      useServers.setState({ servers: [local, { ...mini, token: 'new-login' }] });
      return { machineId: 'machine-mini', name: 'Mac mini', deviceId: 'paired-old' };
    });
    await syncNativeAgentMachineAccess();
    expect(mocks.sync).toHaveBeenCalledWith([]);
    expect(useServers.getState().servers.find((server) => server.id === 'mini')?.deviceId).toBeUndefined();
  });

  it('checks and restarts the Mini even when the active selection is the MacBook', async () => {
    const opened = vi.fn();
    render(<RestartConversationDialog session={source} serverId="mini" onOpen={opened} onClose={vi.fn()} />);
    await screen.findByText(/new@example.test/);
    expect(screen.getByText(/On Mac mini/)).toBeTruthy();
    expect(screen.getByText(/not necessarily the account/)).toBeTruthy();
    expect(mocks.preview).toHaveBeenCalledWith('source', 'mini', expect.any(AbortSignal));
    fireEvent.click(screen.getByRole('button', { name: 'End this runtime and reopen' }));
    await waitFor(() => expect(mocks.restart).toHaveBeenCalledWith('source', 'full', true, 'mini', 'terminal'));
    expect(opened).not.toHaveBeenCalled();
    await screen.findByText(/replacement is running on the original computer/);
  });
});
