// CAPABILITY: each accepted user turn is shown exactly once, regardless of
// whether provider history or the submit receipt arrives first.
import { beforeEach, describe, expect, it } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { RemoteView } from '../../src/components/RemoteView';
import type { StructuredSessionEvent } from '../../src/types';
import { makeSession, useFakeMachines, type FakeMachine } from './fake-daemon';

const idleSidebar = {
  parserName: 'Codex',
  parserIcon: '⬛',
  isWorking: false,
  timer: '',
  tokens: '',
  context: '',
  finalElapsed: '',
  currentTask: '',
  checklist: []
};

function machine(sessionId: string): FakeMachine {
  return {
    id: 'local',
    name: 'Fixture Mac',
    host: 'localhost',
    port: 8787,
    isDefault: true,
    sessions: [makeSession({ id: sessionId })]
  };
}

function userEventRecord(content: string, occurrence: number): StructuredSessionEvent {
  return {
    type: 'user',
    source: 'codex-app-server',
    uuid: `user-${occurrence}`,
    timestamp: new Date(1_000 + occurrence).toISOString(),
    message: { role: 'user', content }
  };
}

function deferred(): { promise: Promise<void>; resolve: () => void } {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => { resolve = done; });
  return { promise, resolve };
}

function view(sessionId: string, events: StructuredSessionEvent[], submitMessage: (data: string) => Promise<void>): JSX.Element {
  return (
    <RemoteView
      sessionId={sessionId}
      events={events}
      historyPending={false}
      sendConfirmed={async () => {}}
      submitMessage={submitMessage}
      connected
      hasEarlierClaudeEvents={false}
      loadingEarlierClaudeEvents={false}
      onLoadEarlierClaudeEvents={() => {}}
      sidebar={idleSidebar}
      onOpenTerminal={() => {}}
      terminalAvailable={false}
      provider="codex"
    />
  );
}

async function typeAndSend(content: string): Promise<void> {
  const user = userEvent.setup();
  const composer = await screen.findByPlaceholderText(/Message Codex/);
  await user.type(composer, content);
  await user.keyboard('{Enter}');
}

describe('capability: dispatch ordering keeps one bubble per genuine turn', () => {
  beforeEach(() => window.localStorage.clear());

  it('does not duplicate a provider event that arrives before the submit receipt', async () => {
    const sessionId = 'event-before-receipt';
    useFakeMachines([machine(sessionId)]);
    const receipt = deferred();
    const rendered = render(view(sessionId, [], () => receipt.promise));

    await typeAndSend('Check release notes');
    rendered.rerender(view(sessionId, [userEventRecord('Check release notes', 1)], () => receipt.promise));

    receipt.resolve();
    await waitFor(() => expect(screen.getAllByText('Check release notes')).toHaveLength(1));
  });

  it('retains two genuine turns with identical text', async () => {
    const sessionId = 'identical-turns';
    useFakeMachines([machine(sessionId)]);
    const first = deferred();
    const second = deferred();
    let attempt = 0;
    const submit = (): Promise<void> => (++attempt === 1 ? first.promise : second.promise);
    const rendered = render(view(sessionId, [], submit));

    await typeAndSend('Continue');
    rendered.rerender(view(sessionId, [userEventRecord('Continue', 1)], submit));
    first.resolve();
    await waitFor(() => expect(screen.getAllByText('Continue')).toHaveLength(1));

    await typeAndSend('Continue');
    rendered.rerender(view(sessionId, [userEventRecord('Continue', 1), userEventRecord('Continue', 2)], submit));
    second.resolve();
    await waitFor(() => expect(screen.getAllByText('Continue')).toHaveLength(2));
  });

  it('reconciles provider history that arrives after the submit receipt', async () => {
    const sessionId = 'history-after-receipt';
    useFakeMachines([machine(sessionId)]);
    const rendered = render(view(sessionId, [], async () => {}));

    await typeAndSend('Inspect the package');
    expect(await screen.findAllByText('Inspect the package')).toHaveLength(1);

    rendered.rerender(view(sessionId, [userEventRecord('Inspect the package', 1)], async () => {}));
    await waitFor(() => expect(screen.getAllByText('Inspect the package')).toHaveLength(1));
  });

  it('reconciles an acknowledged message once after the view reloads', async () => {
    const sessionId = 'reload-after-receipt';
    useFakeMachines([machine(sessionId)]);
    const rendered = render(view(sessionId, [], async () => {}));
    await typeAndSend('Keep the active turn');
    expect(await screen.findAllByText('Keep the active turn')).toHaveLength(1);
    rendered.unmount();
    render(view(sessionId, [userEventRecord('Keep the active turn', 1)], async () => {}));
    await waitFor(() => expect(screen.getAllByText('Keep the active turn')).toHaveLength(1));
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });
});
