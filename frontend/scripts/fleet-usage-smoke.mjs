import assert from 'node:assert/strict';
import { build } from 'esbuild';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const work = await mkdtemp(join(tmpdir(), 'sessions-fleet-usage-'));
try {
  const outfile = join(work, 'fleet-usage.mjs');
  await build({
    // fileURLToPath, not URL.pathname: on Windows the pathname of a file URL
    // keeps the leading slash before the drive letter, so esbuild was handed
    // "/D:/…/fleetUsage.ts" and could not resolve it. Every other smoke script
    // already converts properly; this one did not, and no Windows job had run
    // to say so.
    entryPoints: [fileURLToPath(new URL('../src/lib/fleetUsage.ts', import.meta.url))],
    outfile,
    bundle: true,
    platform: 'node',
    format: 'esm',
    logLevel: 'silent'
  });
  const { combineFleetUsage } = await import(pathToFileURL(outfile));
  const tokens = (input) => ({ inputTokens: input, outputTokens: 10, cacheCreationTokens: 0, cacheReadTokens: 0, reasoningTokens: 0 });
  const event = (eventKey, input) => ({
    eventKey,
    groupKey: '2026-07-31',
    start: '2026-07-31',
    provider: 'claude',
    providerSessionId: 'conversation-1',
    model: 'claude-opus',
    tokens: tokens(input),
    costUSD: input / 1000,
    recordedCostUSD: 0,
    calculatedCostUSD: input / 1000,
    hasRecordedCost: false,
    missingPricing: false
  });
  const report = (machine, events) => ({
    schemaVersion: 1,
    machine,
    generatedAt: '2026-07-31T12:00:00Z',
    group: 'daily',
    mode: 'auto',
    pricing: { source: 'test', revision: '1', url: 'https://example.test', note: 'test' },
    scan: { filesSeen: 1, filesRead: 1, linesRead: 1, entriesSeen: events.length },
    rows: [],
    totals: { key: 'total', models: [], tokens: tokens(0), costUSD: 0, recordedCostUSD: 0, calculatedCostUSD: 0, entries: 0, missingPricingEntries: 0 },
    eventsIncluded: true,
    events
  });

  const summary = combineFleetUsage([
    { serverId: 'mac', serverName: 'MacBook', report: report('mac', [event('shared', 100)]) },
    { serverId: 'mini', serverName: 'Studio Mini', report: report('mini', [event('shared', 100), event('mini-only', 50)]) },
    { serverId: 'offline', serverName: 'Travel PC', error: 'offline' }
  ]);
  assert.equal(summary.reportingMachines, 2);
  assert.deepEqual(summary.unavailableMachines, ['Travel PC']);
  assert.equal(summary.exactDeduplication, true);
  assert.equal(summary.duplicatesRemoved, 1);
  assert.equal(summary.report.totals.entries, 2);
  assert.equal(summary.report.totals.tokens.inputTokens, 150);
} finally {
  await rm(work, { recursive: true, force: true });
}

console.log('fleet usage smoke: ok');
