// Standalone test for the JSONL resolution policy. Runs against synthetic
// temp dirs — does NOT touch the live daemon or ~/.claude.
//   node_modules/.bin/tsx scripts/test-jsonl-resolver.mjs
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { resolveJsonlPath, listJsonlFiles, encodeCwd } from '../src/jsonlResolver.js';

let pass = 0;
function ok(name) { pass++; console.log(`  ok  ${name}`); }

function tmpDirWith(files) {
  const d = fs.mkdtempSync(path.join(os.tmpdir(), 'jsonlres-'));
  for (const f of files) fs.writeFileSync(path.join(d, f), '{}\n');
  return d;
}

const U = 'aaaaaaaa-1111-2222-3333-444444444444';
const V = 'bbbbbbbb-5555-6666-7777-888888888888';

// 1. exact match wins, even with other files present and a newer sibling
{
  const d = tmpDirWith([`${V}.jsonl`, `${U}.jsonl`]);
  const r = resolveJsonlPath(d, U);
  assert.equal(r.reason, 'exact');
  assert.equal(r.path, path.join(d, `${U}.jsonl`));
  ok('exact: launch file present alongside others');
}

// 2. launch file missing, exactly one jsonl -> sole-file
{
  const d = tmpDirWith([`${V}.jsonl`]);
  const r = resolveJsonlPath(d, U);
  assert.equal(r.reason, 'sole-file');
  assert.equal(r.path, path.join(d, `${V}.jsonl`));
  ok('sole-file: launch missing, one conversation in dir');
}

// 3. launch file missing, multiple jsonl -> ambiguous (refuse to guess)
{
  const d = tmpDirWith([`${V}.jsonl`, 'cccccccc.jsonl']);
  const r = resolveJsonlPath(d, U);
  assert.equal(r.reason, 'ambiguous');
  assert.equal(r.path, null);
  ok('ambiguous: launch missing, multiple files -> null');
}

// 4. dir exists but no jsonl yet -> empty-dir
{
  const d = tmpDirWith([]);
  const r = resolveJsonlPath(d, U);
  assert.equal(r.reason, 'empty-dir');
  assert.equal(r.path, null);
  ok('empty-dir: no jsonl yet');
}

// 5. dir does not exist -> no-dir
{
  const r = resolveJsonlPath(path.join(os.tmpdir(), 'does-not-exist-' + Date.now()), U);
  assert.equal(r.reason, 'no-dir');
  assert.equal(r.path, null);
  ok('no-dir: project dir absent');
}

// 6. no launchUuid + single file -> sole-file
{
  const d = tmpDirWith([`${V}.jsonl`]);
  const r = resolveJsonlPath(d, undefined);
  assert.equal(r.reason, 'sole-file');
  ok('no launchUuid + single file -> sole-file');
}

// 7. no launchUuid + multiple -> ambiguous
{
  const d = tmpDirWith([`${V}.jsonl`, `${U}.jsonl`]);
  const r = resolveJsonlPath(d, undefined);
  assert.equal(r.reason, 'ambiguous');
  ok('no launchUuid + multiple -> ambiguous');
}

// 8. only .jsonl files count (ignore other files)
{
  const d = tmpDirWith([`${V}.jsonl`]);
  fs.writeFileSync(path.join(d, 'notes.txt'), 'x');
  fs.writeFileSync(path.join(d, `${U}.jsonl.tmp`), 'x');
  const r = resolveJsonlPath(d, U);
  assert.equal(r.reason, 'sole-file');
  assert.deepEqual(listJsonlFiles(d).sort(), [`${V}.jsonl`]);
  ok('non-jsonl files ignored');
}

// 9. encodeCwd matches Claude's dir convention
{
  assert.equal(encodeCwd('/Users/example/Projects/rail-me'), '-Users-example-Projects-rail-me');
  ok('encodeCwd: slashes -> dashes');
}

console.log(`\n${pass} assertions passed`);
