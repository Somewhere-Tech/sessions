import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { build } from 'esbuild';

const scratch = await mkdtemp(join(tmpdir(), 'sessions-terminal-renderer-'));
const output = join(scratch, 'terminal-renderer.mjs');
try {
  await build({
    entryPoints: ['src/lib/terminalRenderer.ts'],
    bundle: true,
    format: 'esm',
    outfile: output,
    platform: 'node',
    target: 'node20',
    logLevel: 'silent'
  });
  const { terminalRenderer } = await import(`${pathToFileURL(output).href}?v=${Date.now()}`);
  const macWebKit = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 Version/18.0 Safari/605.1.15';
  const windowsWebView = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/130.0.0.0 Safari/537.36 Edg/130.0.0.0';
  const androidWebView = 'Mozilla/5.0 (Linux; Android 15) AppleWebKit/537.36 Chrome/130.0.0.0 Mobile Safari/537.36';
  assert.equal(terminalRenderer(true, macWebKit), 'dom', 'native Apple WebViews must avoid retained-layer ghosting');
  assert.equal(terminalRenderer(true, windowsWebView), 'webgl', 'WebView2 keeps the fast WebGL renderer');
  assert.equal(terminalRenderer(true, androidWebView), 'webgl', 'Android Chromium keeps the fast WebGL renderer');
  assert.equal(terminalRenderer(false, macWebKit), 'webgl', 'browser clients do not inherit the native WKWebView workaround');

  // Eleven `assert.match(<file contents>, /regex/)` assertions over
  // useTerminal.ts, SessionView.tsx and globals.css stood here. None of them
  // ran a line of the code they named; each one failed on a rename and passed
  // on a broken renderer. The renderer decision itself is tested above, by
  // calling it.
} finally {
  await rm(scratch, { recursive: true, force: true });
}

console.log('terminal renderer smoke: ok');
