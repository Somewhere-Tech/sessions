import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { build } from 'esbuild';
import headless from '@xterm/headless';
import serializePackage from '@xterm/addon-serialize';
import { readStylesheetTree } from './lib/source-styles.mjs';

const { Terminal } = headless;
const { SerializeAddon } = serializePackage;

async function write(terminal, data) {
  await new Promise((resolve) => terminal.write(data, resolve));
}

function serializedTerminal(cols = 40, rows = 8) {
  const terminal = new Terminal({ cols, rows, scrollback: 50, allowProposedApi: true });
  const serialize = new SerializeAddon();
  terminal.loadAddon(serialize);
  return { terminal, serialize };
}

function bufferText(terminal) {
  const buffer = terminal.buffer.active;
  const lines = [];
  for (let index = 0; index < buffer.baseY + terminal.rows; index += 1) {
    lines.push(buffer.getLine(index)?.translateToString(true) ?? '');
  }
  return lines.join('');
}

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

  const css = await readStylesheetTree(new URL('../src/styles/globals.css', import.meta.url));
  assert.match(css, /html,\s*body,\s*#root\s*\{[^}]*overflow:\s*hidden/s);
  assert.match(css, /\.operations-content\s*\{[^}]*overflow:\s*hidden/s);
  assert.match(css, /\.remote-scroll\s*\{[^}]*overscroll-behavior:\s*contain/s);

  // Deterministic VT fixtures exercise the sequences provider TUIs actually
  // use. These pin xterm semantics independently of the native GPU renderer:
  // Sessions forwards bytes unchanged, and the renderer must display exactly
  // the resulting active buffer rather than provider-specific text guesses.
  {
    const { terminal, serialize } = serializedTerminal();
    await write(terminal, 'stale screen\x1b[2J\x1b[Hcurrent screen');
    assert.equal(serialize.serialize(), 'current screen', 'erase-display must remove the preceding frame');
    await write(terminal, '\r\x1b[2Kready');
    assert.equal(serialize.serialize(), 'ready', 'erase-line must remove stale prompt cells');
    terminal.dispose();
  }
  {
    const { terminal, serialize } = serializedTerminal();
    await write(terminal, 'normal history');
    await write(terminal, '\x1b[?1049h\x1b[Hprovider picker');
    assert.match(serialize.serialize(), /provider picker/, 'alternate screen must become the visible buffer');
    await write(terminal, '\x1b[?1049l');
    assert.match(serialize.serialize(), /normal history/, 'leaving alternate screen must restore normal history');
    terminal.dispose();
  }
  {
    const { terminal, serialize } = serializedTerminal(12, 5);
    await write(terminal, 'Claude 🦀 漢字 preserves wide glyphs across a resize');
    terminal.resize(24, 5);
    const rendered = bufferText(terminal);
    assert.match(rendered, /Claude 🦀 漢字/, 'wide Unicode glyphs must survive reflow');
    assert.equal((rendered.match(/preserves/g) ?? []).length, 1, 'resize must not duplicate terminal text');
    terminal.dispose();
  }
  {
    const { terminal, serialize } = serializedTerminal(42, 6);
    await write(terminal, '\x1b[2J\x1b[H✻ Working\r\nold command\r\nold prompt');
    await write(terminal, '\x1b[2J\x1b[H● Ready\r\nnew prompt\x1b[K');
    const rendered = serialize.serialize();
    assert.match(rendered, /● Ready[\s\S]*new prompt/, 'a full-screen Claude redraw must show the current frame');
    assert.doesNotMatch(rendered, /old command|old prompt|✻ Working/, 'a full-screen redraw must not retain ghost rows');
    terminal.dispose();
  }
} finally {
  await rm(scratch, { recursive: true, force: true });
}

console.log('terminal renderer smoke: ok');
