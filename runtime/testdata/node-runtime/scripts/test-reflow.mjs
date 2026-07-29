// Smoke test for prettyd/src/reflow.ts. Exercises the cases that actually
// matter for Claude Code / Codex / shell output: SGR carry across breaks,
// bullet indent preservation, table/box-drawing preservation, cursor-
// forward expansion, narrow-width sanity.
//
// Run: node --experimental-vm-modules ../node_modules/.bin/tsx scripts/test-reflow.mjs
//   or: cd prettyd && npx tsx scripts/test-reflow.mjs

import assert from 'node:assert/strict';
import { reflowAnsi, stripAnsi, expandCursorForward, shouldPreserveLine } from '../src/reflow.ts';

let passed = 0;
let failed = 0;

function test(name, fn) {
  try {
    fn();
    console.log(`  ok  ${name}`);
    passed++;
  } catch (err) {
    console.log(`  FAIL ${name}`);
    console.log(`       ${err.message}`);
    if (err.expected !== undefined) {
      console.log(`       expected: ${JSON.stringify(err.expected)}`);
      console.log(`       actual:   ${JSON.stringify(err.actual)}`);
    }
    failed++;
  }
}

console.log('reflow smoke tests');

test('plain ASCII below width — pass-through', () => {
  const out = reflowAnsi('hello world', { width: 80 });
  assert.equal(out, 'hello world');
});

test('plain ASCII over width — wraps at space', () => {
  const out = reflowAnsi('one two three four five six seven', { width: 12 });
  // "one two" = 7, +" three" → 13 > 12 → break before "three".
  // Expected: "one two\r\nthree four\r\nfive six\r\nseven"
  const lines = out.split('\r\n');
  for (const line of lines) {
    assert.ok(stripAnsi(line).length <= 12, `line "${line}" exceeds 12: ${stripAnsi(line).length}`);
  }
  // No words split.
  assert.equal(stripAnsi(out).replace(/\r\n/g, ' '), 'one two three four five six seven');
});

test('SGR active across wrap break — re-applied on continuation', () => {
  const input = '\x1b[31mthis is a long red sentence that wraps\x1b[0m';
  const out = reflowAnsi(input, { width: 15 });
  const lines = out.split('\r\n');
  assert.ok(lines.length >= 2, `expected ≥2 lines, got ${lines.length}: ${JSON.stringify(out)}`);
  // Every line that has visible text should both START with the red SGR
  // (or some SGR that includes 31m) AND end with the reset.
  for (const line of lines) {
    if (stripAnsi(line).trim().length === 0) continue;
    assert.ok(line.includes('\x1b[31m'), `line missing red SGR: ${JSON.stringify(line)}`);
    assert.ok(line.endsWith('\x1b[0m'), `line missing trailing reset: ${JSON.stringify(line)}`);
  }
});

test('SGR transitions within a line render correctly after wrap', () => {
  // "red red red <reset> blue blue blue" wrapped tight.
  const input = '\x1b[31mAAAA AAAA\x1b[0m \x1b[34mBBBB BBBB\x1b[0m';
  const out = reflowAnsi(input, { width: 10 });
  const plain = stripAnsi(out).replace(/\r\n/g, ' ');
  assert.equal(plain, 'AAAA AAAA BBBB BBBB');
});

test('bullet indent preserved on continuation lines', () => {
  const input = '  - this is a long bullet that needs to wrap onto multiple lines';
  const out = reflowAnsi(input, { width: 20 });
  const lines = out.split('\r\n');
  assert.ok(lines.length >= 2, `expected ≥2 lines: ${JSON.stringify(out)}`);
  // First line keeps the "  - " prefix.
  assert.ok(stripAnsi(lines[0]).startsWith('  - '), `first line lost bullet: ${JSON.stringify(lines[0])}`);
  // Continuation lines start with the original 2-space indent.
  for (let i = 1; i < lines.length; i++) {
    if (stripAnsi(lines[i]).trim().length === 0) continue;
    assert.ok(stripAnsi(lines[i]).startsWith('  '), `continuation lost indent: ${JSON.stringify(lines[i])}`);
  }
});

test('box-drawing line preserved (not wrapped)', () => {
  const input = '╭──────────────────────────────────────────────────────────────────╮';
  const out = reflowAnsi(input, { width: 30 });
  // Should be exactly the input, no wraps.
  assert.equal(out, input);
});

test('columnar / table line preserved', () => {
  const input = 'NAME            STATUS          AGE             RESTARTS';
  const out = reflowAnsi(input, { width: 30 });
  assert.equal(out, input, 'table row should not be wrapped');
});

test('cursor-forward expansion before reflow', () => {
  // \x1b[5C = move right 5 cells. Should become 5 spaces.
  const input = 'A\x1b[5CB';
  const out = reflowAnsi(input, { width: 80 });
  assert.equal(stripAnsi(out), 'A     B');
});

test('multi-line input — wraps each independently', () => {
  const input = 'short\r\nthis is a longer line that wraps';
  const out = reflowAnsi(input, { width: 14 });
  const lines = out.split('\r\n');
  assert.equal(lines[0], 'short');
  assert.ok(lines.length >= 2);
});

test('empty line preserved', () => {
  const out = reflowAnsi('first\r\n\r\nthird', { width: 80 });
  assert.equal(out, 'first\r\n\r\nthird');
});

test('absurd width (1) does not infinite-loop', () => {
  const input = 'hello world this is some text';
  const out = reflowAnsi(input, { width: 1 });
  assert.ok(out.length > 0);
  // Width clamps to a sane minimum; chars may hard-break mid-word, but
  // every visible character of the input still appears.
  const flat = stripAnsi(out).replace(/\s+/g, '');
  const expected = input.replace(/\s+/g, '');
  assert.equal(flat, expected);
});

test('long single token without spaces — hard-breaks', () => {
  const input = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa';
  const out = reflowAnsi(input, { width: 10 });
  const lines = out.split('\r\n');
  for (const line of lines) {
    assert.ok(stripAnsi(line).length <= 10, `hard-break exceeded width: ${stripAnsi(line).length}`);
  }
  assert.equal(stripAnsi(out).replace(/\r\n/g, ''), input);
});

test('shouldPreserveLine: short prose returns true (already fits)', () => {
  assert.equal(shouldPreserveLine('hello', 80), true);
});

test('shouldPreserveLine: long prose returns false', () => {
  const long = 'this is a long line of regular prose with no special characters at all here';
  assert.equal(shouldPreserveLine(long, 30), false);
});

test('shouldPreserveLine: 3+ space-gaps is a table', () => {
  assert.equal(shouldPreserveLine('a    b    c    d', 5), true);
});

test('expandCursorForward direct call', () => {
  assert.equal(expandCursorForward('X\x1b[3CY'), 'X   Y');
  assert.equal(expandCursorForward('X\x1b[10CY').length, 12); // X + 10 + Y
});

test('OSC sequences (hyperlinks) stripped from width count', () => {
  // OSC 8 hyperlink: \x1b]8;;url\x07text\x1b]8;;\x07
  const input = '\x1b]8;;https://example.com\x07click here\x1b]8;;\x07';
  const w = stripAnsi(input).length;
  assert.equal(w, 'click here'.length);
});

test('colors in plain ASCII bullets — wrap preserves color and indent', () => {
  const input = '  \x1b[36m▸\x1b[0m \x1b[1mTitle:\x1b[0m followed by a long enough description that wraps around';
  const out = reflowAnsi(input, { width: 30 });
  const lines = out.split('\r\n');
  assert.ok(lines.length >= 2);
  // First line should have its colored bullet intact.
  assert.ok(lines[0].includes('\x1b[36m') && lines[0].includes('▸'),
    `first line lost colored bullet: ${JSON.stringify(lines[0])}`);
});

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed > 0 ? 1 : 0);
