'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const testdata = __dirname;
const mirrorDir = path.resolve(testdata, '..');
const runtime = path.resolve(mirrorDir, '../..');
const repoRoot = path.resolve(runtime, '..');
const oracleScript = path.join(testdata, 'oracle.cjs');
const reflowOracleScript = path.join(testdata, 'reflow-oracle.ts');
const recordingsDir = path.join(testdata, 'recordings');
const go = '/opt/homebrew/bin/go';
const tsx = path.join(repoRoot, 'runtime/testdata/node-runtime/node_modules/.bin/tsx');
const reflowWidth = 60;
const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'sessions-mirror-acceptance-'));

function run(command, args, options = {}) {
  const child = spawnSync(command, args, {
    cwd: options.cwd ?? repoRoot,
    env: { ...process.env, ...options.env },
    encoding: 'utf8',
    maxBuffer: 64 * 1024 * 1024
  });
  if (child.status !== 0) {
    throw new Error(`${command} ${args.join(' ')} failed (${child.status})\n${child.stdout}\n${child.stderr}`);
  }
  return child.stdout.trim();
}

function oracle(input, cols = 300, rows = 50) {
  return JSON.parse(run(process.execPath, [oracleScript, input, String(cols), String(rows)]));
}

function firstDifference(left, right) {
  const length = Math.max(left.length, right.length);
  for (let i = 0; i < length; i++) {
    if (left[i] !== right[i]) {
      return { offset: i, left: left.slice(i, i + 24), right: right.slice(i, i + 24) };
    }
  }
  return null;
}

try {
  const harness = path.join(tmp, 'mirror-harness');
  run(go, ['build', '-o', harness, './internal/mirror/cmd/harness'], {
    cwd: runtime,
    env: { CGO_ENABLED: '0' }
  });

  const recordings = fs.readdirSync(recordingsDir)
    .filter((name) => name.endsWith('.bin'))
    .sort();
  assert.ok(recordings.length > 0, 'no recordings found');

  const probes = new Map([
    ['sgr-controls.probe', Buffer.from(
      '\x1b[1;2;3;4;5;7;9;38;5;202;48;2;90;80;70mstyled\x1b[0m' +
      '\r\n\x1b[3Cgap\x1b[1D!\x1b[K')],
    ['alternate-screen.probe', Buffer.from(
      'main\x1b[?1049h\x1b[2J\x1b[H\x1b[38;2;7;81;205malt screen\x1b[0m')],
    ['wrap-scroll.probe', Buffer.from(
      'w'.repeat(300) + 'W\r\n' +
      Array.from({ length: 54 }, (_, i) => `line-${String(i).padStart(2, '0')}\r\n`).join(''))],
    ['unicode-controls.probe', Buffer.from(
      'ASCII e\u0301 | CJK 界面 | emoji 🙂👨‍💻\r\n' +
      '\x1b]8;id=docs;https://example.com\x07linked\x1b]8;;\x07')]
  ]);
  const cases = recordings.map((name) => ({ name, input: path.join(recordingsDir, name) }));
  for (const [name, raw] of probes) {
    const input = path.join(tmp, name);
    fs.writeFileSync(input, raw);
    cases.push({ name, input });
  }

  const divergenceName = 'sgr-overline.probe';
  const divergenceInput = path.join(tmp, divergenceName);
  fs.writeFileSync(divergenceInput, Buffer.from('\x1b[53moverline\x1b[0m'));

  let passed = 0;
  console.log(`mirror oracle acceptance (${recordings.length} PTY recordings + ${probes.size} control probes, 300x50, reflow=${reflowWidth})`);
  for (const { name, input } of cases) {
    const source = oracle(input);
    const goResult = JSON.parse(run(harness, ['-input', input, '-reflow', String(reflowWidth)]));
    const serializedPath = path.join(tmp, `${name}.go-serialized.bin`);
    fs.writeFileSync(serializedPath, goResult.serialized);
    const roundTrip = oracle(serializedPath);

    assert.equal(goResult.snapshot, source.snapshot,
      `${name}: snapshot diff ${JSON.stringify(firstDifference(goResult.snapshot, source.snapshot))}`);
    assert.equal(goResult.roundTripSnapshot, source.snapshot,
      `${name}: Go self-roundtrip snapshot differs`);
    assert.equal(roundTrip.snapshot, source.snapshot,
      `${name}: xterm render of Go serialization differs`);
    assert.deepEqual(roundTrip.canonical, source.canonical,
      `${name}: styled-cell render of Go serialization differs`);

    const tsReflow = JSON.parse(run(tsx, [reflowOracleScript, input, String(reflowWidth)])).text;
    const tsReflowPath = path.join(tmp, `${name}.ts-reflow.bin`);
    const goReflowPath = path.join(tmp, `${name}.go-reflow.bin`);
    fs.writeFileSync(tsReflowPath, tsReflow);
    fs.writeFileSync(goReflowPath, goResult.reflow);
    const tsRendered = oracle(tsReflowPath, reflowWidth, 600);
    const goRendered = oracle(goReflowPath, reflowWidth, 600);
    assert.equal(goRendered.snapshot, tsRendered.snapshot,
      `${name}: reflow text render differs`);
    assert.deepEqual(goRendered.canonical, tsRendered.canonical,
      `${name}: reflow styled-cell render differs`);

    console.log(`  PASS ${name}: snapshot exact; ANSI roundtrip cells exact; reflow render exact`);
    passed++;
  }
  console.log(`${passed}/${cases.length} cases passed`);

  const divergenceSource = oracle(divergenceInput);
  const divergenceGo = JSON.parse(run(harness, ['-input', divergenceInput, '-reflow', String(reflowWidth)]));
  const divergenceSerialized = path.join(tmp, `${divergenceName}.go-serialized.bin`);
  fs.writeFileSync(divergenceSerialized, divergenceGo.serialized);
  const divergenceRoundTrip = oracle(divergenceSerialized);
  assert.equal(divergenceGo.snapshot, divergenceSource.snapshot,
    `${divergenceName}: text should still match`);
  assert.notDeepEqual(divergenceRoundTrip.canonical, divergenceSource.canonical,
    `${divergenceName}: update the documented result; SGR 53 now round-trips`);
  console.log('  KNOWN DIVERGENCE sgr-overline.probe bytes=1b5b35336d6f7665726c696e651b5b306d: text exact; x/vt omits SGR 53 overline metadata');
} finally {
  fs.rmSync(tmp, { recursive: true, force: true });
}
