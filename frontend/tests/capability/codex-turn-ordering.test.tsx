import { describe, expect, it } from 'vitest';
import { eventsToMessages } from '../../src/lib/claudeEvents';
import type { StructuredSessionEvent } from '../../src/types';

const base = { source: 'codex-app-server', conversationId: 'hey', turnId: 'turn-1' } as const;

function event(timestamp: string, fields: Partial<StructuredSessionEvent>): StructuredSessionEvent {
  return { ...base, type: 'codex', timestamp, ...fields };
}

describe('capability: Codex final answers follow in-turn steering', () => {
  it('keeps segment ids stable when unrelated older history is loaded or the turn starts outside the window', () => {
    const history = [
      event('2026-09-05T16:06:00Z', { subtype: 'turn_started' }),
      event('2026-09-05T16:06:01Z', { subtype: 'item_completed', item: {
        id: 'comment', type: 'agentMessage', phase: 'commentary', text: 'Checking.'
      } }),
      event('2026-09-05T16:06:02Z', { type: 'user', subtype: 'user_steer', uuid: 'steer-stable',
        message: { role: 'user', content: 'Check again.' } }),
      event('2026-09-05T16:06:03Z', { subtype: 'item_completed', item: {
        id: 'final', type: 'agentMessage', phase: 'final_answer', text: 'Checked.'
      } })
    ];
    const ids = (events: StructuredSessionEvent[]): string[] => eventsToMessages(events)
      .filter((message) => message.role === 'assistant').map((message) => message.id);
    expect(ids(history)).toEqual(['codex-turn-turn-1', 'codex-turn-turn-1-after-steer-stable']);
    expect(ids([event('2026-09-05T16:05:00Z', { type: 'user', uuid: 'earlier',
      message: { role: 'user', content: 'Older history.' } }), ...history])).toEqual(ids(history));
    expect(ids(history.slice(2))).toEqual(['codex-turn-turn-1-after-steer-stable']);
  });

  it('never moves a sealed segment after steering when a preexisting final item completes late', () => {
    const messages = eventsToMessages([
      event('2026-09-05T16:06:00Z', { subtype: 'turn_started' }),
      event('2026-09-05T16:06:01Z', { subtype: 'item_started', item: {
        id: 'late-final', type: 'agentMessage', phase: 'final_answer', text: ''
      } }),
      event('2026-09-05T16:06:02Z', { type: 'user', subtype: 'user_steer', uuid: 'steer',
        message: { role: 'user', content: 'Updated requirements.' } }),
      event('2026-09-05T16:06:03Z', { subtype: 'item_completed', item: {
        id: 'late-final', type: 'agentMessage', phase: 'final_answer', text: 'Original item completed.'
      } })
    ]);
    expect(messages.map((message) => message.role)).toEqual(['assistant', 'user']);
    expect(messages[0]?.content).toBe('Original item completed.');
    expect(messages[0]?.createdAt).toBe(Date.parse('2026-09-05T16:06:00Z'));
  });

  it('does not invent a persistent queue for accepted steering without a turn identity', () => {
    const history = [
      event('2026-09-05T16:06:00Z', { subtype: 'turn_started' }),
      event('2026-09-05T16:06:01Z', {
        subtype: 'user_steer', type: 'user', uuid: 'unbound-ack', turnId: undefined,
        message: { role: 'user', content: 'Use the revised requirements' }
      }),
      event('2026-09-05T16:06:02Z', { subtype: 'turn_completed', status: 'completed' })
    ];
    for (const events of [history.slice(0, 2), history, JSON.parse(JSON.stringify(history))]) {
      const messages = eventsToMessages(events).filter((message) => message.id === 'unbound-ack');
      expect(messages).toHaveLength(1);
      expect(messages[0]).toMatchObject({ status: 'sent', content: 'Use the revised requirements' });
      expect(messages[0]?.queued).toBeUndefined();
    }
  });

  it('resolves steering acknowledged after turn completion, including replay', () => {
    const history = [
      event('2026-09-05T16:06:00Z', { subtype: 'turn_started' }),
      event('2026-09-05T16:06:02Z', { subtype: 'turn_completed', status: 'completed' }),
      event('2026-09-05T16:06:03Z', {
        subtype: 'user_steer', type: 'user', uuid: 'late-ack',
        message: { role: 'user', content: 'Finish with the updated answer' }
      })
    ];
    for (const events of [history, JSON.parse(JSON.stringify(history))]) {
      const message = eventsToMessages(events).find((message) => message.id === 'late-ack');
      expect(message).toMatchObject({ status: 'sent', queued: false });
    }
  });

  it('keeps distinct final items and places their bubble after newer user questions', () => {
    const messages = eventsToMessages([
      event('2026-09-05T16:06:00Z', { subtype: 'turn_started' }),
      event('2026-09-05T16:06:10Z', {
        subtype: 'user_steer', type: 'user', uuid: 'question-1',
        message: { role: 'user', content: 'Did you finish the detailed review?' }
      }),
      event('2026-09-05T16:06:15Z', {
        subtype: 'user_steer', type: 'user', uuid: 'question-2',
        message: { role: 'user', content: 'Where did you write the review?' }
      }),
      event('2026-09-05T16:06:20Z', {
        subtype: 'item_completed', item: {
          id: 'answer-1', type: 'agentMessage', phase: 'final_answer',
          text: 'Yes. The detailed review is finished.'
        }
      }),
      event('2026-09-05T16:06:27Z', {
        subtype: 'item_completed', item: {
          id: 'answer-2', type: 'agentMessage', phase: 'final_answer',
          text: 'I saved it here: review.md'
        }
      }),
      event('2026-09-05T16:06:28Z', { subtype: 'turn_completed', status: 'completed' })
    ]);

    expect(messages.map((message) => message.role)).toEqual(['user', 'user', 'assistant']);
    expect(messages[2]?.content).toBe('Yes. The detailed review is finished.\n\nI saved it here: review.md');
    expect(messages[2]?.createdAt).toBe(Date.parse('2026-09-05T16:06:20Z'));
  });

  it('keeps commentary and tool activity before a follow-up, including replay and late tool completion', () => {
    const history = [
      event('2026-09-05T16:06:00Z', { subtype: 'turn_started' }),
      event('2026-09-05T16:06:01Z', { subtype: 'item_completed', item: {
        id: 'update', type: 'agentMessage', phase: 'commentary', text: 'Checking the original request.'
      } }),
      event('2026-09-05T16:06:02Z', { subtype: 'item_started', item: {
        id: 'tool', type: 'commandExecution', command: 'go test ./...', status: 'inProgress'
      } }),
      event('2026-09-05T16:06:03Z', { subtype: 'user_steer', type: 'user', uuid: 'followup',
        message: { role: 'user', content: 'Check the changed requirements too.' } }),
      event('2026-09-05T16:06:04Z', { subtype: 'item_completed', item: {
        id: 'tool', type: 'commandExecution', command: 'go test ./...', status: 'completed', exitCode: 0
      } }),
      event('2026-09-05T16:06:05Z', { subtype: 'item_completed', item: {
        id: 'answer', type: 'agentMessage', phase: 'final_answer', text: 'Both checks passed.'
      } }),
      event('2026-09-05T16:06:06Z', { subtype: 'turn_completed', status: 'completed' })
    ];
    for (const events of [history, JSON.parse(JSON.stringify(history))]) {
      const messages = eventsToMessages(events);
      expect(messages.map((message) => message.role)).toEqual(['assistant', 'user', 'assistant']);
      expect(messages[0]?.updates).toEqual(['Checking the original request.']);
      expect(messages[0]?.toolCalls?.[0]?.status).toBe('completed');
      expect(messages[0]?.streaming).toBe(false);
      expect(messages[1]?.queued).toBe(false);
      expect(messages[2]?.content).toBe('Both checks passed.');
    }
  });

  it('replaces streaming updates for one item instead of duplicating them', () => {
    const messages = eventsToMessages([
      event('2026-09-05T16:06:00Z', { subtype: 'turn_started' }),
      event('2026-09-05T16:06:01Z', {
        subtype: 'item_started', item: { id: 'answer-1', type: 'agentMessage', phase: 'final_answer', text: '' }
      }),
      event('2026-09-05T16:06:02Z', { subtype: 'agent_message_delta', itemId: 'answer-1', delta: 'I saved' }),
      event('2026-09-05T16:06:03Z', { subtype: 'agent_message_delta', itemId: 'answer-1', delta: ' it here.' }),
      event('2026-09-05T16:06:04Z', {
        subtype: 'item_completed', item: { id: 'answer-1', type: 'agentMessage', phase: 'final_answer', text: 'I saved it here.' }
      })
    ]);

    expect(messages).toHaveLength(1);
    expect(messages[0]?.content).toBe('I saved it here.');
    expect(messages[0]?.content.match(/I saved/g)).toHaveLength(1);
  });
});
