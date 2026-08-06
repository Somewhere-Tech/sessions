import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { build } from 'esbuild';

const scratch = await mkdtemp(join(tmpdir(), 'sessions-status-smoke-'));
const output = join(scratch, 'session-status.mjs');

try {
  await build({
    entryPoints: ['src/lib/sessionStatus.ts'],
    bundle: true,
    format: 'esm',
    outfile: output,
    platform: 'node',
    target: 'node20',
    logLevel: 'silent'
  });

  const {
    canContinueSession,
    classifySession,
    continuationSession,
    endedCategory,
    endedSummary,
    isCrashedSession,
    isDegradedSession,
    sessionIsFinished,
    sessionNeedsYou,
    sessionWantsAttention
  } = await import(`${pathToFileURL(output).href}?v=${Date.now()}`);

  const liveMcpWarning = {
    id: 'mcp-warning',
    tool: 'codex',
    exited: false,
    exitCode: null,
    exitSignal: null,
    idleReason: 'failed',
    idleDetail: '⚠ MCP startup incomplete (failed: somewhere)',
    provenanceStatus: 'rooted'
  };
  assert.equal(isDegradedSession(liveMcpWarning), true);
  assert.equal(isCrashedSession(liveMcpWarning), false);

  const explicitlyEnded = {
    ...liveMcpWarning,
    id: 'ended-by-user',
    exited: true,
    exitReason: 'ended-by-user',
    idleReason: 'completed'
  };
  assert.equal(endedCategory(explicitlyEnded), 'user');
  assert.equal(endedSummary(explicitlyEnded).label, 'Ended');

  const endedByAgent = {
    ...explicitlyEnded,
    endedByKind: 'session',
    endedById: 'pm-claude',
    endedByClient: 'sessions-cli',
    endReason: 'Kill completed lanes',
    endOperationId: 'batch-123'
  };
  const attributed = endedSummary(endedByAgent, [{ id: 'pm-claude', name: 'PM Claude' }]);
  assert.equal(attributed.label, 'Ended by PM Claude');
  assert.match(attributed.detail, /Kill completed lanes/);
  assert.match(attributed.detail, /batch end operation/);

  const providerFinished = {
    ...explicitlyEnded,
    id: 'provider-finished',
    exitReason: 'completed',
    exitCode: 0
  };
  assert.equal(endedCategory(providerFinished), 'provider');
  assert.equal(endedSummary(providerFinished).label, 'Finished on its own');

  const crashed = {
    ...explicitlyEnded,
    id: 'crashed',
    exitReason: 'failed',
    exitCode: 1
  };
  assert.equal(isCrashedSession(crashed), true);
  assert.equal(endedCategory(crashed), 'crashed');
  assert.equal(endedSummary(crashed).label, 'Ended unexpectedly');
  assert.equal(endedSummary(crashed).tone, 'attention');
  assert.match(endedSummary(crashed).detail, /Saved history is still available/);

  const runnerLost = {
    ...explicitlyEnded,
    id: 'runner-lost',
    exitReason: 'runner-lost',
    provenanceStatus: 'lost'
  };
  assert.equal(endedSummary(runnerLost).label, 'Ready to continue');
  assert.match(endedSummary(runnerLost).detail, /conversation and its captured output are saved/);

  const endedWithContinuation = {
    ...explicitlyEnded,
    id: 'original-runtime',
    createdAt: 100,
    reopenedAs: 'live-successor',
    claudeSessionId: 'provider-conversation'
  };
  const liveSuccessor = {
    ...liveMcpWarning,
    id: 'live-successor',
    createdAt: 200,
    name: 'PM',
    claudeSessionId: 'provider-conversation'
  };
  assert.equal(continuationSession(endedWithContinuation, [endedWithContinuation, liveSuccessor])?.id, 'live-successor');
  assert.equal(endedSummary(endedWithContinuation, [endedWithContinuation, liveSuccessor]).label, 'Live as PM');
  assert.equal(canContinueSession(endedWithContinuation), false);

  const inferredSuccessor = {
    ...liveSuccessor,
    id: 'inferred-successor',
    resumedFrom: 'original-runtime'
  };
  assert.equal(continuationSession(
    { ...endedWithContinuation, reopenedAs: undefined },
    [endedWithContinuation, inferredSuccessor]
  )?.id, 'inferred-successor');

  // ── The one classifier ──────────────────────────────────────────────────
  // Each case below is a contradiction two surfaces used to show at once.

  // An exited record still carrying idleReason:'needs-input' read "Ended" in
  // the navigator and "Needs you" in Fleet. Exit is terminal: a dead runtime
  // cannot be waiting for you.
  const endedButAsking = {
    id: 'ended-still-asking',
    tool: 'claude-code',
    exited: true,
    exitReason: 'ended-by-user',
    exitCode: 0,
    exitSignal: null,
    working: false,
    idleReason: 'needs-input',
    provenanceStatus: 'rooted'
  };
  assert.equal(classifySession(endedButAsking).state, 'ended');
  assert.equal(classifySession(endedButAsking).label, 'Ended');
  assert.equal(sessionNeedsYou(endedButAsking), false);
  assert.equal(sessionIsFinished(endedButAsking), true);

  // A degraded session counted toward the navigator's "Needs you" badge but
  // not toward Home's "Needs you" tile: two numbers, same screen, same
  // sessions. Degraded is calm and is never a question.
  assert.equal(classifySession(liveMcpWarning).state, 'limited');
  assert.equal(classifySession(liveMcpWarning).label, 'Limited');
  assert.equal(sessionNeedsYou(liveMcpWarning), false);
  assert.equal(classifySession(liveMcpWarning).degraded, true);
  assert.equal(sessionWantsAttention(liveMcpWarning), true);

  // A recorded provider question outranks a transient working heuristic, so a
  // pending approval is never labelled "Working". The daemon clears
  // idleReason when it sets working (runtime/internal/state/session.go), so
  // this only bites on cached snapshots and on SessionView's event-derived
  // working signal — which reports "working" for a turn stopped on tool_use,
  // i.e. exactly a tool awaiting approval.
  const askingWhileBusy = {
    id: 'asking',
    tool: 'claude-code',
    exited: false,
    exitCode: null,
    exitSignal: null,
    working: true,
    idleReason: 'needs-input',
    provenanceStatus: 'rooted'
  };
  assert.equal(classifySession(askingWhileBusy).state, 'needs-you');
  assert.equal(classifySession(askingWhileBusy, { working: true }).state, 'needs-you');
  assert.equal(sessionNeedsYou(askingWhileBusy), true);

  // A crashed session outranks everything, exited or not.
  assert.equal(classifySession(crashed).state, 'failed');
  assert.equal(classifySession(crashed).label, 'Failed');
  assert.equal(sessionIsFinished(crashed), false);
  assert.equal(classifySession(runnerLost).state, 'failed');

  // Ordinary live states.
  const working = { ...askingWhileBusy, id: 'working', idleReason: undefined };
  assert.equal(classifySession(working).state, 'working');
  const ready = { ...working, working: false };
  assert.equal(classifySession(ready).state, 'ready');
  assert.equal(classifySession({ ...ready, idleReason: 'completed' }).state, 'finished');
  assert.equal(classifySession({ ...ready, idleReason: 'never-started' }).state, 'not-started');

  // The caller-supplied live activity signal replaces the daemon flag at its
  // own step, and cannot promote a session past exited-ness.
  assert.equal(classifySession(ready, { working: true }).state, 'working');
  assert.equal(classifySession(endedButAsking, { working: true }).state, 'ended');

  // Every state carries exactly one label and one `is-<state>` class token.
  const states = new Set();
  for (const sample of [crashed, endedButAsking, askingWhileBusy, working, liveMcpWarning, ready]) {
    const status = classifySession(sample);
    assert.equal(status.className, `is-${status.state}`);
    assert.ok(status.label.length > 0);
    states.add(status.state);
  }
  assert.equal(states.size, 6, 'each sample must land in a distinct state');

  console.log('session status smoke: ok');
} finally {
  await rm(scratch, { recursive: true, force: true });
}
