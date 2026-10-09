// CAPABILITY: a message sent from the composer to a terminal Claude session is
// shown once, as the person wrote it, after Claude records it.
//
// The daemon delivers every composer message to a terminal Claude session as a
// bracketed paste, and some Claude versions persist that user event inside a
// <pasted_content id="…"> envelope. The acknowledged send used to be matched
// against the envelope by exact text, so it never reconciled: the chat showed
// "Accepted · waiting for conversation" forever beside a second bubble that
// displayed the raw envelope.
import { beforeEach, describe, expect, it } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { RemoteView } from '../../src/components/RemoteView';
import { claudeAuthoredText, eventsToMessages } from '../../src/lib/claudeEvents';
import type { StructuredSessionEvent } from '../../src/types';
import { makeSession, useFakeMachines, type FakeMachine } from './fake-daemon';

const idleSidebar = {
  parserName: 'Claude',
  parserIcon: '✳',
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
    sessions: [makeSession({ id: sessionId, tool: 'claude-code' })]
  };
}

function claudeUser(content: unknown, occurrence: number, at = Date.now() + occurrence * 1000): StructuredSessionEvent {
  return {
    type: 'user',
    uuid: `claude-user-${occurrence}`,
    timestamp: new Date(at).toISOString(),
    message: { role: 'user', content }
  } as StructuredSessionEvent;
}

function claudeReply(text: string, occurrence: number, at: number): StructuredSessionEvent {
  return {
    type: 'assistant',
    uuid: `claude-reply-${occurrence}`,
    timestamp: new Date(at).toISOString(),
    message: { role: 'assistant', content: [{ type: 'text', text }] }
  } as StructuredSessionEvent;
}

const wrapped = (text: string, id = '7ab0'): string =>
  `<pasted_content id="${id}">\n${text}\n</pasted_content id="${id}">`;

function view(sessionId: string, events: StructuredSessionEvent[], submit: (data: string) => Promise<void>): JSX.Element {
  return (
    <RemoteView
      sessionId={sessionId}
      events={events}
      historyPending={false}
      sendConfirmed={async () => {}}
      submitMessage={submit}
      connected
      hasEarlierClaudeEvents={false}
      loadingEarlierClaudeEvents={false}
      onLoadEarlierClaudeEvents={() => {}}
      sidebar={idleSidebar}
      onOpenTerminal={() => {}}
      terminalAvailable
      provider="claude-code"
    />
  );
}

async function typeAndSend(content: string): Promise<void> {
  const user = userEvent.setup();
  const composer = await screen.findByRole('textbox');
  await user.type(composer, content);
  await user.keyboard('{Enter}');
}

function storedDispatch(sessionId: string): Array<{ content: string; status: string }> {
  return JSON.parse(window.localStorage.getItem(`sessions:dispatch:local:${sessionId}`) ?? '[]');
}

describe('capability: terminal Claude paste envelopes reconcile one-for-one', () => {
  beforeEach(() => window.localStorage.clear());

  it('reconciles an acknowledged send with its wrapped provider record and shows the authored text', async () => {
    const sessionId = 'pty-claude-paste';
    useFakeMachines([machine(sessionId)]);
    const submitted: string[] = [];
    const submit = async (data: string): Promise<void> => { submitted.push(data); };
    const rendered = render(view(sessionId, [], submit));

    await typeAndSend('Audit the paste path');
    expect(await screen.findByText(/Accepted · waiting for conversation/)).toBeInTheDocument();

    rendered.rerender(view(sessionId, [claudeUser(wrapped('Audit the paste path'), 1)], submit));
    await waitFor(() => expect(screen.getAllByText('Audit the paste path')).toHaveLength(1));
    expect(screen.queryByText(/Accepted · waiting for conversation/)).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain('pasted_content');
    expect(storedDispatch(sessionId)).toMatchObject([{ content: 'Audit the paste path', status: 'sent' }]);
    // Reconciliation is display only: the one bracketed-paste submit is all
    // that was sent.
    expect(submitted).toEqual(['\u001b[200~Audit the paste path\u001b[201~']);
  });

  it('keeps an acknowledged send waiting, without resending, while no provider record exists', async () => {
    const sessionId = 'pty-claude-paste-unconfirmed';
    useFakeMachines([machine(sessionId)]);
    const submitted: string[] = [];
    const submit = async (data: string): Promise<void> => { submitted.push(data); };
    const earlier = Date.now() - 60_000;
    const rendered = render(view(sessionId, [claudeUser(wrapped('Earlier request', 'a1'), 1, earlier)], submit));

    await typeAndSend('Later request');
    rendered.rerender(view(sessionId, [claudeUser(wrapped('Earlier request', 'a1'), 1, earlier)], submit));
    expect(await screen.findByText(/Accepted · waiting for conversation/)).toBeInTheDocument();
    expect(storedDispatch(sessionId)).toMatchObject([{ content: 'Later request', status: 'accepted' }]);
    expect(submitted).toEqual(['\u001b[200~Later request\u001b[201~']);
  });

  it('matches repeated identical pastes one-for-one and keeps chronological order', async () => {
    const sessionId = 'pty-claude-paste-repeat';
    useFakeMachines([machine(sessionId)]);
    const submit = async (): Promise<void> => {};
    const start = Date.now();
    const history = [
      claudeUser(wrapped('Continue', 'p1'), 1, start - 30_000),
      claudeReply('First answer', 1, start - 29_000)
    ];
    const rendered = render(view(sessionId, history, submit));
    await typeAndSend('Continue');
    expect(await screen.findAllByText('Continue')).toHaveLength(2);

    const confirmed = [...history, claudeUser(wrapped('Continue', 'p2'), 2, Date.now() + 1_000)];
    rendered.rerender(view(sessionId, confirmed, submit));
    await waitFor(() => expect(screen.queryByText(/Accepted · waiting for conversation/)).not.toBeInTheDocument());
    const order = Array.from(document.querySelectorAll('*'))
      .filter((element) => element.children.length === 0 && /^(Continue|First answer)$/.test(element.textContent ?? ''))
      .map((element) => element.textContent);
    expect(order).toEqual(['Continue', 'First answer', 'Continue']);
  });

  it('reads both recorded content shapes and both closing-tag forms', () => {
    const body = '# Start now\nline two with <b>markup</b>\nEND';
    for (const recorded of [
      wrapped(body),
      `<pasted_content id="7ab0">${body}</pasted_content>`,
      `<pasted_content>${body}</pasted_content>`
    ]) {
      for (const content of [recorded, [{ type: 'text', text: recorded }]]) {
        const [message] = eventsToMessages([claudeUser(content, 1)]);
        expect(message).toMatchObject({ role: 'user', content: body, status: 'sent' });
      }
    }
  });

  it('leaves anything other than one complete outer envelope exactly as recorded', () => {
    const body = 'keep me';
    for (const recorded of [
      `<pasted_content id="7ab0">${body}</pasted_content id="other">`,
      `<pasted_content id="7ab0">${body}`,
      `another request\n<pasted_content>${body}</pasted_content>`,
      `<pasted_content>${body}</pasted_content>\nextra`,
      '<pasted_content id="empty">\n</pasted_content id="empty">',
      `<pasted_content id="bad id">${body}</pasted_content id="bad id">`
    ]) {
      expect(claudeAuthoredText(recorded)).toBe(recorded);
    }
    // A person who pastes the envelope literally gets it back intact: Claude
    // wraps their paste once more and only that outer layer is removed.
    const literal = '<pasted_content id="literal">literal markup</pasted_content id="literal">';
    expect(claudeAuthoredText(wrapped(literal, 'outer'))).toBe(literal);
  });
});
