// CAPABILITY: the left navigator is a person's main-session list.
//
// Agent-created helpers are managed from the selected manager's Subagents
// panel. They must not leak back into Pinned, Live, Ended, search, or an
// expandable manager tree. Promoting a helper is the explicit action that
// makes it a main session and therefore visible here.
import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import { Workbench } from './harness';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from './fake-daemon';

function localMachine(): FakeMachine {
  return {
    id: 'local',
    name: 'Fixture Mac',
    host: 'localhost',
    port: 8787,
    isDefault: true,
    sessions: [
      makeSession({ id: 'manager', name: 'Platform manager', pinned: true }),
      makeSession({
        id: 'helper',
        name: 'Audit API routes',
        parentSessionId: 'manager',
        creatorKind: 'session',
        delegationKind: 'agent',
        kind: 'lane'
      }),
      makeSession({
        id: 'promoted',
        name: 'Promoted review',
        parentSessionId: 'manager',
        displayParentSessionId: '',
        creatorKind: 'session',
        delegationKind: 'agent',
        kind: 'lane'
      })
    ]
  };
}

describe('capability: keep subagents out of the main navigator', () => {
  it('shows managers and promoted work without rendering helper rows or per-session chevrons', async () => {
    const machine = localMachine();
    installFakeDaemon([machine]);
    useFakeMachines([machine]);

    render(<Workbench />);

    expect(await screen.findByText('Platform manager')).toBeInTheDocument();
    expect(await screen.findByText('Promoted review')).toBeInTheDocument();
    expect(screen.queryByText('Audit API routes')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Expand Platform manager/i })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Collapse Platform manager/i })).not.toBeInTheDocument();
  });

  it('shows project refresh failures without hiding the fallback inbox', async () => {
    const machine = localMachine();
    machine.projectFailure = { status: 503, message: 'project index unavailable' };
    installFakeDaemon([machine]);
    useFakeMachines([machine]);

    render(<Workbench />);

    expect(await screen.findByText('Platform manager')).toBeInTheDocument();
    expect(await screen.findByRole('status')).toHaveTextContent('Project grouping could not be refreshed. Sessions are shown together. sessionsd 503');
    expect(screen.getByText('Other projects')).toBeInTheDocument();
  });
});
