import { describe, expect, it } from 'vitest';
import { eventsToMessages } from '../../src/lib/claudeEvents';
import type { StructuredSessionEvent } from '../../src/types';

function event(time: number, fields: Partial<StructuredSessionEvent>): StructuredSessionEvent {
  return { source: 'codex-app-server', type: 'codex', conversationId: 'conversation', turnId: 'turn',
    timestamp: `2026-09-30T12:00:${String(time).padStart(2, '0')}Z`, ...fields };
}

describe('capability: Codex history reconciliation', () => {
  it('keeps no-UUID user and imported-message ids stable when older history is prepended', () => {
    const recent = [event(10, { type: 'user', subtype: 'user_steer',
      message: { role: 'user', content: 'Repeat the check.' } }),
    event(11, { type: 'assistant', source: 'sessions-continuation',
      message: { role: 'assistant', content: 'An imported answer.' } })];
    const older = event(1, { type: 'user', message: { role: 'user', content: 'Older request.' } });
    const initial = eventsToMessages(recent);
    const expanded = eventsToMessages([older, ...recent]);
    expect(expanded.slice(1).map((message) => message.id)).toEqual(initial.map((message) => message.id));
    const reordered = Object.fromEntries(Object.entries(recent[0]!).reverse()) as StructuredSessionEvent;
    expect(eventsToMessages([reordered])[0]?.id).toBe(initial[0]?.id);
  });

  it('deduplicates exact overlapping replay without losing intentional repeated messages or deltas', () => {
    const user = event(1, { type: 'user', message: { role: 'user', content: 'Continue.' } });
    const delta = event(2, { subtype: 'agent_message_delta', itemId: 'answer', delta: 'yes ' });
    const repeatedUser = event(3, { type: 'user', message: { role: 'user', content: 'Continue.' } });
    const repeatedDelta = event(4, { subtype: 'agent_message_delta', itemId: 'answer', delta: 'yes ' });
    const messages = eventsToMessages([user, delta, JSON.parse(JSON.stringify(user)),
      JSON.parse(JSON.stringify(delta)), repeatedUser, repeatedDelta]);
    expect(messages.filter((message) => message.role === 'user')).toHaveLength(2);
    expect(messages.find((message) => message.role === 'assistant')?.content).toBe('yes yes');
    expect(eventsToMessages([user, Object.fromEntries(Object.entries(user).reverse()) as StructuredSessionEvent])).toHaveLength(1);
  });

  it('does not infer duplicate deltas when timestamp and UUID are unavailable', () => {
    const delta = event(1, { timestamp: undefined, subtype: 'agent_message_delta', itemId: 'answer', delta: 'ha' });
    expect(eventsToMessages([delta, { ...delta }])[0]?.content).toBe('haha');
  });

  it('completed snapshots supersede late replayed deltas and started snapshots', () => {
    const messages = eventsToMessages([
      event(1, { subtype: 'item_completed', item: {
        id: 'answer', type: 'agentMessage', phase: 'final_answer', text: 'Done.'
      } }),
      event(2, { subtype: 'agent_message_delta', itemId: 'answer', delta: 'Done.' }),
      event(3, { subtype: 'item_started', item: {
        id: 'answer', type: 'agentMessage', phase: 'commentary', text: 'Earlier partial answer'
      } }),
      event(4, { subtype: 'item_completed', item: {
        id: 'command', type: 'commandExecution', command: 'go test', status: 'completed', exitCode: 0
      } }),
      event(5, { subtype: 'item_started', item: {
        id: 'command', type: 'commandExecution', command: 'go test', status: 'inProgress'
      } })
    ]);
    expect(messages).toHaveLength(1);
    expect(messages[0]?.content).toBe('Done.');
    expect(messages[0]?.updates).toBeUndefined();
    expect(messages[0]?.toolCalls?.[0]?.status).toBe('completed');
    expect(messages[0]?.toolCalls?.[0]?.resultFull).toBe('exit code: 0');
  });

  it('does not let an older completed snapshot overwrite the latest completed item', () => {
    const snapshot = (time: number, text: string): StructuredSessionEvent => event(time, {
      subtype: 'item_completed', item: { id: 'answer', type: 'agentMessage', phase: 'final_answer', text }
    });
    const messages = eventsToMessages([snapshot(3, 'Complete answer.'), snapshot(1, 'Older answer.')]);
    expect(messages[0]?.content).toBe('Complete answer.');
    expect(messages[0]?.createdAt).toBe(Date.parse('2026-09-30T12:00:03Z'));
  });
});
