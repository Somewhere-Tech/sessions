import { describe, expect, it } from 'vitest';
import { eventsToMessages } from '../../src/lib/claudeEvents';
import { uniqueCodexEvents } from '../../src/lib/codexEventIdentity';
import type { StructuredSessionEvent } from '../../src/types';

function event(time: number, fields: Partial<StructuredSessionEvent>): StructuredSessionEvent {
  return { source: 'codex-app-server', type: 'codex', conversationId: 'conversation', turnId: 'turn',
    timestamp: `2026-09-30T12:00:${String(time).padStart(2, '0')}Z`, ...fields };
}

describe('capability: Codex history reconciliation', () => {
  it('uses exact nested equality independent of key order, including unknown fields', () => {
    const first = event(1, { subtype: 'item_completed', item: {
      id: 'tool', type: 'mcpToolCall', result: { rows: [{ a: 1, b: null }, ['x', 'y']] }
    } });
    const reordered = event(1, { subtype: 'item_completed', item: {
      result: { rows: [{ b: null, a: 1 }, ['x', 'y']] }, type: 'mcpToolCall', id: 'tool'
    } });
    const changed = { ...reordered, detail: 'A new unknown notification field' };
    const reversedArray = event(1, { subtype: 'item_completed', item: {
      id: 'tool', type: 'mcpToolCall', result: { rows: [{ a: 1, b: null }, ['y', 'x']] }
    } });
    expect(uniqueCodexEvents([first, reordered, changed, reversedArray])).toEqual([first, changed, reversedArray]);
  });

  it('keeps distinct large outputs that collide in metadata buckets, but removes exact copies', () => {
    const output = 'tool output line\n'.repeat(3200);
    const first = event(1, { subtype: 'item_completed', item: {
      id: 'tool', type: 'commandExecution', aggregatedOutput: `${output}first`
    } });
    const second = event(1, { subtype: 'item_completed', item: {
      id: 'tool', type: 'commandExecution', aggregatedOutput: `${output}second`
    } });
    expect(uniqueCodexEvents([first, second, JSON.parse(JSON.stringify(first))])).toEqual([first, second]);
  });

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
    const user = event(1, { type: 'user', uuid: 'durable-user-1', message: { role: 'user', content: 'Continue.' } });
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

  it.each([undefined, '2026-09-30T12:00:01Z'])('preserves identical authored sends with distinct keys when UUID is absent (timestamp %s)', (timestamp) => {
    for (const role of ['user', 'assistant'] as const) {
      const message = event(1, { timestamp, type: role, provider: 'codex', source: role === 'assistant' ? 'sessions-continuation' : 'codex-app-server',
        message: { role, content: 'Continue.' } });
      const messages = eventsToMessages([message, { ...message }]);
      expect(messages).toHaveLength(2);
      expect(new Set(messages.map((value) => value.id)).size).toBe(2);
      expect(messages[1]?.id).toBe(`${messages[0]?.id}-occurrence-2`);
      expect(eventsToMessages([message])[0]?.id).toBe(messages[0]?.id);
    }
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

  it('gives repeated ambiguous steering messages distinct assistant segment keys too', () => {
    const steer = event(2, { type: 'user', subtype: 'user_steer', message: { role: 'user', content: 'Continue.' } });
    const answer = (time: number, id: string): StructuredSessionEvent => event(time, {
      subtype: 'item_completed', item: { id, type: 'agentMessage', phase: 'commentary', text: id }
    });
    const messages = eventsToMessages([answer(1, 'before'), steer, answer(3, 'middle'), { ...steer }, answer(4, 'after')]);
    expect(messages.map((message) => message.role)).toEqual(['assistant', 'user', 'user', 'assistant', 'assistant']);
    expect(new Set(messages.map((message) => message.id)).size).toBe(messages.length);
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
