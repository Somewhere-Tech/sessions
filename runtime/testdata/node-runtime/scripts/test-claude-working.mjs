// Tests for the "is Claude in a turn" footer detector. Uses snapshot
// strings shaped like real Claude footers (captured from the PTY logs).
//   node_modules/.bin/tsx scripts/test-claude-working.mjs
import assert from 'node:assert/strict';
import { claudeWorkingFromSnapshot } from '../src/claudeActivity.js';

let pass = 0;
function check(name, input, expected) {
  assert.equal(claudeWorkingFromSnapshot(input), expected, name);
  pass++;
  console.log(`  ok  ${name}`);
}

const transcript = [
  '⏺ Let me run the typecheck.',
  '⏺ Bash(npm run typecheck)',
  '  ⎿  > tsc --noEmit',
  '',
].join('\n');

const inputBox = ['────────────────────────────', '❯', '────────────────────────────'].join('\n');

// 1. idle footer (skip-perms) -> false
check('idle: "← for agents" footer',
  `${transcript}\n${inputBox}\n⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents`,
  false);

// 2. working footer with timer/tokens -> true
check('working: "esc to interrupt · ↓ tokens"',
  `${transcript}\n✻ Synthesizing… (12s)\n${inputBox}\n⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt · ↓ 2.4k tokens`,
  true);

// 3. working footer immediately followed by the /goal statusline glyph -> true
check('working: esc to interrupt + statusline tail',
  `${transcript}\n${inputBox}\n⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt ◎ /goal active (3d)`,
  true);

// 4. CRITICAL: a session whose transcript discusses "esc to interrupt"
//    but is idle (footer says "← for agents") must read false.
check('meta: prose mentions phrase, footer idle -> false',
  [
    '⏺ The esc to interrupt footer is the only honest signal here.',
    '⏺ Confirmed: "esc to interrupt" appears 146x in the logs.',
    inputBox,
    '⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents',
  ].join('\n'),
  false);

// 5. soft-wrapped footer (SerializeAddon emits wrapped rows contiguously,
//    no separator) -> phrase stays intact -> true
check('soft-wrapped footer (contiguous)',
  `${transcript}\n${inputBox}\n⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt · ↓ 1.1k tokens`,
  true);
// whitespace-tolerant: even a hard split inside the phrase still matches
check('whitespace-split phrase still matches',
  '⏵⏵ bypass permissions on (shift+tab to cycle) · esc to\ninterrupt · ↓ 1.1k tokens',
  true);

// 6. finished-turn summary is NOT working ("Cooked for 2m 33s" + idle footer)
check('finished summary -> false',
  `⏺ Done.\n✻ Cooked for 2m 33s\n${inputBox}\n⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents`,
  false);

// 7. ANSI-laden working footer (SGR codes around the phrase) -> true after strip
check('ANSI-wrapped working footer',
  `\x1b[2m⏵⏵ bypass permissions on (shift+tab to cycle) \x1b[0m·\x1b[2m esc to interrupt\x1b[0m · ↓`,
  true);

// 8. empty snapshot -> false
check('empty snapshot -> false', '', false);

// 9. plain prose with phrase but no middot, in last lines -> false
check('plain phrase, no middot -> false', 'press esc to interrupt the build\n', false);

// --- spinner-timer signal (survives the "Remote Control active" overlay) ---

// 10. working spinner present, footer shows "Remote Control active" (no esc)
//     -> true via the spinner, even though the footer hint is clobbered.
check('remote-control: spinner true despite no esc in footer',
  [
    '⏺ Wiring it up…',
    '· Honking… (3m53s · ↓ 15.4k tokens)',
    '  ⎿ Tip: /loop can babysit a PR.',
    inputBox,
    '⏵⏵ bypass permissions on (shift+tab to cycle) Remote Control active',
  ].join('\n'),
  true);

// 11. spinner with spaced timer + prose tail -> true
check('spinner "Canoodling… (2m 30s · …)"',
  `✽ Canoodling… (2m 30s · almost done thinking with xhigh effort)\n${inputBox}\n⏵⏵ bypass permissions on (shift+tab to cycle) Remote Control active`,
  true);

// 12. ellipsis line that is NOT a timer ("(ctrl+o…") + idle footer -> false
check('non-timer ellipsis "(ctrl+o)" -> false',
  `  ⎿  Reading 1 file… (ctrl+o to expand)\n${inputBox}\n⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents`,
  false);

// 13. plain "..." ellipsis variant with timer -> true
check('ascii ellipsis "... (45s" -> true', '✻ Pondering... (45s · esc to interrupt)\n', true);

console.log(`\n${pass} assertions passed`);
