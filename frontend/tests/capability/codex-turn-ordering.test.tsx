import { describe, expect, it } from 'vitest';
import { eventsToMessages } from '../../src/lib/claudeEvents';
import type { StructuredSessionEvent } from '../../src/types';

const base = { source: 'codex-app-server', conversationId: 'hey', turnId: 'turn-1' } as const;

function event(timestamp: string, fields: Partial<StructuredSessionEvent>): StructuredSessionEvent {
  return { ...base, type: 'codex', timestamp, ...fields };
}

describe('capability: Codex final answers follow in-turn steering', () => {
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
    expect(messages[2]?.createdAt).toBe(Date.parse('2026-09-05T16:06:27Z'));
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
