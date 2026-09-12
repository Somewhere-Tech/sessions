// CAPABILITY: Fork reviews the paid runtime and exact copy boundary before create.
import { useState } from 'react';
import { describe, expect, it } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ForkConfirmationDialog, type ForkPoint } from '../../src/components/ForkConfirmationDialog';
import { SessionHistoryView } from '../../src/components/SessionHistoryView';
import type { SessionInfo } from '../../src/types';
import { Workbench } from './harness';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from './fake-daemon';

function forkMachine(session: SessionInfo): FakeMachine {
  return {
    id: 'local', name: 'Fixture Mac', host: 'localhost', port: 8787, isDefault: true,
    sessions: [session],
    history: [{
      id: session.id, name: session.name ?? 'Release review', tool: 'claude',
      provider_session_id: session.claudeSessionId, cwd: session.cwd, machine: 'Fixture Mac',
      created_at: session.createdAt, last_activity_at: session.lastDataAt,
      message_count: 3, conversation_available: true
    }],
    transcripts: {
      [session.id]: [
        { index: 0, id: 'user-0', role: 'user', text: 'Check the release.', timestamp: null },
        { index: 1, id: 'agent-1', role: 'assistant', text: 'The checks are green.', timestamp: null },
        { index: 2, id: 'user-2', role: 'user', text: 'Now make a copy.', timestamp: null }
      ]
    },
    providers: [{
      id: 'claude', name: 'Claude Code', installed: true, updateAvailable: false,
      models: [{
        id: 'claude-fable-5', displayName: 'Fable 5', description: 'Everyday work', hidden: false,
        isDefault: true, defaultReasoningEffort: 'medium',
        supportedReasoningEfforts: [{ reasoningEffort: 'medium', description: 'Medium' }]
      }]
    }, { id: 'codex', name: 'Codex', installed: true, updateAvailable: false, models: [] }]
  };
}

describe('capability: confirm a conversation fork', () => {
  it('starts light, generates only on request, and sends one reviewed briefing with the chosen account', async () => {
    const session = makeSession({ id: 'brief-source', name: 'Manager', tool: 'claude-code', cmd: 'claude', kind: 'claude-structured' });
    const machine = forkMachine(session);
    machine.profiles = [{ name: 'second-account', tool: 'claude' }];
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine]);
    const user = userEvent.setup();
    render(<Workbench><ForkConfirmationDialog session={session} destinationProvider="claude" onClose={() => {}} onStarted={() => {}} /></Workbench>);
    const start = await screen.findByRole('button', { name: 'Start Claude (Fable 5)' });
    expect(start).toBeDisabled();
    expect(daemon.requests.some((r) => r.path.includes('/transcript') || r.path === '/api/recovery/briefing')).toBe(false);
    await user.click(screen.getByRole('button', { name: 'Generate draft briefing' }));
    const briefing = await screen.findByRole('textbox', { name: 'Briefing' });
    await waitFor(() => expect(briefing).toHaveValue('Review the release. Preserve the original conversation. Verify the tests.'));
    await user.click(screen.getByText('Account', { selector: 'summary' }));
    await user.selectOptions(screen.getByRole('combobox', { name: 'Destination account' }), 'second-account');
    await user.type(screen.getByRole('textbox', { name: 'Agent name' }), 'Release reviewer');
    await user.dblClick(start);
    const calls = daemon.requests.filter((r) => r.path === '/api/recovery/collaborator');
    expect(calls).toHaveLength(1);
    expect(calls[0].body).toMatchObject({ contextMode: 'briefing', profile: 'second-account', name: 'Release reviewer', briefing: 'Review the release. Preserve the original conversation. Verify the tests.' });
    expect(daemon.ended).toEqual([]);
  });

  it('opens the shared plan at an exact fork point before the fork call', async () => {
    const session = makeSession({
      id: 'source-session', name: 'Release review', tool: 'claude-code',
      cmd: 'claude', kind: 'claude-structured', claudeSessionId: 'provider-1'
    });
    const machine = forkMachine(session);
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine]);
    const user = userEvent.setup();

    function ForkFlow(): JSX.Element {
      const [request, setRequest] = useState<{ destination: 'claude' | 'codex'; point?: ForkPoint } | null>(null);
      return <Workbench>
        <SessionHistoryView
          session={session}
          onFork={async (_source, destination, point) => setRequest({ destination, point })}
        />
        {request ? <ForkConfirmationDialog
          session={session}
          destinationProvider={request.destination}
          point={request.point}
          onClose={() => setRequest(null)}
          onStarted={() => {}}
        /> : null}
      </Workbench>;
    }
    render(<ForkFlow />);

    await user.click(screen.getByRole('button', { name: 'Fork' }));
    const forkPoints = await screen.findAllByRole('button', { name: 'Fork here' });
    await user.click(forkPoints[1]);
    await user.click(screen.getByRole('button', { name: 'Fork in Claude' }));
    expect(screen.getByRole('radio', { name: /Briefing Start light/ })).toBeChecked();
    await user.click(screen.getByRole('radio', { name: /Conversation copy/ }));

    const plan = await screen.findByRole('group', { name: 'Start plan' });
    expect(plan).toHaveTextContent('Fable 5');
    expect(screen.getByText('Permissions: Ask me')).toBeInTheDocument();
    expect(await screen.findByText('2 messages, about 10 tokens')).toBeInTheDocument();
    expect(screen.getByText(/Tool output, file changes, attachments, sign-in details/)).toBeInTheDocument();
    expect(screen.getByText('The new agent starts only when you press Start.')).toBeInTheDocument();
    expect(daemon.requests.filter((request) => request.path === '/api/recovery/fork')).toHaveLength(0);

    await user.click(screen.getByRole('button', { name: 'Start Claude (Fable 5)' }));
    await waitFor(() => expect(daemon.requests.filter((request) => request.path === '/api/recovery/collaborator')).toHaveLength(1));
    expect(daemon.requests.find((request) => request.path === '/api/recovery/collaborator')?.body).toMatchObject({
      contextMode: 'conversation', profile: '',
      sourceSessionId: session.id,
      destinationProvider: 'claude',
      sourceMessageIndex: 1,
      sourceMessageId: 'agent-1',
      model: 'claude-fable-5',
      effort: 'medium',
      permissions: 'constrained'
    });
  });
});
