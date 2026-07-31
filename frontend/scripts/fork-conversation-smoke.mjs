import fs from 'node:fs';

const navigator = fs.readFileSync(new URL('../src/components/SessionNavigator.tsx', import.meta.url), 'utf8');
const api = fs.readFileSync(new URL('../src/api/sessionsd.ts', import.meta.url), 'utf8');
const history = fs.readFileSync(new URL('../src/components/SessionHistoryView.tsx', import.meta.url), 'utf8');
const conversation = fs.readFileSync(new URL('../src/components/RemoteView.tsx', import.meta.url), 'utf8');
const view = fs.readFileSync(new URL('../src/components/SessionView.tsx', import.meta.url), 'utf8');
const forkButton = fs.readFileSync(new URL('../src/components/ConversationForkButton.tsx', import.meta.url), 'utf8');

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
for (const text of ['sourceMessageIndex', 'sourceMessageId']) {
  if (!api.includes(text)) throw new Error(`missing exact fork-point API field: ${text}`);
}
for (const source of [history, conversation]) {
  if (!source.includes('Fork here')) {
    throw new Error('saved and live conversation views must expose an exact fork-point action in fork mode');
  }
  if (!source.includes('The original stays unchanged') && !source.includes('This conversation keeps running')) {
    throw new Error('fork-point UI must state that the source remains unchanged');
  }
}
if (!view.includes('forkMode={forkMode}') || !history.includes('forkMode && onFork')) {
  throw new Error('fork points must stay hidden until the user enters fork mode');
}
if (!conversation.includes('Boolean(a.onToggleFork) !== Boolean(b.onToggleFork)')) {
  throw new Error('memoized live messages must repaint when fork mode changes');
}
if (!forkButton.includes("active ? 'Cancel fork' : 'Fork'")) {
  throw new Error('conversation views must expose one clear fork-mode control');
}
if (conversation.includes('reasoned before replying')) {
  throw new Error('generic reasoning-presence badges should not clutter the conversation');
}

console.log('conversation fork smoke passed');
