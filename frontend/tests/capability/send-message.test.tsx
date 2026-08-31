// CAPABILITY: a person can send their first message to a session and see that
// it went.
//
// The old coverage for this asserted that the string '\x1b[200~' appears in
// InputBar.tsx. That passes if the component throws on render. Here the real
// composer is mounted inside the real conversation view, wired to the real
// api/sessionsd.submitMessage, and the assertions are the two things a person
// can actually observe: their message is on screen, and the daemon received it.
import { describe, expect, it } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { RemoteView } from '../../src/components/RemoteView';
import { submitMessage, sendInput } from '../../src/api/sessionsd';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from './fake-daemon';

const SESSION_ID = 'live-session';

function localMachine(): FakeMachine {
  return {
    id: 'local',
    name: 'Fixture Mac',
    host: 'localhost',
    port: 8787,
    isDefault: true,
    sessions: [makeSession({ id: SESSION_ID, name: 'Release notes' })]
  };
}

const idleSidebar = {
  parserName: 'Claude',
  parserIcon: '🟠',
  isWorking: false,
  timer: '',
  tokens: '',
  context: '',
  finalElapsed: '',
  currentTask: '',
  checklist: []
};

describe('capability: send the first message', () => {
  it('delivers what the person typed and shows it in the conversation', async () => {
    const machine = localMachine();
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine]);
    const user = userEvent.setup();

    render(
      <RemoteView
        sessionId={SESSION_ID}
        events={[]}
        historyPending={false}
        sendConfirmed={(data) => sendInput(SESSION_ID, data)}
        submitMessage={(data) => submitMessage(SESSION_ID, data)}
        connected
        hasEarlierClaudeEvents={false}
        loadingEarlierClaudeEvents={false}
        onLoadEarlierClaudeEvents={() => {}}
        sidebar={idleSidebar}
        cwd="/Users/example/project"
        onOpenTerminal={() => {}}
        provider="claude-code"
      />
    );

    const composer = await screen.findByPlaceholderText(/Message Claude/);
    await user.type(composer, 'Summarise what changed since Friday');
    await user.keyboard('{Enter}');

    // The daemon received exactly the message, once — not the bracketed-paste
    // wrapper, not two copies from a retry.
    await waitFor(() => expect(daemon.delivered[SESSION_ID]).toEqual([
      'Summarise what changed since Friday'
    ]));

    // And the person can see their message in the conversation.
    expect(await screen.findByText('Summarise what changed since Friday')).toBeInTheDocument();
    // The composer is cleared, so a second Enter does not send it twice.
    expect(composer).toHaveValue('');
  });

  it('coalesces two same-tick send gestures into one delivery', async () => {
    const machine = { ...localMachine(), submitDelayMS: 50 };
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine]);

    render(
      <RemoteView
        sessionId={SESSION_ID}
        events={[]}
        historyPending={false}
        sendConfirmed={(data) => sendInput(SESSION_ID, data)}
        submitMessage={(data) => submitMessage(SESSION_ID, data)}
        connected
        hasEarlierClaudeEvents={false}
        loadingEarlierClaudeEvents={false}
        onLoadEarlierClaudeEvents={() => {}}
        sidebar={idleSidebar}
        cwd="/Users/example/project"
        onOpenTerminal={() => {}}
        provider="claude-code"
      />
    );

    const composer = await screen.findByPlaceholderText(/Message Claude/);
    fireEvent.change(composer, { target: { value: 'Send this once' } });
    fireEvent.keyDown(composer, { key: 'Enter' });
    fireEvent.keyDown(composer, { key: 'Enter' });

    await waitFor(() => expect(daemon.delivered[SESSION_ID]).toEqual(['Send this once']));
  });

  it('keeps a live session sendable while its display stream reconnects', async () => {
    const machine = localMachine();
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine]);
    const user = userEvent.setup();

    render(
      <RemoteView
        sessionId={SESSION_ID}
        events={[]}
        historyPending={false}
        sendConfirmed={(data) => sendInput(SESSION_ID, data)}
        submitMessage={(data) => submitMessage(SESSION_ID, data)}
        connected={false}
        sendAvailable
        hasEarlierClaudeEvents={false}
        loadingEarlierClaudeEvents={false}
        onLoadEarlierClaudeEvents={() => {}}
        sidebar={idleSidebar}
        cwd="/Users/example/project"
        onOpenTerminal={() => {}}
        provider="codex"
      />
    );

    const composer = await screen.findByPlaceholderText(/Message Codex/);
    expect(composer).toBeEnabled();
    await user.type(composer, 'List the connected external drives');
    await user.keyboard('{Enter}');

    await waitFor(() => expect(daemon.delivered[SESSION_ID]).toEqual([
      'List the connected external drives'
    ]));
  });
});
