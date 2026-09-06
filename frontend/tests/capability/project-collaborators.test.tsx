import { describe, expect, it } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { groupCollaborators } from '../../src/lib/projectCollaborators';
import { ProjectCollaborators } from '../../src/components/ProjectCollaborators';
import { makeSession, installFakeDaemon, useFakeMachines } from './fake-daemon';
import type { ProjectView } from '../../src/api/sessionsd';

const mac = { id: 'mac', name: 'MacBook', host: 'localhost', port: 8787, isDefault: true };
const mini = { id: 'mini', name: 'Mac mini', host: 'mini.test', port: 8787, isDefault: false };
const project = (id: string, github?: string): ProjectView => ({ id, name: 'Sessions', implicit: true, roots: ['/work/Sessions'], github, session_ids: [id], live: 1, needs_input: 0 });

describe('project collaborators', () => {
  it('groups shared repositories across hosts but never guesses from folder names', () => {
    const snapshots = [mac, mini].map((server) => ({ server, error: null, sessions: [makeSession({ id: server.id, cwd: '/work/Sessions' })] }));
    expect(groupCollaborators(snapshots, {}, false, () => true)).toHaveLength(2);
    const shared = groupCollaborators(snapshots, { mac: [project('mac', 'Somewhere-Tech/Sessions')], mini: [project('mini', 'somewhere-tech/sessions')] }, false, () => true);
    expect(shared).toHaveLength(1);
    expect(shared[0].rows.map((row) => row.server.id).sort()).toEqual(['mac', 'mini']);
  });

  it('keeps helpers hidden and saved work separate without dropping records', () => {
    const snapshots = [{ server: mac, error: null, sessions: [
      makeSession({ id: 'manager', tags: { project: 'Sessions' } }),
      makeSession({ id: 'helper', kind: 'lane', creatorKind: 'session', parentSessionId: 'manager' }),
      makeSession({ id: 'closed', exited: true }),
      makeSession({ id: 'missing', runnerGone: true, unreachable: true })
    ] }];
    expect(groupCollaborators(snapshots, {}, false, () => true).flatMap((p) => p.rows.map((r) => r.session.id))).toEqual(['manager']);
    expect(groupCollaborators(snapshots, {}, true, () => true).flatMap((p) => p.rows.map((r) => r.session.id)).sort()).toEqual(['closed', 'missing']);
    expect(snapshots[0].sessions).toHaveLength(4);
  });

  it('renames on the owning computer even with another computer selected', async () => {
    const session = makeSession({ id: 'reviewer', name: 'hey' });
    const machines = [{ ...mac, sessions: [] }, { ...mini, sessions: [session] }];
    const daemon = installFakeDaemon(machines);
    useFakeMachines(machines, mac.id);
    const groups = groupCollaborators([{ server: mini, sessions: [session], error: null }], {}, false, () => true);
    render(<ProjectCollaborators groups={groups} activeMachineId={mac.id} renderLocal={() => null} onOpen={() => {}} />);
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Rename Hey' }));
    await user.clear(screen.getByRole('textbox', { name: 'Agent name' }));
    await user.type(screen.getByRole('textbox', { name: 'Agent name' }), 'Release reviewer');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(screen.getByText('Release reviewer')).toBeInTheDocument());
    expect(daemon.requests.filter((r) => r.method === 'PUT')).toHaveLength(1);
    expect(machines[1].sessions[0].name).toBe('Release reviewer');
  });
});
