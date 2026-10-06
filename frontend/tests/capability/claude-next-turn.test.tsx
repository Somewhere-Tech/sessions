import { describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { submitMessage } from '../../src/api/sessionsd/operations';
import { eventsToMessages } from '../../src/lib/claudeEvents';
import { InputBar } from '../../src/components/InputBar';
import { RemoteView } from '../../src/components/RemoteView';
import { useFakeMachines } from './fake-daemon';
import type { ClaudeSessionEvent } from '../../src/types';

describe('Claude next-turn acceptance', () => {
  it.each([false, true])('preserves a queued receipt after broken HTTP: %s', async (broken) => {
    useFakeMachines([{ id: 'local', name: 'Fixture', host: 'localhost', port: 8787, isDefault: true, sessions: [] }]);
    let operation = '';
    const fetch = vi.fn(async (_url: unknown, init?: RequestInit) => {
      if (init?.method === 'POST') operation = JSON.parse(String(init.body)).operation_id;
      if (init?.method === 'POST' && broken) return new Response('{', { status: 500 });
      return Response.json({ operation_id: operation, session_id: 'fixture', status: 'accepted', acceptance: 'queue', delivered: true, retry: false });
    });
    globalThis.fetch = fetch;
    await expect(submitMessage('fixture', 'follow-up', 'local')).resolves.toEqual({ queued: true });
    expect(fetch.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(1);
    expect(fetch).toHaveBeenCalledTimes(broken ? 2 : 1);
  });

  it('keeps a draft when an older busy runner refuses it', async () => {
    const user = userEvent.setup();
    const submit = vi.fn().mockRejectedValue(new Error('This runner cannot accept mid-turn messages'));
    render(<InputBar sessionId="old-runner" draftMachineId="local" provider="claude-code" richSession providerWorking connected
      send={async () => {}} submitMessage={submit} />);
    await user.type(screen.getByRole('textbox'), 'Do not lose this');
    await user.click(screen.getByRole('button', { name: 'Send' }));
    expect(submit).toHaveBeenCalledOnce();
    expect(screen.getByRole('textbox')).toHaveValue('Do not lose this');
    expect(screen.getByRole('alert')).toHaveTextContent('Your draft is still here');
  });

  it('keeps identical queued messages distinct until their own dispatch', () => {
    const queued = (id: string): ClaudeSessionEvent => ({ type: 'queue-operation', operation: 'enqueue', content: 'Continue', uuid: `sessions-message:${id}`, timestamp: '2026-10-06T10:00:00Z' });
    const user: ClaudeSessionEvent = { type: 'user', uuid: 'sessions-message:first', timestamp: '2026-10-06T10:00:01Z', message: { role: 'user', content: 'Continue' } };
    const messages = eventsToMessages([queued('first'), queued('second'), user]);
    expect(messages.filter((message) => message.pendingQueue)).toHaveLength(1);
    expect(messages.find((message) => message.pendingQueue)?.blockId).toBe('sessions-message:second');
    expect(messages.filter((message) => message.role === 'user' && !message.pendingQueue)).toHaveLength(1);
  });

  it('never labels a previously claimed message as queued or confirmed after restart', () => {
    const messages = eventsToMessages([
      { type: 'queue-operation', source: 'sessions-next-turn', operation: 'enqueue', content: 'Follow-up', uuid: 'sessions-message:uncertain' },
      { type: 'queue-operation', source: 'sessions-next-turn', operation: 'dispatch-unknown', operation_id: 'sessions-message:uncertain', content: 'Follow-up', uuid: 'sessions-message:uncertain:unknown' }
    ]);
    expect(messages).toHaveLength(1);
    expect(messages[0]?.pendingQueue).toBeUndefined();
    expect(messages[0]?.status).toBe('accepted');
    expect(messages[0]?.errorResponse).toContain('unknown');
  });

  it('reconciles an uncertain dispatch with a held local queue without showing it as pending', () => {
    useFakeMachines([{ id: 'local', name: 'Fixture', host: 'localhost', port: 8787, isDefault: true, sessions: [] }]);
    window.localStorage.setItem('sessions:dispatch:local:uncertain-chat', JSON.stringify([
      { id: 'local-send', role: 'user', content: 'Uncertain follow-up', status: 'queued', queued: true, createdAt: 1 }
    ]));
    render(<RemoteView sessionId="uncertain-chat" events={[
      { type: 'queue-operation', source: 'sessions-next-turn', operation: 'dispatch-unknown', operation_id: 'sessions-message:uncertain', uuid: 'sessions-message:uncertain:unknown', content: 'Uncertain follow-up' }
    ]} historyPending={false} connected sendConfirmed={async () => {}} submitMessage={async () => {}}
      hasEarlierClaudeEvents={false} loadingEarlierClaudeEvents={false} onLoadEarlierClaudeEvents={() => {}}
      sidebar={{ parserName: 'Claude', parserIcon: '', isWorking: false, timer: '', tokens: '', context: '', finalElapsed: '', currentTask: '', checklist: [] }}
      onOpenTerminal={() => {}} terminalAvailable={false} provider="claude-code" />);
    expect(screen.getAllByText('Uncertain follow-up')).toHaveLength(1);
    expect(screen.queryByText(/saved for Claude's next turn/)).not.toBeInTheDocument();
    expect(screen.getByRole('textbox')).toHaveValue('');
  });
});
