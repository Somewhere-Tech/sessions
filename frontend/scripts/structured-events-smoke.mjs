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

  const orderedClaude = eventsToMessages([
    {
      type: 'user',
      uuid: 'ordered-user',
      timestamp: '2026-07-20T11:00:00Z',
      message: { role: 'user', content: 'Continue the merge' }
    },
    {
      type: 'assistant',
      uuid: 'ordered-intro',
      timestamp: '2026-07-20T11:00:01Z',
      message: { role: 'assistant', content: [{ type: 'text', text: 'I will merge master first.' }] }
    },
    {
      type: 'assistant',
      uuid: 'ordered-tool-1',
      timestamp: '2026-07-20T11:00:02Z',
      message: { role: 'assistant', content: [{ type: 'tool_use', id: 'tool-1', name: 'Bash', input: { command: 'git merge master', description: 'Fetch and merge master' } }] }
    },
    {
      type: 'user',
      uuid: 'ordered-result-1',
      timestamp: '2026-07-20T11:00:03Z',
      message: { role: 'user', content: [{ type: 'tool_result', tool_use_id: 'tool-1', content: 'merged' }] }
    },
    {
      type: 'assistant',
      uuid: 'ordered-tool-2',
      timestamp: '2026-07-20T11:00:04Z',
      message: { role: 'assistant', content: [{ type: 'tool_use', id: 'tool-2', name: 'Bash', input: { command: 'git status', description: 'List conflicted files' } }] }
    },
    {
      type: 'user',
      uuid: 'ordered-result-2',
      timestamp: '2026-07-20T11:00:05Z',
      message: { role: 'user', content: [{ type: 'tool_result', tool_use_id: 'tool-2', content: 'package.json' }] }
    },
    {
      type: 'assistant',
      uuid: 'ordered-followup',
      timestamp: '2026-07-20T11:00:06Z',
      message: { role: 'assistant', content: [{ type: 'text', text: 'Now I will regenerate the docs.' }] }
    }
  ]);
  assert.deepEqual(
    orderedClaude.map((message) => ({ content: message.content, tools: message.toolCalls?.map((tool) => tool.id) ?? [] })),
    [
      { content: 'Continue the merge', tools: [] },
      { content: 'I will merge master first.', tools: [] },
      { content: '', tools: ['tool-1', 'tool-2'] },
      { content: 'Now I will regenerate the docs.', tools: [] }
    ],
    'Claude tool activity must remain between the prose messages that surround it'
  );

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

  // Codex accepts input for an active turn through turn/steer. Sessions keeps
  // that provider-native state visible until the owning turn completes.
  const codexBase = { source: 'codex-app-server', conversationId: 'thread-2' };
  const codexQueued = eventsToMessages([
    { ...codexBase, type: 'codex', subtype: 'turn_started', turnId: 'turn-1', timestamp: '2026-07-20T10:00:00Z' },
    {
      ...codexBase,
      type: 'user',
      subtype: 'user_steer',
      turnId: 'turn-1',
      queued: true,
      timestamp: '2026-07-20T10:00:02Z',
      message: { role: 'user', content: 'Also run the integration tests.' }
    }
  ]);
  assert.equal(codexQueued.filter((message) => message.queued).length, 1);
  assert.equal(codexQueued.find((message) => message.role === 'user')?.content, 'Also run the integration tests.');

  const codexCompletedQueue = eventsToMessages([
    { ...codexBase, type: 'codex', subtype: 'turn_started', turnId: 'turn-1', timestamp: '2026-07-20T10:00:00Z' },
    {
      ...codexBase,
      type: 'user',
      subtype: 'user_steer',
      turnId: 'turn-1',
      queued: true,
      timestamp: '2026-07-20T10:00:02Z',
      message: { role: 'user', content: 'Also run the integration tests.' }
    },
    { ...codexBase, type: 'codex', subtype: 'turn_completed', turnId: 'turn-1', status: 'completed', timestamp: '2026-07-20T10:00:03Z' }
  ]);
  assert.equal(codexCompletedQueue.filter((message) => message.queued).length, 0);

  // A provider refusal is still explicit. It remains separate from the turn
  // in flight rather than vanishing or being mistaken for assistant prose.
  const codexRejectionText =
    'Codex did not accept the message for its active turn. The message was not queued.';
  const codexRejected = eventsToMessages([
    { ...codexBase, type: 'codex', subtype: 'turn_started', turnId: 'turn-1', timestamp: '2026-07-20T10:00:00Z' },
    {
      ...codexBase,
      type: 'codex',
      subtype: 'agent_message_delta',
      turnId: 'turn-1',
      itemId: 'item-1',
      delta: 'Working on it.',
      timestamp: '2026-07-20T10:00:01Z'
    },
    {
      ...codexBase,
      type: 'system',
      subtype: 'input_rejected',
      timestamp: '2026-07-20T10:00:02Z',
      error: codexRejectionText
    }
  ]);
  const codexRejections = codexRejected.filter((message) => message.errorResponse);
  assert.equal(codexRejections.length, 1, 'a rejected Codex message must survive into the rendered list');
  assert.equal(codexRejections[0].errorResponse, codexRejectionText);
  assert.equal(codexRejections[0].content, '', 'the rejection carries no assistant prose of its own');
  // It must be its own entry, not folded into the turn that was in flight —
  // the rejection has no turnId and does not belong to that turn's output.
  assert.equal(codexRejected.length, 2, 'the rejection is a separate message from the in-flight turn');
  assert.equal(codexRejected[0].content, 'Working on it.');
  assert.equal(codexRejected[0].errorResponse, undefined, 'the in-flight turn must not absorb the rejection');
  assert.ok(
    codexRejected[0].createdAt <= codexRejections[0].createdAt,
    'the rejection is ordered after the turn it interrupted'
  );

  const codexRecoverableRejection = eventsToMessages([
    {
      ...codexBase,
      type: 'system',
      subtype: 'input_rejected',
      timestamp: '2026-07-20T10:00:02Z',
      input: 'Do not lose this follow-up.',
      error: codexRejectionText
    }
  ]);
  assert.deepEqual(
    codexRecoverableRejection.map((message) => ({
      role: message.role, content: message.content, status: message.status
    })),
    [{ role: 'user', content: 'Do not lose this follow-up.', status: 'failed' }],
    'a refused steering message must remain recoverable on every client'
  );

  // A steer Codex received but never confirmed. Its exact text is the
  // person's and is shown once, unconfirmed: never as sent, never as a failed
  // draft to send again, and a later provider echo of it adds nothing. Codex
  // may have applied it, so agent text after it starts a new entry.
  const unconfirmedText = 'Ändere den Plan:\n\tzweite Zeile — 日本語 ✓';
  const unconfirmedSteer = eventsToMessages([
    { ...codexBase, type: 'user', subtype: 'user_message', timestamp: '2026-07-20T10:00:00Z', message: { role: 'user', content: 'Long task' } },
    { ...codexBase, type: 'codex', subtype: 'turn_started', turnId: 'turn-1', timestamp: '2026-07-20T10:00:00.5Z' },
    { ...codexBase, type: 'codex', subtype: 'agent_message_delta', turnId: 'turn-1', itemId: 'a1', delta: 'Before.', timestamp: '2026-07-20T10:00:01Z' },
    {
      ...codexBase, type: 'system', subtype: 'input_rejected', timestamp: '2026-07-20T10:00:02Z', unconfirmed: true,
      unconfirmedInput: unconfirmedText, operationId: 'op-1', error: 'Codex did not confirm the message sent to its active turn',
      author: { kind: 'session', id: 'lane-1', name: 'PM Claude', client: 'sessions-cli' }
    },
    { ...codexBase, type: 'codex', subtype: 'item_completed', turnId: 'turn-1', timestamp: '2026-07-20T10:00:03Z',
      item: { id: 'echo', type: 'userMessage', content: [{ type: 'text', text: unconfirmedText }] } },
    { ...codexBase, type: 'codex', subtype: 'agent_message_delta', turnId: 'turn-1', itemId: 'a2', delta: 'After.', timestamp: '2026-07-20T10:00:04Z' },
    { ...codexBase, type: 'codex', subtype: 'turn_completed', turnId: 'turn-1', status: 'completed', timestamp: '2026-07-20T10:00:05Z' }
  ]);
  assert.deepEqual(
    unconfirmedSteer.map((message) => [message.role, message.status, message.content]),
    [['user', 'sent', 'Long task'], ['assistant', 'sent', 'Before.'], ['user', 'unconfirmed', unconfirmedText], ['assistant', 'sent', 'After.']],
    'an unconfirmed steer must appear once, exactly, in order, and labelled unconfirmed'
  );
  assert.equal(unconfirmedSteer[2].author?.name, 'PM Claude', 'an unconfirmed steer keeps its sender');
  assert.equal(unconfirmedSteer[2].errorResponse, undefined, 'an unconfirmed steer is not an error');

  // Chronology of a steer whose record is written late. The runner stamps the
  // record with its submission time but appends it when Codex answers, which
  // can be after later output or after the turn completed, and one agent item
  // can stream across the steer. Output recorded before the submission stays
  // before it, output after stays after, and no text is lost or repeated.
  const steerAt = '2026-07-20T10:00:02Z';
  const at = (second) => `2026-07-20T10:00:0${second}Z`;
  const turnEvent = (subtype, second, rest = {}) => ({ ...codexBase, type: 'codex', subtype, turnId: 'turn-1', timestamp: at(second), ...rest });
  const longTask = { ...codexBase, type: 'user', subtype: 'user_message', timestamp: at(0), message: { role: 'user', content: 'Long task' } };
  const unconfirmedRecord = (rest = {}) => ({ ...codexBase, type: 'system', subtype: 'input_rejected', timestamp: steerAt, unconfirmed: true,
    unconfirmedInput: unconfirmedText, error: 'Codex did not confirm the message sent to its active turn', ...rest });
  const acceptedRecord = { ...codexBase, type: 'user', subtype: 'user_steer', turnId: 'turn-1', timestamp: steerAt, message: { role: 'user', content: unconfirmedText } };
  const delta = (second, itemId, text) => turnEvent('agent_message_delta', second, { itemId, delta: text });
  const completed = (second, id, text) => turnEvent('item_completed', second, { item: { id, type: 'agentMessage', text } });
  const done = turnEvent('turn_completed', 5, { status: 'completed' });
  const started = turnEvent('turn_started', 0);
  const chronology = (events) => eventsToMessages(events).map((message) => [message.role, message.status, message.content]);
  const expectedChronology = (status) => [['user', 'sent', 'Long task'], ['assistant', 'sent', 'Before.'], ['user', status, unconfirmedText], ['assistant', 'sent', 'After.']];
  for (const [name, status, events] of [
    ['unknown recorded after the turn completed', 'unconfirmed',
      [longTask, started, delta(1, 'a1', 'Before.'), delta(3, 'a2', 'After.'), done, unconfirmedRecord({ operationId: 'op-late' })]],
    ['raw-input unknown recorded late, no operation', 'unconfirmed',
      [longTask, started, delta(1, 'a1', 'Before.'), delta(3, 'a2', 'After.'), unconfirmedRecord()]],
    ['one item streams across the steer', 'unconfirmed',
      [longTask, started, delta(1, 'a1', 'Before.'), unconfirmedRecord({ operationId: 'op-span' }), delta(3, 'a1', 'After.'), done]],
    ['one item streams across a late record and completes cumulatively', 'unconfirmed',
      [longTask, started, delta(1, 'a1', 'Before.'), delta(3, 'a1', 'After.'), completed(4, 'a1', 'Before.After.'), done, unconfirmedRecord({ operationId: 'op-cumulative' })]],
    ['one item streams across an accepted steer', 'sent',
      [longTask, started, delta(1, 'a1', 'Before.'), acceptedRecord, delta(3, 'a1', 'After.'), completed(4, 'a1', 'Before.After.'), done]]
  ]) {
    assert.deepEqual(chronology(events), expectedChronology(status), `${name}: output must keep its place around the steer`);
  }
  // An accepted steer is also recorded when Codex answers, stamped with the
  // time it was sent. A uniquely identified record in its own turn is placed
  // there, whether the answer came before or after the turn completed.
  const lateAccepted = (rest = {}) => ({ ...acceptedRecord, ...rest });
  for (const [name, events] of [
    ['accepted, answered after completion, different items', [longTask, started, delta(1, 'a1', 'Before.'), delta(3, 'a2', 'After.'), done, lateAccepted()]],
    ['accepted, answered after completion, one cumulative item', [longTask, started, delta(1, 'a1', 'Before.'), delta(3, 'a1', 'After.'), completed(4, 'a1', 'Before.After.'), done, lateAccepted()]],
    ['accepted, answered before completion, different items', [longTask, started, delta(1, 'a1', 'Before.'), delta(3, 'a2', 'After.'), lateAccepted(), done]],
    ['accepted, answered before completion, one streamed item', [longTask, started, delta(1, 'a1', 'Before.'), delta(3, 'a1', 'After.'), lateAccepted(), done]],
    ['accepted, answered late, provider echoes it', [longTask, started, delta(1, 'a1', 'Before.'),
      turnEvent('item_completed', 3, { item: { id: 'echo', type: 'userMessage', content: [{ type: 'text', text: unconfirmedText }] } }),
      delta(3, 'a2', 'After.'), done, lateAccepted()]]
  ]) {
    const projected = eventsToMessages(events);
    assert.deepEqual(projected.map((message) => [message.role, message.status, message.content]), expectedChronology('sent'), `${name}: placed where it was sent`);
    assert.equal(projected[2].queued, false, `${name}: a steer in a completed turn is not still queued`);
  }
  // Two late steers keep their recorded order relative to each other.
  const secondText = 'Zweiter Hinweis ✓';
  assert.deepEqual(
    chronology([longTask, started, delta(1, 'a1', 'Before.'), delta(4, 'a2', 'After.'), done,
      lateAccepted(), lateAccepted({ timestamp: at(3), message: { role: 'user', content: secondText } })]).map((entry) => entry[2]),
    ['Long task', 'Before.', unconfirmedText, secondText, 'After.'],
    'several late steers are placed in order'
  );
  // What the record cannot establish stays where it was recorded: an
  // unreadable time, no turn of its own, or another turn's events in between.
  for (const [name, record, between] of [
    ['unreadable time', lateAccepted({ timestamp: 'not a time' }), []],
    ['no turn identity', lateAccepted({ turnId: undefined }), []],
    ['another turn in between', lateAccepted(), [{ ...codexBase, type: 'codex', subtype: 'turn_started', turnId: 'turn-2', timestamp: at(6) }]]
  ]) {
    const contents = chronology([longTask, started, delta(1, 'a1', 'Before.'), delta(3, 'a2', 'After.'), done, ...between, record]).map((entry) => entry[2]);
    assert.equal(contents.indexOf(unconfirmedText) > contents.indexOf('Before.\n\nAfter.'), true, `${name}: not moved`);
  }
  // Identical records without a UUID are not independent submissions.
  assert.deepEqual(
    chronology([longTask, started, delta(1, 'a1', 'Before.'), delta(3, 'a2', 'After.'), done, lateAccepted(), lateAccepted()]).map((entry) => entry[2]),
    ['Long task', 'Before.\n\nAfter.', unconfirmedText, unconfirmedText],
    'ambiguous identical late steers keep their recorded order'
  );
  // A completed text that does not continue what was shown cannot be split
  // honestly: the item stays whole, once, where it began, as without a steer.
  assert.deepEqual(
    chronology([longTask, started, delta(1, 'a1', 'Before.'), unconfirmedRecord({ operationId: 'op-rewrite' }), completed(4, 'a1', 'Rewritten answer.'), done]),
    [['user', 'sent', 'Long task'], ['assistant', 'sent', 'Rewritten answer.'], ['user', 'unconfirmed', unconfirmedText]],
    'a completed item that rewrites its streamed text is shown once, where it began'
  );
  // Loading older history must not re-key the steer or the entry after it.
  const older = [{ ...codexBase, type: 'user', subtype: 'user_message', timestamp: '2026-07-20T09:00:00Z', message: { role: 'user', content: 'Older' } },
    { ...codexBase, type: 'codex', subtype: 'turn_started', turnId: 'turn-0', timestamp: '2026-07-20T09:00:01Z' },
    { ...codexBase, type: 'codex', subtype: 'agent_message_delta', turnId: 'turn-0', itemId: 'o1', delta: 'Older answer.', timestamp: '2026-07-20T09:00:02Z' }];
  for (const record of [unconfirmedRecord({ operationId: 'op-page' }), unconfirmedRecord()]) {
    const window = [longTask, started, delta(1, 'a1', 'Before.'), record, delta(3, 'a2', 'After.'), done];
    const keys = (events) => eventsToMessages(events).slice(-3).map((message) => message.id);
    assert.deepEqual(keys([...older, ...window]), keys(window), 'an unconfirmed steer keeps its identity when older history loads');
  }
  // Loading older history must not re-key a placed accepted steer or the
  // entry after it.
  {
    const window = [longTask, started, delta(1, 'a1', 'Before.'), delta(3, 'a2', 'After.'), done, lateAccepted()];
    const keys = (events) => eventsToMessages(events).slice(-3).map((message) => message.id);
    assert.deepEqual(keys([...older, ...window]), keys(window), 'a placed accepted steer keeps its identity when older history loads');
  }

  // Same event with no turn ever started: previously ensureTurn returned null
  // here and the event was dropped on the floor.
  const codexRejectedNoTurn = eventsToMessages([
    { ...codexBase, type: 'codex', subtype: 'turn_started', turnId: 'turn-9', timestamp: '2026-07-20T09:59:00Z' },
    { ...codexBase, type: 'system', subtype: 'input_rejected', timestamp: '2026-07-20T10:00:02Z', error: codexRejectionText }
  ]);
  assert.equal(
    codexRejectedNoTurn.filter((message) => message.errorResponse).length,
    1,
    'a rejection with no turn of its own must still render'
  );

  const providerFaults = eventsToMessages([
    {
      ...codexBase,
      type: 'system',
      subtype: 'provider_fault',
      timestamp: '2026-07-20T10:00:04Z',
      provider: 'codex',
      kind: 'provider-unavailable',
      detail: 'Codex API unavailable (503, overloaded)',
      status: 503
    },
    {
      ...codexBase,
      type: 'system',
      subtype: 'provider_retry',
      timestamp: '2026-07-20T10:00:05Z',
      provider: 'codex',
      attempt: 2,
      max: 5,
      nextAt: Date.now() + 42_000
    }
  ]);
  assert.equal(providerFaults[0].errorResponse, 'Codex API unavailable (503, overloaded)');
  assert.equal(providerFaults[0].content, '', 'provider faults must not become assistant prose');
  assert.equal(providerFaults[1].quietStatus, 'Retrying (2 of 5) …');
  assert.equal(providerFaults[1].content, '', 'provider retries must stay a quiet system line');

  const continued = eventsToMessages([{
    ...codexBase,
    type: 'system',
    subtype: 'continuation_started',
    timestamp: '2026-07-20T10:00:06Z',
    detail: 'Continued from Frozen release plan (Codex) · 84 messages · model Sonnet 5'
  }]);
  assert.equal(continued.length, 1, 'a continued conversation must begin with its source line');
  assert.equal(continued[0].quietStatus, 'Continued from Frozen release plan (Codex) · 84 messages · model Sonnet 5');

  const resumed = eventsToMessages([
    { type: 'user', source: 'sessions-continuation', uuid: 'old-user', message: { role: 'user', content: 'Original request' } },
    { type: 'assistant', source: 'sessions-continuation', uuid: 'old-answer', message: { role: 'assistant', content: [{ type: 'text', text: 'Original answer' }] } },
    { ...codexBase, type: 'user', uuid: 'new-user', message: { role: 'user', content: 'New request' } }
  ]);
  assert.deepEqual(resumed.map((message) => message.content), ['Original request', 'Original answer', 'New request'], 'live app-server events must not hide restored display history');
  const unavailable = eventsToMessages([{ ...codexBase, type: 'system', subtype: 'resume_history_unavailable', detail: 'Earlier display history is unavailable; open the saved runtime.' }]);
  assert.equal(unavailable[0].quietStatus, 'Earlier display history is unavailable; open the saved runtime.');

  process.stdout.write('structured-events smoke passed\n');
} finally {
  await rm(work, { recursive: true, force: true });
}
