#!/usr/bin/env node
// Real-Claude diagnostic. Spawns the actual `claude` CLI under node-pty,
// pipes its output into the same @xterm/headless + SerializeAddon stack
// the browser uses, then dumps:
//   1. The raw PTY stream (what sessionsd's WS forwards verbatim)
//   2. The serialized xterm buffer (what usePrettyParser feeds to detectTool)
//   3. The parser's output (Block[] types) for that snapshot
//
// This is the gap-finder: if the parser produces 0 blocks against a real
// Claude session, the input format from xterm-headless differs from what
// the sessions-tmux parser was written for (tmux capture-pane -p -e -J).

const path = require('node:path');
const fs = require('node:fs');
const os = require('node:os');

const FRONTEND_ROOT = path.resolve(__dirname, '..');
const SESSIONS_NODE_MODULES = path.resolve(FRONTEND_ROOT, '..', 'runtime', 'testdata', 'node-runtime', 'node_modules');

const esbuild = require(path.join(FRONTEND_ROOT, 'node_modules', 'esbuild'));
const { Terminal } = require(path.join(FRONTEND_ROOT, 'node_modules', '@xterm', 'headless'));
const { SerializeAddon } = require(path.join(FRONTEND_ROOT, 'node_modules', '@xterm', 'addon-serialize'));
const pty = require(path.join(SESSIONS_NODE_MODULES, 'node-pty'));

// Bundle the parsers AND the snapshot normalizer.
const tmp = path.join(os.tmpdir(), `sessions-claude-dump-${process.pid}.cjs`);
const tmpNorm = path.join(os.tmpdir(), `sessions-claude-norm-${process.pid}.cjs`);
esbuild.buildSync({
  entryPoints: [path.join(FRONTEND_ROOT, 'src', 'parsers', 'detect.ts')],
  bundle: true,
  platform: 'node',
  format: 'cjs',
  target: 'node20',
  outfile: tmp,
  external: ['react', 'react-dom'],
  logLevel: 'silent'
});
esbuild.buildSync({
  entryPoints: [path.join(FRONTEND_ROOT, 'src', 'lib', 'xtermSnapshot.ts')],
  bundle: true,
  platform: 'node',
  format: 'cjs',
  target: 'node20',
  outfile: tmpNorm,
  logLevel: 'silent'
});
let parserMod, normMod;
try {
  parserMod = require(tmp);
  normMod = require(tmpNorm);
} finally {
  try { fs.unlinkSync(tmp); } catch {}
  try { fs.unlinkSync(tmpNorm); } catch {}
}
const { detectTool } = parserMod;
const { normalizeXtermSnapshot } = normMod;

const COLS = 120, ROWS = 32;
const RUN_MS = 6000; // collect 6s of frames; claude's banner + idle prompt is enough

const term = new Terminal({ cols: COLS, rows: ROWS, scrollback: 5000, allowProposedApi: true });
const serialize = new SerializeAddon();
term.loadAddon(serialize);

const rawChunks = [];

const ptyProc = pty.spawn(
  process.argv[2] || '/opt/homebrew/bin/claude',
  process.argv.slice(3),
  {
    name: 'xterm-256color',
    cols: COLS,
    rows: ROWS,
    cwd: process.env.HOME,
    env: {
      ...process.env,
      TERM: 'xterm-256color',
      COLORTERM: 'truecolor'
    }
  }
);

ptyProc.onData((chunk) => {
  rawChunks.push(chunk);
  term.write(chunk);
});

ptyProc.onExit(() => { /* may or may not happen in this window */ });

setTimeout(async () => {
  // Kill claude — we just wanted the banner + initial prompt.
  try { ptyProc.kill(); } catch {}

  // Wait one tick so any buffered xterm writes finish.
  await new Promise((r) => setTimeout(r, 200));

  const rawSnapshot = serialize.serialize();
  const snapshot = normalizeXtermSnapshot(rawSnapshot);
  const raw = rawChunks.join('');

  const outDir = '/tmp/sessions-claude-dump';
  fs.mkdirSync(outDir, { recursive: true });
  fs.writeFileSync(path.join(outDir, 'raw.bin'), raw);
  fs.writeFileSync(path.join(outDir, 'snapshot.txt'), snapshot);

  const parser = detectTool(snapshot);
  const blocks = parser.parse(snapshot);
  const findings = parser.extractSidebarFindings ? parser.extractSidebarFindings(snapshot, blocks) : {};
  const working = parser.workingState(snapshot);

  fs.writeFileSync(path.join(outDir, 'blocks.json'), JSON.stringify(blocks, null, 2));
  fs.writeFileSync(path.join(outDir, 'findings.json'), JSON.stringify({ findings, working }, null, 2));

  console.log('=== RAW PTY STREAM (first 600 chars, escapes shown) ===');
  console.log(JSON.stringify(raw.slice(0, 600)));
  console.log(`\n  raw bytes: ${raw.length}, written to ${outDir}/raw.bin`);

  console.log('\n=== SERIALIZE() SNAPSHOT (first 1200 chars, escapes shown) ===');
  console.log(JSON.stringify(snapshot.slice(0, 1200)));
  console.log(`\n  snapshot bytes: ${snapshot.length}, written to ${outDir}/snapshot.txt`);

  console.log('\n=== SNAPSHOT (clean, first 60 lines) ===');
  // Strip ANSI for human reading.
  const ANSI_RE = /\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07/g;
  const clean = snapshot.replace(ANSI_RE, '');
  for (const ln of clean.split('\n').slice(0, 60)) {
    console.log('  | ' + ln);
  }

  console.log('\n=== PARSER VERDICT ===');
  console.log(`  parser.id        = ${parser.id}`);
  console.log(`  blocks.length    = ${blocks.length}`);
  console.log(`  blocks.types     = ${JSON.stringify(blocks.map(b => b.type))}`);
  console.log(`  findings         = ${JSON.stringify(findings)}`);
  console.log(`  workingState     = ${JSON.stringify(working)}`);

  console.log(`\nFiles: ${outDir}/{raw.bin, snapshot.txt, blocks.json, findings.json}`);
  process.exit(0);
}, RUN_MS);
