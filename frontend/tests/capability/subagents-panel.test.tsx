// CAPABILITY: delegated work recedes under its manager without becoming
// inaccessible. A person can open a helper or promote it to the main list from
// the manager's Subagents panel.
import { describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { SubagentsPanel } from '../../src/components/SubagentsPanel';
import { installFakeDaemon, makeSession, useFakeMachines } from './fake-daemon';

describe('capability: manage delegated work from its manager', () => {
  it('opens a helper and can promote it to a main session', async () => {
    const user = userEvent.setup();
    const onOpen = vi.fn();
    const onMakeMain = vi.fn().mockResolvedValue(undefined);
    const onEnd = vi.fn().mockResolvedValue(undefined);

    render(
      <SubagentsPanel
        manager={makeSession({ id: 'manager', name: 'Platform review' })}
        subagents={[
          makeSession({
            id: 'helper',
            name: 'Audit API routes',
            description: 'Map the public routes and authentication boundaries.',
            parentSessionId: 'manager',
            delegationKind: 'agent',
            working: true
          })
        ]}
        onClose={() => {}}
        onOpen={onOpen}
        onMakeMain={onMakeMain}
        onEnd={onEnd}
      />
    );

    expect(screen.getByText('Work delegated by')).toHaveTextContent('Platform review');
    expect(screen.getByText('Map the public routes and authentication boundaries.')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Open' }));
    expect(onOpen).toHaveBeenCalledWith('helper');

    await user.click(screen.getByRole('button', { name: 'Make main session' }));
    expect(onMakeMain).toHaveBeenCalledWith('helper');
    expect(onEnd).not.toHaveBeenCalled();
  });

  it('only ends an inactive subagent after an explicit review and confirmation', async () => {
    const user = userEvent.setup();
    const onEnd = vi.fn().mockResolvedValue(undefined);
    render(
      <SubagentsPanel
        manager={makeSession({ id: 'manager', name: 'Platform review' })}
        subagents={[makeSession({
          id: 'quiet',
          name: 'Old audit',
          createdAt: Date.now() - 26 * 60 * 60 * 1000,
          lastDataAt: Date.now() - 25 * 60 * 60 * 1000
        })]}
        onClose={() => {}}
        onOpen={() => {}}
        onMakeMain={async () => {}}
        onEnd={onEnd}
      />
    );

    expect(screen.getByText('Nothing is ended automatically. Review these suggestions or ask the manager to clean up its own subagents.')).toBeInTheDocument();
    expect(onEnd).not.toHaveBeenCalled();
    await user.click(screen.getByRole('button', { name: 'Review' }));
    await user.click(screen.getByRole('button', { name: 'End…' }));
    expect(onEnd).not.toHaveBeenCalled();
    await user.click(screen.getByRole('button', { name: 'End now' }));
    expect(onEnd).toHaveBeenCalledWith('quiet');
  });

  it('uses the daemon team summary and needs-you count when the route is available', async () => {
    const manager = makeSession({ id: 'manager', name: 'Platform review' });
    const helper = makeSession({
      id: 'helper',
      parentSessionId: manager.id,
      lastSummary: 'Older session-list summary'
    });
    const machine = {
      id: 'local', name: 'Fixture Mac', host: 'localhost', port: 8787, isDefault: true,
      sessions: [manager, helper],
      team: {
        self: { id: manager.id, tool: 'claude', relation: 'self' as const, depth: 0, state: 'working' as const, needs_you: false, working: true, exited: false },
        members: [{ id: helper.id, tool: 'codex', relation: 'child' as const, depth: 1, state: 'needs-you' as const, needs_you: true, working: false, exited: false, waiting: 'Choose the release target' }],
        needs_input: 1
      }
    };
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine]);

    render(<SubagentsPanel manager={manager} subagents={[helper]} onClose={() => {}} onOpen={() => {}} onMakeMain={async () => {}} onEnd={async () => {}} />);

    expect(await screen.findByText('Waiting: Choose the release target')).toHaveClass('is-attention');
    expect(screen.getByText(/1 need you/)).toBeInTheDocument();
    await waitFor(() => expect(daemon.requests.some((request) => request.path === '/api/lanes/mine' && new URL(request.url).searchParams.get('lane') === manager.id)).toBe(true));
  });

  it('keeps session-list status when an older daemon has no team route', async () => {
    const manager = makeSession({ id: 'manager' });
    const helper = makeSession({
      id: 'helper',
      idleReason: 'needs-input',
      idleDetail: 'Pick a client-side option'
    });
    const machine = { id: 'local', name: 'Fixture Mac', host: 'localhost', port: 8787, isDefault: true, sessions: [manager, helper] };
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine]);

    render(<SubagentsPanel manager={manager} subagents={[helper]} onClose={() => {}} onOpen={() => {}} onMakeMain={async () => {}} onEnd={async () => {}} />);

    expect(screen.getByText('Waiting: Pick a client-side option')).toHaveClass('is-attention');
    expect(screen.getByText(/1 need you/)).toBeInTheDocument();
    await waitFor(() => expect(daemon.unhandled.some((request) => request.path === '/api/lanes/mine')).toBe(true));
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('shows a team refresh failure while keeping the session-list fallback', async () => {
    const manager = makeSession({ id: 'manager' });
    const helper = makeSession({
      id: 'helper',
      idleReason: 'needs-input',
      idleDetail: 'Pick a client-side option'
    });
    const machine = {
      id: 'local', name: 'Fixture Mac', host: 'localhost', port: 8787, isDefault: true,
      sessions: [manager, helper], teamFailure: { status: 401, message: 'token required' }
    };
    installFakeDaemon([machine]);
    useFakeMachines([machine]);

    render(<SubagentsPanel manager={manager} subagents={[helper]} onClose={() => {}} onOpen={() => {}} onMakeMain={async () => {}} onEnd={async () => {}} />);

    expect(screen.getByText('Waiting: Pick a client-side option')).toHaveClass('is-attention');
    expect(await screen.findByRole('alert')).toHaveTextContent('Team details could not be refreshed. sessionsd: authentication required');
  });
});
