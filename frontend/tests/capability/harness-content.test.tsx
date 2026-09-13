// CAPABILITY: what the harness injected is not what the person said.
//
// The founder's screenshot, 11 September: a structured Claude session's
// transcript showed `<task-notification>` blocks, `<system-reminder>` blocks and
// a queue marker as his own chat bubbles — each with a timestamp and a copy
// button — while Claude's own Remote Control showed none of them. They are
// user-role records in the provider's JSONL, which is how Claude Code delivers
// a background task's result to the model, so a reader that trusts the role
// renders machinery as somebody's words.
import { describe, expect, it } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import { RemoteView } from '../../src/components/RemoteView';
import { eventsToMessages } from '../../src/lib/claudeEvents';
import { isHarnessOnly, splitHarnessContent, harnessDisplay } from '../../src/lib/harnessContent';
import { lastMessage, lastMessageLine } from '../../src/lib/lastMessage';
import { sendInput, submitMessage } from '../../src/api/sessionsd';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from './fake-daemon';
import type { ClaudeSessionEvent, SessionInfo } from '../../src/types';

const SESSION_ID = 'harness-session';
const AT = '2026-09-11T18:00:00Z';

const TASK_NOTIFICATION = [
  '<task-notification>',
  "Background task finished: Monitor 'Opus worker…'",
  'Exit code 0 · 4 minutes',
  '</task-notification>'
].join('\n');

const SYSTEM_REMINDER = [
  '<system-reminder>',
  'Codebase and user instructions are shown below. Be sure to adhere to these instructions.',
  '</system-reminder>'
].join('\n');

// The four shapes from the founder's session, in one transcript.
function fixtureEvents(): ClaudeSessionEvent[] {
  const user = (text: string, uuid: string, timestamp: string): ClaudeSessionEvent => ({
    type: 'user', uuid, timestamp, message: { role: 'user', content: text }
  } as ClaudeSessionEvent);
  return [
    user('Please review the release notes', 'u1', AT),
    user(TASK_NOTIFICATION, 'u2', '2026-09-11T18:01:00Z'),
    user(SYSTEM_REMINDER, 'u3', '2026-09-11T18:02:00Z'),
    user('[SYSTEM NOTIFICATION] Background task completed]', 'u4', '2026-09-11T18:03:00Z'),
    user(`Ship it when the gates pass\n\n${SYSTEM_REMINDER}`, 'u5', '2026-09-11T18:04:00Z'),
    {
      type: 'queue-operation', operation: 'enqueue', content: 'and then push the branch',
      timestamp: '2026-09-11T18:05:00Z'
    } as unknown as ClaudeSessionEvent
  ];
}

describe('capability: harness records are system events, not messages', () => {
  it('keeps every harness shape out of the person\'s bubbles', () => {
    const messages = eventsToMessages(fixtureEvents());
    const userMessages = messages.filter((message) => message.role === 'user');

    for (const message of userMessages) {
      expect(message.content, `user bubble ${message.id}`).not.toContain('<task-notification>');
      expect(message.content, `user bubble ${message.id}`).not.toContain('<system-reminder>');
      expect(message.content, `user bubble ${message.id}`).not.toContain('[SYSTEM NOTIFICATION');
    }

    // Three whole-entry injections became three quiet system events, each
    // saying what happened rather than reprinting the block.
    const events = messages.filter((message) => message.systemEvent);
    expect(events).toHaveLength(3);
    expect(events[0]!.systemEvent!.summary).toContain("Background task finished: Monitor 'Opus worker…'");
    expect(events[1]!.systemEvent!.summary).toBe('System reminder: Codebase and user instructions are shown below. Be sure to adhere to these instructions.');
    expect(events[2]!.systemEvent!.summary).toContain('System notification');
    // The block itself is kept, for whoever wants to read exactly what arrived.
    expect(events[0]!.systemEvent!.detail).toContain('<task-notification>');
    expect(events[0]!.role).not.toBe('user');
  });

  it('shows the person\'s words and folds the block that trailed them', () => {
    const messages = eventsToMessages(fixtureEvents());
    const mixed = messages.find((message) => message.id === 'u5');

    expect(mixed?.role).toBe('user');
    expect(mixed?.content).toBe('Ship it when the gates pass');
    expect(mixed?.systemNote?.summary).toContain('System reminder');
    expect(mixed?.systemNote?.detail).toContain('<system-reminder>');
  });

  it('leaves a person quoting a reminder alone', () => {
    const asking = 'why does <system-reminder> show up in my chat?';
    expect(isHarnessOnly(asking)).toBe(false);
    expect(harnessDisplay(asking, 'You said').speaker).toBe('You said');
    // The person's question keeps the words they typed around the block.
    expect(splitHarnessContent(asking).personText).toContain('why does');
  });
});

describe('capability: a queued send waits on the composer, not in the record', () => {
  it('is not a transcript entry and is shown beside the composer', async () => {
    const machine: FakeMachine = {
      id: 'local', name: 'Fixture Mac', host: 'localhost', port: 8787, isDefault: true,
      sessions: [makeSession({ id: SESSION_ID, name: 'Release notes' })]
    };
    installFakeDaemon([machine]);
    useFakeMachines([machine]);

    render(
      <RemoteView
        sessionId={SESSION_ID}
        events={fixtureEvents()}
        historyPending={false}
        sendConfirmed={(data) => sendInput(SESSION_ID, data)}
        submitMessage={(data) => submitMessage(SESSION_ID, data)}
        connected
        hasEarlierClaudeEvents={false}
        loadingEarlierClaudeEvents={false}
        onLoadEarlierClaudeEvents={() => {}}
        sidebar={{
          parserName: 'Claude', parserIcon: '🟠', isWorking: false, timer: '', tokens: '',
          context: '', finalElapsed: '', currentTask: '', checklist: [], status: ''
        } as never}
        cwd="/Users/example/project"
        onOpenTerminal={() => {}}
        provider="claude-code"
      />
    );

    // The person's own message is there…
    expect(await screen.findByText('Please review the release notes')).toBeInTheDocument();
    // …the harness blocks are one quiet line each, not bubbles…
    const events = await screen.findAllByText(/Background task|System reminder|System notification/);
    expect(events.length).toBeGreaterThanOrEqual(3);
    expect(screen.queryByText(/Codebase and user instructions are shown below/, {
      selector: '.remote-bubble-content'
    })).not.toBeInTheDocument();
    // …and the block the person's message trailed is folded under their words.
    const mixed = screen.getByText('Ship it when the gates pass').closest('.remote-bubble-user') as HTMLElement;
    expect(within(mixed).getByText(/System reminder/)).toBeInTheDocument();

    // The queued send is composer status, with the text the person is waiting
    // to have picked up. It is not a line in the record of what was said.
    await waitFor(() => {
      const status = screen.getByRole('status', { name: '' });
      expect(status.textContent).toMatch(/queued — Claude is finishing the previous turn/);
    });
    expect(screen.queryByText('and then push the branch', {
      selector: '.remote-bubble-content'
    })).not.toBeInTheDocument();
  }, 20_000);

  it('clears a harness-only queue only after provider history contains it', () => {
    const queued = (text: string, timestamp: string): ClaudeSessionEvent => ({
      type: 'queue-operation', operation: 'enqueue', content: text, timestamp
    } as unknown as ClaudeSessionEvent);
    const delivered = (text: string, uuid: string, timestamp: string): ClaudeSessionEvent => ({
      type: 'user', uuid, timestamp, message: { role: 'user', content: text }
    } as ClaudeSessionEvent);

    const pending = eventsToMessages([queued(TASK_NOTIFICATION, AT)]);
    expect(pending).toHaveLength(1);
    expect(pending[0]).toMatchObject({ pendingQueue: true, content: TASK_NOTIFICATION });

    const confirmed = eventsToMessages([
      queued(TASK_NOTIFICATION, AT),
      delivered(TASK_NOTIFICATION, 'notification-delivered', '2026-09-11T18:01:00Z')
    ]);
    expect(confirmed.some((message) => message.pendingQueue)).toBe(false);
    expect(confirmed).toHaveLength(1);
    expect(confirmed[0]!.systemEvent?.detail).toContain('<task-notification>');
  });

});

describe('capability: the inbox line never quotes machinery', () => {
  it('does not present a harness block as the agent\'s reply', () => {
    const session = Object.assign(makeSession({ id: 'inbox', tool: 'claude-code' }), {
      idleReason: 'completed', idleSince: Date.parse(AT),
      lastSummary: SYSTEM_REMINDER
    }) as SessionInfo;

    // Nothing was said, so the row says nothing rather than printing a block.
    expect(lastMessageLine(session)).toBe('Agent replied');
    expect(lastMessage(session).text).toBe('');
  });

  it('keeps a real summary that happens to trail a reminder', () => {
    const session = Object.assign(makeSession({ id: 'inbox-2', tool: 'claude-code' }), {
      idleReason: 'completed', idleSince: Date.parse(AT),
      lastSummary: `Release notes are ready.\n\n${SYSTEM_REMINDER}`
    }) as SessionInfo;

    expect(lastMessageLine(session)).toBe('Agent: Release notes are ready.');
  });
});
