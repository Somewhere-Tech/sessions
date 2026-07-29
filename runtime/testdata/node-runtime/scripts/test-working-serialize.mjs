// Integration test: drive a real @xterm/headless Terminal + SerializeAddon
// (same as sessions.ts) and confirm claudeWorkingFromSnapshot reads the
// footer correctly off serialize({scrollback:0}) — the exact call the
// decay loop makes. Validates the full plumbing without spawning claude.
//   node_modules/.bin/tsx scripts/test-working-serialize.mjs
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import { claudeWorkingFromSnapshot } from '../src/claudeActivity.js';

const require = createRequire(import.meta.url);
const { Terminal } = require('@xterm/headless');
const { SerializeAddon } = require('@xterm/addon-serialize');

function render(lines) {
  const term = new Terminal({ cols: 80, rows: 24, scrollback: 5000, allowProposedApi: true });
  const ser = new SerializeAddon();
  term.loadAddon(ser);
  term.write(lines.join('\r\n'));
  // give xterm's write a tick to flush, then serialize the viewport only
  return new Promise((resolve) => {
    term.write('', () => resolve(ser.serialize({ scrollback: 0 })));
  });
}

let pass = 0;
function ok(n) { pass++; console.log(`  ok  ${n}`); }

// Working screen: spinner + "· esc to interrupt" footer.
const working = await render([
  '⏺ Working on the fix…',
  '✻ Synthesizing… (12s · ↓ 1.2k tokens)',
  '',
  '────────────────────────────────────────',
  '❯',
  '────────────────────────────────────────',
  '⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt · ↓ 1.2k tokens',
]);
assert.equal(claudeWorkingFromSnapshot(working), true);
ok('serialize({scrollback:0}) of a working screen -> working:true');

// Idle screen: same layout but "· ← for agents" footer, and the transcript
// even *mentions* the phrase (the meta case) — must read false.
const idle = await render([
  '⏺ The esc to interrupt footer is the honest signal.',
  '✻ Cooked for 2m 33s',
  '',
  '────────────────────────────────────────',
  '❯',
  '────────────────────────────────────────',
  '⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents',
]);
assert.equal(claudeWorkingFromSnapshot(idle), false);
ok('serialize({scrollback:0}) of an idle screen (with prose) -> working:false');

console.log(`\n${pass} assertions passed`);
