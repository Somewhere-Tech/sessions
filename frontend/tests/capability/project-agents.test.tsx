import { describe, expect, it } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { groupAgents } from '../../src/lib/projectAgents';
import { ProjectAgents } from '../../src/components/ProjectAgents';
import { makeSession, installFakeDaemon, useFakeMachines } from './fake-daemon';
import type { ProjectView } from '../../src/api/sessionsd';
import { projectPreview } from '../../src/lib/projectPreview';
import { InboxSections } from '../../src/components/InboxSections';
import type { InboxLayout } from '../../src/lib/inboxSections';

const mac = { id: 'mac', name: 'MacBook', host: 'localhost', port: 8787, isDefault: true };
const mini = { id: 'mini', name: 'Mac mini', host: 'mini.test', port: 8787, isDefault: false };
const project = (id: string, github?: string): ProjectView => ({ id, name: 'Sessions', implicit: true, roots: ['/work/Sessions'], github, session_ids: [id], live: 1, needs_input: 0 });

describe('project agents', () => {
  it('previews three ordinary rows without reordering or changing records', () => {
    const sessions = Array.from({ length: 12 }, (_, index) => makeSession({ id: `plain-${index}` }));
    const before = JSON.stringify(sessions);
    expect(projectPreview(sessions, (session) => session, () => false)).toEqual(sessions.slice(0, 3));
    expect(JSON.stringify(sessions)).toBe(before);
    expect(projectPreview([], (session) => session, () => false)).toEqual([]);
  });

  it('keeps every attention, pinned, and selected row beyond the preview cap', () => {
    const sessions = [makeSession({ id: 'ordinary' }), ...[
      makeSession({ id: 'working', working: true }),
      makeSession({ id: 'waiting', idleReason: 'needs-input' }),
      makeSession({ id: 'pinned', pinned: true }),
      makeSession({ id: 'selected' }),
      makeSession({ id: 'also-working', working: true })
    ]];
    expect(projectPreview(sessions, (session) => session, (session) => session.id === 'selected').map((session) => session.id))
      .toEqual(['working', 'waiting', 'pinned', 'selected', 'also-working']);
  });

  it('counts every fleet agent beside Add and independently reveals and collapses groups', async () => {
    const sessions = Array.from({ length: 20 }, (_, index) => makeSession({ id: `fleet-${index}`, name: `Agent ${index}` }));
    const rows = sessions.map((session) => ({ session, server: mini, unavailable: false }));
    render(<ProjectAgents groups={[{ id: 'one', name: 'Sessions', rows }, { id: 'two', name: 'Website', rows: rows.slice(0, 1) }]}
      activeMachineId={mac.id} renderLocal={() => null} onOpen={() => {}} onAdd={() => {}} />);
    const group = screen.getByRole('region', { name: 'Sessions' });
    const user = userEvent.setup();
    expect(within(group).getByRole('button', { name: 'Sessions 20' })).toHaveAttribute('aria-expanded', 'true');
    expect(within(group).getByRole('button', { name: 'Add agent to Sessions' })).toBeInTheDocument();
    expect(within(group).getAllByRole('button', { name: /^Rename/ })).toHaveLength(3);
    await user.click(within(group).getByRole('button', { name: 'Show 17 remaining agents' }));
    expect(within(group).getAllByRole('button', { name: /^Rename/ })).toHaveLength(20);
    await user.click(within(group).getByRole('button', { name: 'Show fewer agents' }));
    const disclosure = within(group).getByRole('button', { name: 'Sessions 20' });
    disclosure.focus();
    await user.keyboard('{Enter}');
    expect(disclosure).toHaveAttribute('aria-expanded', 'false');
    expect(within(group).queryByRole('button', { name: /^Rename/ })).not.toBeInTheDocument();
    expect(within(screen.getByRole('region', { name: 'Website' })).getByRole('button', { name: 'Rename Agent 0' })).toBeInTheDocument();
    await user.keyboard(' ');
    expect(within(group).getAllByRole('button', { name: /^Rename/ })).toHaveLength(3);
  });

  it('keeps local project previews bounded and their selected conversation visible', async () => {
    const live = Array.from({ length: 12 }, (_, index) => makeSession({ id: `local-${index}`, name: `Local ${index}` }));
    const layout: InboxLayout = { needsYou: [], moreNeedsYou: 0, providerTrouble: [], other: null, flat: null,
      sections: [{ id: 'sessions', name: 'Sessions', implicit: false, live, notConnected: [], finished: [], needsYou: 0, updatedAt: 1 }] };
    render(<InboxSections layout={layout} activeSessionId="local-11" renderNode={(session) => <div key={session.id}>{session.name}</div>}
      onOpen={() => {}} onShowAllNeedsYou={() => {}} folderOf={() => ''} relativeTime={() => ''} lastActivity={() => 1} />);
    expect(screen.getByText('Local 11')).toBeInTheDocument();
    expect(screen.queryByText('Local 2')).not.toBeInTheDocument();
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Show 9 remaining agents' }));
    expect(screen.getByText('Local 2')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /Sessions 12/ })).toHaveAttribute('aria-expanded', 'true');
    await user.click(screen.getByRole('button', { name: /Sessions 12/ }));
    expect(screen.queryByText('Local 11')).not.toBeInTheDocument();
  });

  it('groups shared repositories across hosts but never guesses from folder names', () => {
    const snapshots = [mac, mini].map((server) => ({ server, error: null, sessions: [makeSession({ id: server.id, cwd: '/work/Sessions' })] }));
    expect(groupAgents(snapshots, {}, false, () => true)).toHaveLength(2);
    const shared = groupAgents(snapshots, { mac: [project('mac', 'Somewhere-Tech/Sessions')], mini: [project('mini', 'somewhere-tech/sessions')] }, false, () => true);
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
    expect(groupAgents(snapshots, {}, false, () => true).flatMap((p) => p.rows.map((r) => r.session.id))).toEqual(['manager']);
    expect(groupAgents(snapshots, {}, true, () => true).flatMap((p) => p.rows.map((r) => r.session.id)).sort()).toEqual(['closed', 'missing']);
    expect(snapshots[0].sessions).toHaveLength(4);
  });

  it('renames on the owning computer even with another computer selected', async () => {
    const session = makeSession({ id: 'reviewer', name: 'hey' });
    const machines = [{ ...mac, sessions: [] }, { ...mini, sessions: [session] }];
    const daemon = installFakeDaemon(machines);
    useFakeMachines(machines, mac.id);
    const groups = groupAgents([{ server: mini, sessions: [session], error: null }], {}, false, () => true);
    render(<ProjectAgents groups={groups} activeMachineId={mac.id} renderLocal={() => null} onOpen={() => {}} />);
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
