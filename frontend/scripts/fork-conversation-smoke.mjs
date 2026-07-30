import fs from 'node:fs';

const navigator = fs.readFileSync(new URL('../src/components/SessionNavigator.tsx', import.meta.url), 'utf8');
const api = fs.readFileSync(new URL('../src/api/sessionsd.ts', import.meta.url), 'utf8');

for (const text of [
  'Open a copy in ${otherProviderLabel}',
  "Fork a copy in {providerName === 'claude' ? 'Claude' : 'Codex'}",
  'original keeps running'
]) {
  if (!navigator.includes(text)) throw new Error(`missing live-copy UI contract: ${text}`);
}
if (!api.includes('/api/recovery/fork')) {
  throw new Error('live-copy UI must use the dedicated non-lifecycle fork endpoint');
}

console.log('conversation fork smoke passed');
