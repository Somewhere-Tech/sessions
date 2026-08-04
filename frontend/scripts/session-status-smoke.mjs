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
    continuationSession,
    endedCategory,
    endedSummary,
    isCrashedSession,
    isDegradedSession
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

  console.log('session status smoke: ok');
} finally {
  await rm(scratch, { recursive: true, force: true });
}
