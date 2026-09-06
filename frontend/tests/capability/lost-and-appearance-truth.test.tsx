// CAPABILITY: unavailable provider conversations and provider-owned terminal
// choices say exactly what the person can do next.
import { describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { SessionView } from '../../src/components/SessionView';
import { effectiveSessionView, preferNextSessionView } from '../../src/lib/sessionViewPreference';
import { classifySnapshotComposerState } from '../../src/lib/detectMultiChoice';
import { classifySession } from '../../src/lib/sessionStatus';
import { useSessions } from '../../src/store/sessions';
import { Workbench } from './harness';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from './fake-daemon';

// Exact visible terminal text from docs/reviews/2026-09-03-ui-friction.md
// section 8. The prompt wraps at phone width and has neither a question mark
// nor the usual picker footer.
const CLAUDE_APPEARANCE_SNAPSHOT = `
Let's get started.

Choose the text style that looks best with
your terminal
To change this later, run /theme

  1. Auto (match terminal)
❯ 2. Dark mode ✔
`;

function localMachine(sessions: ReturnType<typeof makeSession>[]): FakeMachine {
  return {
    id: 'local',
    name: 'Fixture Mac',
    host: 'localhost',
    port: 8787,
    isDefault: true,
    sessions
  };
}

describe('capability: lost and waiting state truth', () => {
  it('keeps conversation-only sessions readable despite a requested terminal view', async () => {
    const session = makeSession({ id: 'rich-view', tool: 'codex', kind: 'codex-app-server' });
    const machine = localMachine([session]);
    installFakeDaemon([machine]);
    useFakeMachines([machine]);
    preferNextSessionView(session.id, 'terminal');
    const { container } = render(<SessionView sessionId={session.id} isActive />);
    await waitFor(() => expect(container.querySelector('.session-view')).toHaveClass('view-remote'));
    expect(screen.queryByText('No terminal')).not.toBeInTheDocument();
    expect(screen.queryByText('No terminal for this Rich session')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Conversation' })).toBeInTheDocument();
    expect(effectiveSessionView('terminal', true, false)).toBe('terminal');
    expect(effectiveSessionView('remote', false, false)).toBe('terminal');
  });

  it('shows one lost-conversation recovery card with consistent surrounding state', async () => {
    const lost = makeSession({
      id: 'lost-codex',
      name: 'Lost recovery demo',
      cmd: 'codex',
      tool: 'codex',
      kind: 'codex-app-server',
      conversationId: 'saved-codex-conversation'
    });
    const machine = localMachine([lost]);
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine]);
    // The daemon's next poll is what proves the runner gone. This transition
    // catches a reconciliation bug where connectivity-only changes reused the
    // old session object and never reached the view.
    lost.unreachable = true;
    lost.unreachableReason = 'runner-lost';
    lost.runnerGone = true;
    await useSessions.getState().refresh();
    const continueConversation = vi.fn();
    const user = userEvent.setup();

    const { container } = render(
      <Workbench>
        <SessionView sessionId={lost.id} isActive onContinueConversation={continueConversation} />
      </Workbench>
    );

    const card = await screen.findByRole('group', { name: 'Lost conversation recovery' });
    expect(screen.getAllByRole('group', { name: 'Lost conversation recovery' })).toHaveLength(1);
    expect(within(card).getByText('The runner is gone. Your Codex conversation is saved.')).toBeInTheDocument();
    const view = container.querySelector<HTMLElement>('.session-view');
    expect(view).not.toBeNull();
    expect(view!.querySelector('.session-live-pill')).toHaveTextContent('Not connected');
    expect(view!.querySelector('.sidebar-run-state')).toHaveTextContent('Not connected');
    expect(within(view!).queryByText('Terminal unavailable')).not.toBeInTheDocument();
    const composer = within(view!).getByRole('textbox');
    expect(composer).toBeDisabled();
    expect(composer).toHaveAttribute('placeholder', 'This session is not connected, so messages cannot be sent.');
    expect(within(view!).queryByText('Ready')).not.toBeInTheDocument();

    await user.click(within(card).getByRole('button', { name: 'Resume conversation…' }));
    expect(continueConversation).toHaveBeenCalledWith(expect.objectContaining({ id: lost.id }));
    expect(daemon.adopted).toEqual([]);

    await user.click(within(card).getByRole('button', { name: 'Close' }));
    await waitFor(() => expect(daemon.ended).toEqual([lost.id]));
  });

  it('maps the appearance snapshot to needs-you and keeps its question in the inbox', async () => {
    const composerState = classifySnapshotComposerState(CLAUDE_APPEARANCE_SNAPSHOT);
    expect(composerState).toEqual({
      kind: 'numbered-picker',
      title: 'Claude appearance choice is open',
      description: "Choose Claude's terminal appearance"
    });
    const waiting = makeSession({
      id: 'claude-appearance',
      name: 'Claude terminal setup',
      idleReason: 'needs-input',
      idleDetail: composerState.description
    });
    const machine = localMachine([waiting]);
    installFakeDaemon([machine]);
    useFakeMachines([machine]);

    expect(classifySession(waiting).label).toBe('Needs you');
    render(<Workbench />);

    expect(await screen.findByText("Choose Claude's terminal appearance")).toBeInTheDocument();
    expect(screen.queryByText('Finished')).not.toBeInTheDocument();
  });
});
