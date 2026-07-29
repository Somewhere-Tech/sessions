// Unit-style proof for finish-notification classification and summaries.
// Run from prettyd/: node_modules/.bin/tsx scripts/test-idle-notifications.mjs

import assert from 'node:assert/strict';
import { classifyIdleReason, finalAssistantSummary } from '../src/sessions.js';

function internalFor(snapshot) {
  return {
    mirrorSerialize: {
      serialize: () => snapshot,
    },
  };
}

const snapshots = [
  {
    name: 'done screen',
    text: 'Implemented finish notifications.\n12 tests passed, 0 failed.\n❯',
    expected: 'done',
  },
  {
    name: 'y/n prompt',
    text: 'The migration changes production data.\nContinue? [y/N]',
    expected: 'blocked',
  },
  {
    name: 'numbered picker',
    text: 'Deployment target\n❯ 1) Staging\n  2) Production\n  3) Cancel',
    expected: 'blocked',
  },
  {
    name: 'error trace',
    text: 'Traceback (most recent call last):\n  at notify.ts:42\nFatal error: connection failed',
    expected: 'error',
  },
  {
    name: 'resolved error',
    text: 'Error: first attempt failed\nRetrying with fallback\nAll checks passed',
    expected: 'done',
  },
];

console.log('classifyIdleReason samples');
for (const sample of snapshots) {
  const actual = classifyIdleReason(internalFor(sample.text));
  assert.equal(actual, sample.expected, sample.name);
  console.log(`  ${sample.name}: ${actual}`);
}

const summary = finalAssistantSummary([
  {
    type: 'assistant',
    message: {
      role: 'assistant',
      content: [{ type: 'text', text: '## Shipped **lovable notifications**. Added rich hook metadata too.' }],
    },
  },
  {
    type: 'assistant',
    message: { role: 'assistant', content: [], usage: { output_tokens: 12 } },
  },
]);
assert.equal(summary, 'Shipped lovable notifications.');
console.log(`finalAssistantSummary sample: ${summary}`);

console.log(`\n${snapshots.length + 1} assertions passed`);
