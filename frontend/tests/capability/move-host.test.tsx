import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { ContinueElsewhereButton } from '../../src/components/ContinueElsewhereButton';
import { useServers } from '../../src/lib/servers';
import { useSessions } from '../../src/store/sessions';
import { makeSession } from './fake-daemon';

const mocks = vi.hoisted(() => ({ sync: vi.fn(), machines: vi.fn(), move: vi.fn(), kill: vi.fn() }));
vi.mock('../../src/lib/tauriBridge', () => ({ isTauri: () => true, syncNativeAgentMachines: mocks.sync, listNativeMoveMachines: mocks.machines, moveNativeSession: mocks.move }));
vi.mock('../../src/api/sessionsd', () => ({ killSession: mocks.kill }));
const local = { id: 'local', name: 'MacBook', host: 'localhost', port: 8787, isDefault: true };
const mini = { id: 'mini', machineId: 'mini-id', deviceId: 'paired-mini', name: 'Mac mini', host: '192.168.1.20', port: 8787, token: 'approved', isDefault: false };
const source = makeSession({ id: 'source', name: 'New PM', tool: 'claude-code', args: [], cwd: '/project', exited: false });

describe('capability: moving keeps the original source computer', () => {
  beforeEach(() => {
    useServers.setState({ servers: [local, mini], activeId: 'mini' });
    useSessions.setState({ serverId: 'mini', sessions: [source], hydrated: true });
    mocks.sync.mockReset().mockResolvedValue(undefined);
    mocks.machines.mockReset().mockResolvedValue([{ alias: 'mini', machine_id: 'mini-id', name: 'Mac mini', endpoint: 'http://192.168.1.20:8787' }]);
    mocks.move.mockReset().mockResolvedValue({ tool: 'claude-code', conversation_bytes: 100, workspace: {}, runtime_mode: 'terminal' });
    mocks.kill.mockReset().mockResolvedValue(undefined);
  });
  it('syncs saved access before listing, offers this Mac, and does not retarget a kill after switching computers', async () => {
    render(<ContinueElsewhereButton sessionId="source" label="New PM" />);
    fireEvent.click(screen.getByRole('button', { name: 'Move to another computer' }));
    await screen.findByRole('combobox');
    expect(mocks.sync.mock.invocationCallOrder[0]).toBeLessThan(mocks.machines.mock.invocationCallOrder[0]);
    expect(screen.getByRole('option', { name: /This Mac/ })).toBeTruthy();
    useServers.setState({ activeId: 'local' });
    useSessions.setState({ serverId: 'local', sessions: [] });
    fireEvent.click(screen.getByRole('button', { name: 'End here and review move' }));
    await waitFor(() => expect(mocks.kill).toHaveBeenCalledWith('source', 'Moved to another computer through Sessions', 'mini'));
    expect(mocks.move).toHaveBeenCalledWith('source', '__local__', { dryRun: true, allowDirty: false, runtimeMode: 'terminal', sourceMachine: 'mini' });
  });
  it('does not call an inaccessible saved source an empty fleet or offer an enabled move', async () => {
    mocks.machines.mockResolvedValue([]);
    render(<ContinueElsewhereButton sessionId="source" label="New PM" />);
    fireEvent.click(screen.getByRole('button', { name: 'Move to another computer' }));
    await screen.findByRole('alert');
    expect(screen.queryByText('No saved machines yet.')).toBeNull();
    expect(screen.getByRole('button', { name: 'End here and review move' })).toBeDisabled();
    expect(mocks.kill).not.toHaveBeenCalled();
    expect(mocks.move).not.toHaveBeenCalled();
  });
});
