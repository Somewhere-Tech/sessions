// Regression tests for bounded and full-scan Codex rollout resolution.
// Real metadata under ~/.codex and pretty-PTY runner state is read-only;
// the full-scan fixture writes only beneath a disposable temporary HOME.
//   npm run test:codex-resolver

import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import { codexFreshSessionDirs, resolveCodexRolloutPath } from '../src/codexResolver.js';

const sessionsDir = path.join(os.homedir(), '.codex', 'sessions');
const runnersDir = path.join(os.homedir(), '.local', 'state', 'pretty-PTY', 'runners');
const packageDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const resolverUrl = new URL('../src/codexResolver.ts', import.meta.url).href;
const realRunnerPrefixes = ['0cae0b90', '0ecc9dcd', '0fa852c4'];

function readSessionMeta(filePath) {
  const fd = fs.openSync(filePath, 'r');
  const buffer = Buffer.alloc(65_536);
  let firstLine;
  try {
    const bytesRead = fs.readSync(fd, buffer, 0, buffer.length, 0);
    firstLine = buffer.subarray(0, bytesRead).toString('utf8').split('\n', 1)[0];
  } finally {
    fs.closeSync(fd);
  }
  if (!firstLine) return null;

  try {
    const parsed = JSON.parse(firstLine);
    const cwd = parsed?.payload?.cwd;
    const timestamp = parsed?.payload?.timestamp ?? parsed?.timestamp;
    const timestampMs = typeof timestamp === 'string' ? Date.parse(timestamp) : Number.NaN;
    return typeof cwd === 'string' && Number.isFinite(timestampMs)
      ? { cwd, timestampMs }
      : null;
  } catch {
    return null;
  }
}

function writeRollout(filePath, cwd, timestampMs) {
  fs.mkdirSync(path.dirname(filePath), { recursive: true });
  fs.writeFileSync(filePath, `${JSON.stringify({
    timestamp: new Date(timestampMs).toISOString(),
    type: 'session_meta',
    payload: {
      cwd,
      timestamp: new Date(timestampMs).toISOString()
    }
  })}\n`);
}

function localDatePath(root, timestampMs) {
  const d = new Date(timestampMs);
  return path.join(
    root,
    String(d.getFullYear()),
    String(d.getMonth() + 1).padStart(2, '0'),
    String(d.getDate()).padStart(2, '0')
  );
}

function loadRealRunner(prefix) {
  const matches = fs.readdirSync(runnersDir)
    .filter((name) => name.startsWith(prefix) && name.endsWith('.json'));
  assert.equal(matches.length, 1, `expected one real runner matching ${prefix}`);

  const runner = JSON.parse(fs.readFileSync(path.join(runnersDir, matches[0]), 'utf8'));
  assert.equal(typeof runner.id, 'string');
  assert.equal(typeof runner.cwd, 'string');
  assert.ok(Array.isArray(runner.args));
  assert.equal(typeof runner.createdAt, 'number');
  return runner;
}

function findOlderRollout() {
  const legacyDirs = new Set(codexFreshSessionDirs());
  const candidates = [];

  for (const year of fs.readdirSync(sessionsDir).sort()) {
    const yearDir = path.join(sessionsDir, year);
    for (const month of fs.readdirSync(yearDir).sort()) {
      const monthDir = path.join(yearDir, month);
      for (const day of fs.readdirSync(monthDir).sort()) {
        const dayDir = path.join(monthDir, day);
        if (legacyDirs.has(dayDir)) continue;

        for (const name of fs.readdirSync(dayDir).sort()) {
          if (!name.startsWith('rollout-') || !name.endsWith('.jsonl')) continue;
          const filePath = path.join(dayDir, name);
          const meta = readSessionMeta(filePath);
          if (meta) candidates.push({ path: filePath, ...meta });
        }
      }
    }
  }

  candidates.sort((a, b) => {
    const ts = b.timestampMs - a.timestampMs;
    return ts !== 0 ? ts : a.path.localeCompare(b.path);
  });

  // Choose the earliest matching rollout at or after its own timestamp,
  // which is the resolver's deterministic selection policy.
  for (const candidate of candidates) {
    const expected = candidates
      .filter((other) =>
        path.dirname(other.path) === path.dirname(candidate.path)
        && other.cwd === candidate.cwd
        && other.timestampMs >= candidate.timestampMs)
      .sort((a, b) => {
        const ts = a.timestampMs - b.timestampMs;
        return ts !== 0 ? ts : a.path.localeCompare(b.path);
      })[0];
    if (expected?.path === candidate.path) return candidate;
  }

  throw new Error(`no older Codex rollout with session metadata found under ${sessionsDir}`);
}

test('resolves an older real rollout from the runner createdAt date', (t) => {
  const candidate = findOlderRollout();
  const result = resolveCodexRolloutPath({
    cwd: candidate.cwd,
    args: [],
    createdAt: candidate.timestampMs
  });

  assert.ok(
    !codexFreshSessionDirs().includes(path.dirname(candidate.path)),
    'fixture must be outside the legacy today/yesterday scan'
  );
  assert.equal(result.reason, 'fresh-match');
  assert.equal(result.path, candidate.path);
  t.diagnostic(`verified ${path.relative(sessionsDir, candidate.path)}`);
});

test('falls back to a full scan and accepts an older closest match', (t) => {
  const tempHome = fs.mkdtempSync(path.join(os.tmpdir(), 'pretty-pty-codex-resolver-'));
  t.after(() => fs.rmSync(tempHome, { recursive: true, force: true }));

  const tempSessionsDir = path.join(tempHome, '.codex', 'sessions');
  const createdAt = Date.now() - 7 * 24 * 60 * 60 * 1000;
  const cwd = '/tmp/pretty-pty-fullscan-target';
  const closestPath = path.join(tempSessionsDir, '2000', '01', '01', 'rollout-closest.jsonl');
  const laterPath = path.join(tempSessionsDir, '2000', '01', '02', 'rollout-much-later.jsonl');
  const boundedPath = path.join(localDatePath(tempSessionsDir, createdAt), 'rollout-other-cwd.jsonl');

  writeRollout(closestPath, cwd, createdAt - 60_000);
  writeRollout(laterPath, cwd, createdAt + 60 * 60_000);
  writeRollout(boundedPath, '/tmp/pretty-pty-other-cwd', createdAt + 1_000);

  const source = `
    import { resolveCodexRolloutPath } from ${JSON.stringify(resolverUrl)};
    const result = resolveCodexRolloutPath(${JSON.stringify({ cwd, args: [], createdAt })});
    process.stdout.write(JSON.stringify(result));
  `;
  const stdout = execFileSync(
    process.execPath,
    ['--import', 'tsx', '--input-type=module', '--eval', source],
    {
      cwd: packageDir,
      env: { ...process.env, HOME: tempHome },
      encoding: 'utf8'
    }
  );
  const result = JSON.parse(stdout);

  assert.equal(result.reason, 'fresh-match-fullscan');
  assert.equal(result.path, closestPath);
  assert.equal(result.ambiguousCount, 2);
});

test('resolves the three real runner regressions read-only', async (t) => {
  for (const prefix of realRunnerPrefixes) {
    await t.test(prefix, (st) => {
      const runner = loadRealRunner(prefix);
      const result = resolveCodexRolloutPath({
        cwd: runner.cwd,
        args: runner.args,
        createdAt: runner.createdAt
      });

      assert.notEqual(result.path, null, `${runner.id} still returned ${result.reason}`);
      assert.ok(fs.statSync(result.path).isFile(), `${result.path} is not a rollout file`);
      st.diagnostic(
        `${runner.id} -> ${path.relative(sessionsDir, result.path)} (${result.reason})`
      );
    });
  }
});
