import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { build } from 'esbuild';

const scratch = await mkdtemp(join(tmpdir(), 'sessions-terminal-viewport-'));
const output = join(scratch, 'terminal-viewport.mjs');
try {
  await build({
    entryPoints: ['src/lib/terminalViewport.ts'],
    bundle: true,
    format: 'esm',
    outfile: output,
    platform: 'node',
    target: 'node20',
    logLevel: 'silent'
  });
  const { TerminalViewportCoordinator, isTerminalScrollKey } = await import(
    `${pathToFileURL(output).href}?v=${Date.now()}`
  );

  const point = (type, viewportY, baseY) => ({ type, viewportY, baseY });
  const restore = (coordinator, position) => {
    const calls = [];
    coordinator.restoreAfterMutation(
      position,
      (amount) => calls.push(['lines', amount]),
      () => calls.push(['bottom'])
    );
    return calls;
  };

  const outputWhileReading = new TerminalViewportCoordinator();
  outputWhileReading.userScrolled(point('normal', 50, 100));
  outputWhileReading.beforeMutation(point('normal', 50, 100));
  assert.deepEqual(
    restore(outputWhileReading, point('normal', 100, 110)),
    [['lines', -50]],
    'provider output must restore the user-owned scrollback anchor'
  );
  assert.equal(outputWhileReading.isFollowing(point('normal', 50, 110)), false);

  outputWhileReading.followLatest(point('normal', 50, 110));
  assert.deepEqual(restore(outputWhileReading, point('normal', 55, 120)), [['bottom']],
    'Jump to latest must explicitly restore follow mode');

  const alternateScreen = new TerminalViewportCoordinator();
  alternateScreen.userScrolled(point('normal', 40, 100));
  alternateScreen.beforeMutation(point('normal', 40, 100));
  assert.deepEqual(restore(alternateScreen, point('alternate', 0, 0)), [['bottom']],
    'an alternate screen owns separate follow intent');
  alternateScreen.beforeMutation(point('alternate', 0, 0));
  assert.deepEqual(restore(alternateScreen, point('normal', 100, 120)), [['lines', -60]],
    'returning from the alternate screen must recover the normal-buffer anchor');

  const eviction = new TerminalViewportCoordinator();
  eviction.userScrolled(point('normal', 40, 100));
  eviction.beforeMutation(point('normal', 40, 100));
  assert.deepEqual(restore(eviction, point('normal', 10, 30)), [['lines', 20]],
    'bounded scrollback eviction must clamp an unavailable anchor safely');

  assert.equal(isTerminalScrollKey({ key: 'PageUp', shiftKey: false }), true);
  assert.equal(isTerminalScrollKey({ key: 'ArrowUp', shiftKey: true }), true);
  assert.equal(isTerminalScrollKey({ key: 'ArrowUp', shiftKey: false }), false);
  assert.equal(isTerminalScrollKey({ key: 'a', shiftKey: false }), false);

  const hook = await readFile(new URL('../src/hooks/useTerminal.ts', import.meta.url), 'utf8');
  assert.match(hook, /term\.onScroll\(\(\) => \{\s*if \(!userScrollGesture \|\| internalViewportScrollDepth > 0\) return;/s,
    'output-driven onScroll must not become user follow intent');
  for (const event of ['wheel', 'touchstart', 'touchmove', 'pointerdown', 'keydown']) {
    assert.match(hook, new RegExp(`addEventListener\\('${event}'`),
      `${event} must participate in explicit scroll-intent detection`);
  }
  assert.match(hook, /beforeTerminalMutation\(\);\s*term\.write\(data,[\s\S]*afterTerminalMutation\(\);/,
    'live output must preserve the current viewport intent');
  assert.match(hook, /beforeTerminalMutation\(\);\s*try \{\s*fit\.fit\(\);[\s\S]*afterTerminalMutation\(\);/,
    'FitAddon resize must preserve the current viewport intent');
} finally {
  await rm(scratch, { recursive: true, force: true });
}

console.log('terminal viewport smoke: ok');
