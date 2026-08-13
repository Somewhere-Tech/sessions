import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
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
  const { terminalNeedsGpuRepair, terminalRenderer } = await import(`${pathToFileURL(output).href}?v=${Date.now()}`);
  const macWebKit = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 Version/18.0 Safari/605.1.15';
  const windowsWebView = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/130.0.0.0 Safari/537.36 Edg/130.0.0.0';
  const androidWebView = 'Mozilla/5.0 (Linux; Android 15) AppleWebKit/537.36 Chrome/130.0.0.0 Mobile Safari/537.36';
  assert.equal(terminalRenderer(true, macWebKit), 'webgl', 'native Apple WebViews must keep accelerated typing');
  assert.equal(terminalRenderer(true, windowsWebView), 'webgl', 'WebView2 keeps the fast WebGL renderer');
  assert.equal(terminalRenderer(true, androidWebView), 'webgl', 'Android Chromium keeps the fast WebGL renderer');
  assert.equal(terminalRenderer(false, macWebKit), 'webgl', 'browser clients do not inherit the native WKWebView workaround');
  assert.equal(terminalNeedsGpuRepair('\x1b[?1049h'), true, 'alternate-screen entry repairs the GPU atlas');
  assert.equal(terminalNeedsGpuRepair('\x1b[2J'), true, 'full-screen erase repairs the GPU atlas');
  assert.equal(terminalNeedsGpuRepair('ordinary terminal echo'), false, 'ordinary typing never triggers a full repaint');

  const hook = await readFile(new URL('../src/hooks/useTerminal.ts', import.meta.url), 'utf8');
  assert.match(hook, /terminalRenderer\(isTauri\(\), navigator\.userAgent\)/);
  assert.match(hook, /webgl\.clearTextureAtlas\(\)/);
  assert.match(hook, /gpuRepairScanTail \+ data/);
  assert.match(hook, /terminalNeedsGpuRepair\(gpuRepairProbe\)/);
  assert.match(hook, /term\.write\(data, \(\) =>/,
    'PTY frames must repaint only after xterm has parsed the complete batch');
  assert.doesNotMatch(hook, /repaintAlternateScreenAfterWrite/,
    'native Apple must not restore the per-frame DOM repaint regression');
  assert.match(hook, /fetchServerSnapshot\(sessionId, undefined, true\)/,
    'interactive terminals must restore bounded server scrollback during bulk prefill');

  const sessionView = await readFile(new URL('../src/components/SessionView.tsx', import.meta.url), 'utf8');
  assert.match(sessionView, /sendInput\('\\x1b\\x1b'\).*↶ Earlier/,
    'mobile terminal controls must expose Claude\'s native Esc Esc rewind');

  const css = await readFile(new URL('../src/styles/globals.css', import.meta.url), 'utf8');
  assert.match(css, /html,\s*body,\s*#root\s*\{[^}]*overflow:\s*hidden/s);
  assert.match(css, /\.operations-content\s*\{[^}]*overflow:\s*hidden/s);
  assert.match(css, /\.remote-scroll\s*\{[^}]*overscroll-behavior:\s*contain/s);
} finally {
  await rm(scratch, { recursive: true, force: true });
}

console.log('terminal renderer smoke: ok');
