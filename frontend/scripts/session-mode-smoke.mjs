// The session-mode vocabulary: what Sessions calls a runtime, and when.
//
// Rescued from lifecycle-clarity-smoke.mjs, which was deleted. That suite held
// ~177 `assert.match(<file contents>, /regex/)` assertions over eighteen
// components plus three hand-copied-HTML CSS scenarios — none of which ran a
// line of product code. These twelve did: they bundle lib/sessionMode.ts and
// call it. That is the whole of what survived, and it is kept intact.
import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { build } from 'esbuild';

const scratch = await mkdtemp(join(tmpdir(), 'sessions-mode-smoke-'));
const output = join(scratch, 'session-mode.mjs');
try {
  await build({
    entryPoints: ['src/lib/sessionMode.ts'],
    bundle: true,
    format: 'esm',
    outfile: output,
    platform: 'node',
    target: 'node20',
    logLevel: 'silent'
  });
  const { sessionMode, sessionModeGlyph, sessionModeName } = await import(`${pathToFileURL(output).href}?v=${Date.now()}`);
  const rich = { kind: 'codex-app-server', tool: 'codex' };
  const terminal = { kind: 'pty', tool: 'claude-code', args: [] };
  const remote = { kind: 'pty', tool: 'claude-code', args: ['--remote-control'] };
  assert.equal(sessionMode(rich), 'rich');
  assert.equal(sessionModeGlyph(rich), '◆');
  assert.equal(sessionModeName(rich), 'Rich — Codex app-server');
  assert.equal(sessionMode(terminal), 'terminal');
  assert.equal(sessionModeGlyph(terminal), '▮');
  assert.equal(sessionModeName(terminal), 'Claude interactive — Conversation + Terminal');
  assert.equal(sessionModeName(remote), 'Claude Remote Control — Conversation + Terminal');
} finally {
  await rm(scratch, { recursive: true, force: true });
}

console.log('session mode smoke: ok');
