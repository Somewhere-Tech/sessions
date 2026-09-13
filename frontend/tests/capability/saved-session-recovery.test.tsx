// CAPABILITY: a saved session offers one exact recovery action on its own host.
// Browsing remains read-only; only the explicit Resume button may continue it.
import { describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ProjectAgents } from '../../src/components/ProjectAgents';
import { SessionView } from '../../src/components/SessionView';
import '../../src/styles/globals.css';
import { useExactResume } from '../../src/hooks/useExactResume';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from './fake-daemon';
import type { AgentProject } from '../../src/lib/projectAgents';

function savedGroup(options: { resumable?: boolean; unavailable?: boolean } = {}): AgentProject[] {
  const session = makeSession({
    id: 'paused-on-mini', name: 'Release handoff', tool: options.resumable === false ? 'terminal' : 'codex'
  });
  session.runnerGone = true;
  session.unreachable = true;
  session.unreachableReason = 'restart-restore-pending';
  session.lostReason = 'machine rebooted';
  session.conversationId = options.resumable === false ? undefined : 'provider-conversation-exact';
  return [{
    id: 'project:release', name: 'Release', rows: [{
      session,
      server: { id: 'paired-mini', name: 'Paired Mini', host: 'mini.local', port: 8787, isDefault: false },
      unavailable: Boolean(options.unavailable)
    }]
  }];
}

function SavedRows({ groups, onOpen, onResume }: {
  groups: AgentProject[];
  onOpen: (serverId: string, sessionId: string) => void;
  onResume: (row: AgentProject['rows'][number]) => void;
}): JSX.Element {
  return <ProjectAgents
    groups={groups}
    activeMachineId="local"
    renderLocal={() => null}
    onOpen={onOpen}
    onResume={onResume}
  />;
}

describe('capability: recently closed recovery stays exact and explicit', () => {
  it('targets the paired host and exact selected session only on Resume', async () => {
    const open = vi.fn();
    const resume = vi.fn();
    render(<SavedRows groups={savedGroup()} onOpen={open} onResume={resume} />);

    expect(await screen.findByText(/this machine restarted/i)).toBeInTheDocument();
    expect(resume).not.toHaveBeenCalled();

    // Reading the saved row is inert with respect to recovery.
    await userEvent.setup().click(screen.getByRole('button', { name: /^Release handoff Needs recovery/ }));
    expect(open).toHaveBeenCalledWith('paired-mini', 'paused-on-mini');
    expect(resume).not.toHaveBeenCalled();

    await userEvent.setup().click(screen.getByRole('button', { name: /^Resume/ }));
    expect(resume).toHaveBeenCalledWith(expect.objectContaining({
      server: expect.objectContaining({ id: 'paired-mini' }),
      session: expect.objectContaining({ id: 'paused-on-mini', conversationId: 'provider-conversation-exact' })
    }));
    const resumeButton = screen.getByRole('button', { name: /^Resume/ });
    expect(resumeButton).toBeVisible();
    expect(resumeButton).not.toHaveClass('session-row-continue');
    expect(getComputedStyle(resumeButton).minHeight).toBe('44px');
  });

  it('shows an honest fallback for unavailable hosts and sessions without provider history', async () => {
    const { rerender } = render(<SavedRows groups={savedGroup({ unavailable: true })} onOpen={() => {}} onResume={() => {}} />);
    expect(screen.queryByRole('button', { name: /^Resume/ })).not.toBeInTheDocument();
    expect(await screen.findByText('Reconnect this computer to resume.')).toBeInTheDocument();

    rerender(<SavedRows groups={savedGroup({ resumable: false })} onOpen={() => {}} onResume={() => {}} />);
    expect(screen.queryByRole('button', { name: /^Resume/ })).not.toBeInTheDocument();
    expect(await screen.findByText('No resumable provider conversation is available.')).toBeInTheDocument();
  });

  it('explains when the conversation already continued elsewhere', async () => {
    const groups = savedGroup({ resumable: false });
    groups[0]!.rows[0]!.session.reopenedAs = 'new-session';
    render(<SavedRows groups={groups} onOpen={() => {}} onResume={() => {}} />);
    expect(await screen.findByText(/Continued elsewhere/)).toBeInTheDocument();
    expect(screen.queryByText(/No resumable provider conversation/)).not.toBeInTheDocument();
  });

  it.each([
    ['Paused', { unreachable: true, unreachableReason: 'restart-restore-pending' }],
    ['Lost', { runnerGone: true, lostReason: 'runner disappeared' }]
  ] as const)('renders a non-exited %s session as read-only history without live transport', async (label, state) => {
    const session = makeSession({ id: `saved-${label.toLowerCase()}`, name: `${label} work`, tool: 'codex', ...state });
    const machine: FakeMachine = { id: 'local', name: 'This Mac', host: 'localhost', port: 8787, isDefault: true, sessions: [session] };
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine], 'local');
    let socketCount = 0;
    globalThis.WebSocket = class {
      constructor() {
        socketCount += 1;
        throw new Error('saved history must not open live transport');
      }
    } as unknown as typeof WebSocket;

    render(<SessionView sessionId={session.id} isActive />);

    expect(await screen.findByText(label)).toBeInTheDocument();
    expect(await screen.findByText(/Read-only history/)).toBeInTheDocument();
    expect(screen.getByText(/Viewing does not resume or send anything/)).toBeInTheDocument();
    expect(socketCount).toBe(0);
    expect(daemon.requests.every((request) => request.method === 'GET')).toBe(true);
  });

  it('keeps a paired-host resume error visible', async () => {
    const local: FakeMachine = { id: 'local', name: 'This Mac', host: 'localhost', port: 8787, isDefault: true, sessions: [] };
    const paired: FakeMachine = { id: 'paired-mini', name: 'Paired Mini', host: 'mini.local', port: 8787, isDefault: false, sessions: [] };
    installFakeDaemon([local, paired]);
    useFakeMachines([local, paired], 'local');
    const saved = savedGroup()[0]!.rows[0]!.session;

    function ErrorFlow(): JSX.Element {
      const { resume, notice } = useExactResume(() => {});
      return <><button type="button" onClick={() => { void resume(saved, 'missing-paired-host'); }}>Resume exact</button>{notice ? <p role="alert">{notice}</p> : null}</>;
    }
    render(<ErrorFlow />);
    await userEvent.setup().click(screen.getByRole('button', { name: 'Resume exact' }));
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(/no longer configured/i));
    expect(screen.getByRole('alert')).toBeInTheDocument();
  });
});
