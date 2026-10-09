// The account part of a New Session create request.
//
// sessionsd starts a same-provider delegate on its manager's account when the
// request names no account. So a person who picks Default for a delegate has
// to be sent as an explicit choice, or the daemon would quietly put the
// delegate back on the manager's account. Everyone else keeps leaving the
// account out, which an older daemon reads the same way.

import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { build } from 'esbuild';

const scratch = await mkdtemp(join(tmpdir(), 'sessions-account-choice-'));
try {
  const output = join(scratch, 'account-choice.mjs');
  await build({
    entryPoints: ['src/lib/accountChoice.ts'], bundle: true, outfile: output,
    format: 'esm', platform: 'neutral', logLevel: 'silent'
  });
  const { accountRequestFields } = await import(pathToFileURL(output).href);

  assert.deepEqual(accountRequestFields('', true), { defaultProfile: true },
    'Default under a profiled manager is an explicit choice');
  assert.deepEqual(accountRequestFields('work', true), { profile: 'work' },
    'a named account wins for a delegate');
  assert.deepEqual(accountRequestFields('work', false), { profile: 'work' });
  assert.deepEqual(accountRequestFields('', false), {},
    'a top-level session leaves the account out, as before');
  assert.equal(JSON.stringify({ cmd: 'claude', ...accountRequestFields('', true) }),
    '{"cmd":"claude","defaultProfile":true}');

  const dialog = await readFile(new URL('../src/components/NewSessionDialog.tsx', import.meta.url), 'utf8');
  assert.match(dialog, /\.\.\.accountRequestFields\(selectedProfile, Boolean\(parentSession && profileTool\)\)/,
    'the launcher builds its account fields with the shared rule');
  assert.doesNotMatch(dialog, /profile: selectedProfile \|\| undefined/,
    'the launcher no longer drops an explicit Default for a delegate');
  console.log('account choice smoke passed');
} finally {
  await rm(scratch, { recursive: true, force: true });
}
