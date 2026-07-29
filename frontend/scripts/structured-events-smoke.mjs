import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { build } from 'esbuild';

const work = await mkdtemp(join(tmpdir(), 'sessions-structured-events-'));
const output = join(work, 'events.mjs');

try {
  await build({
    entryPoints: [fileURLToPath(new URL('../src/lib/claudeEvents.ts', import.meta.url))],
    outfile: output,
    bundle: true,
    platform: 'node',
    format: 'esm',
    logLevel: 'silent'
  });
  const { eventsToMessages } = await import(pathToFileURL(output).href);

  const base = { source: 'codex-app-server', conversationId: 'thread-1' };
  const events = [
    {
      ...base,
      type: 'user',
      uuid: 'user-1',
      timestamp: '2026-07-20T10:00:00Z',
      author: { kind: 'session', id: 'lane-1', name: 'PM Claude', client: 'sessions-cli' },
      message: { role: 'user', content: 'Build the GUI' }
    },
    { ...base, type: 'codex', subtype: 'turn_started', timestamp: '2026-07-20T10:00:01Z' },
    {
      ...base,
      type: 'codex',
      subtype: 'plan_updated',
      turnId: 'turn-1',
      plan: [
        { step: 'Inspect', status: 'completed' },
        { step: 'Build', status: 'inProgress' }
      ],
      explanation: 'Keep the existing runtime.'
    },
    {
      ...base,
      type: 'codex',
      subtype: 'item_started',
      turnId: 'turn-1',
      item: {
        id: 'command-1',
        type: 'commandExecution',
        command: 'go test ./...',
        cwd: '/repo',
        status: 'inProgress'
      }
    },
    {
      ...base,
      type: 'codex',
      subtype: 'item_completed',
      turnId: 'turn-1',
      item: {
        id: 'command-1',
        type: 'commandExecution',
        command: 'go test ./...',
        cwd: '/repo',
        status: 'completed',
        aggregatedOutput: 'ok\n',
        exitCode: 0,
        durationMs: 120
      }
    },
    {
      ...base,
      type: 'codex',
      subtype: 'item_completed',
      turnId: 'turn-1',
      item: {
        id: 'reasoning-1',
        type: 'reasoning',
        summary: ['Reuse the normalized event boundary.']
      }
    },
    {
      ...base,
      type: 'codex',
      subtype: 'item_started',
      turnId: 'turn-1',
      item: { id: 'commentary-1', type: 'agentMessage', text: '', phase: 'commentary' }
    },
    {
      ...base,
      type: 'codex',
      subtype: 'agent_message_delta',
      turnId: 'turn-1',
      itemId: 'commentary-1',
      delta: 'Backend is ready.'
    },
    {
      ...base,
      type: 'codex',
      subtype: 'item_completed',
      turnId: 'turn-1',
      item: { id: 'commentary-1', type: 'agentMessage', text: 'Backend is ready.', phase: 'commentary' }
    },
    {
      ...base,
      type: 'codex',
      subtype: 'item_started',
      turnId: 'turn-1',
      item: { id: 'answer-1', type: 'agentMessage', text: '', phase: 'final_answer' }
    },
    {
      ...base,
      type: 'codex',
      subtype: 'agent_message_delta',
      turnId: 'turn-1',
      itemId: 'answer-1',
      delta: 'Sessions GUI'
    },
    {
      ...base,
      type: 'codex',
      subtype: 'item_completed',
      turnId: 'turn-1',
      item: { id: 'answer-1', type: 'agentMessage', text: 'Sessions GUI shipped.', phase: 'final_answer' }
    },
    {
      ...base,
      type: 'codex',
      subtype: 'turn_completed',
      turnId: 'turn-1',
      status: 'completed'
    }
  ];

  const streaming = eventsToMessages(events.slice(0, -2));
  const streamingAssistant = streaming.find((message) => message.role === 'assistant');
  assert.equal(streamingAssistant?.streaming, true);
  assert.equal(streamingAssistant?.content, 'Sessions GUI');

  const messages = eventsToMessages(events);
  assert.equal(messages.length, 2);
  assert.equal(messages[0].role, 'user');
  assert.equal(messages[0].author?.name, 'PM Claude');
  const assistant = messages[1];
  assert.equal(assistant.role, 'assistant');
  assert.equal(assistant.content, 'Sessions GUI shipped.');
  assert.equal(assistant.streaming, false);
  assert.equal(assistant.turnStatus, 'completed');
  assert.deepEqual(assistant.updates, ['Backend is ready.']);
  assert.equal(assistant.reasoningSummary, 'Reuse the normalized event boundary.');
  assert.equal(assistant.plan?.[1]?.status, 'inProgress');
  assert.equal(assistant.toolCalls?.[0]?.name, 'Command');
  assert.match(assistant.toolCalls?.[0]?.resultFull ?? '', /exit code: 0/);

  const claude = eventsToMessages([
    {
      type: 'user',
      uuid: 'claude-user',
      timestamp: '2026-07-20T10:00:00Z',
      author: { kind: 'session', id: 'lane-2', name: 'Review lane', client: 'sessions-cli' },
      message: { role: 'user', content: 'hello' }
    },
    {
      type: 'assistant',
      uuid: 'claude-assistant',
      timestamp: '2026-07-20T10:00:01Z',
      message: { role: 'assistant', content: [{ type: 'text', text: 'hi' }] }
    }
  ]);
  assert.deepEqual(claude.map((message) => message.content), ['hello', 'hi']);
  assert.equal(claude[0].author?.name, 'Review lane');

  const rejected = eventsToMessages([
    {
      type: 'system',
      subtype: 'input_rejected',
      timestamp: '2026-07-20T10:00:02Z',
      error: 'Claude is still working. This message was not sent or queued.'
    }
  ]);
  assert.equal(rejected.length, 1);
  assert.match(rejected[0].errorResponse, /not sent or queued/);

  process.stdout.write('structured-events smoke passed\n');
} finally {
  await rm(work, { recursive: true, force: true });
}
