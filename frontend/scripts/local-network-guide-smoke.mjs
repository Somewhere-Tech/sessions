// The nearby-access guide may only claim what was actually observed.
//
// The host's permission status is its last observation — macOS exposes no API
// for the Local Network switch — so a stored `granted` is history, and a check
// that finds nothing or fails is fresher evidence than that history.
import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { build } from 'esbuild';

const scratch = await mkdtemp(join(tmpdir(), 'sessions-local-network-smoke-'));
const output = join(scratch, 'local-network-guide.mjs');

try {
  await build({
    entryPoints: ['src/lib/localNetworkGuide.ts'],
    bundle: true, format: 'esm', outfile: output, platform: 'node', target: 'node20', logLevel: 'silent'
  });
  const { localNetworkGuideMode } = await import(`${pathToFileURL(output).href}?v=${Date.now()}`);
  const mode = (status, sawUnproven, lastCheck) => localNetworkGuideMode({ status, sawUnproven, lastCheck });

  // Fresh contact is the only thing that proves nearby access works now.
  assert.equal(mode('granted', true, 'reached'), 'confirmed');
  assert.equal(mode('not-yet-asked', true, 'reached'), 'confirmed');

  // A stored success plus an empty discovery is not a restored connection. The
  // browse answered 200 and nothing was there; saying "working" would be a
  // claim about a network nobody contacted.
  assert.equal(mode('granted', true, 'none-found'), 'guide');

  // A failed check after an earlier success must not be swallowed by that
  // success: the guide stays up so the fresh error can be read.
  assert.equal(mode('granted', true, 'failed'), 'guide');
  assert.equal(mode('denied', true, 'failed'), 'guide');

  // Without fresh evidence, a stored success is reported as history, never as a
  // live connection.
  assert.equal(mode('granted', true, 'none'), 'previously-worked');

  // Someone who never saw a problem is not shown a recovery surface at all, and
  // platforms without the permission never see one.
  assert.equal(mode('granted', false, 'none'), 'hidden');
  assert.equal(mode('not-required', true, 'failed'), 'hidden');
  assert.equal(mode(undefined, false, 'none'), 'hidden');

  // The unproven and older-host states both open the guide.
  assert.equal(mode('not-yet-asked', true, 'none'), 'guide');
  assert.equal(mode('denied', true, 'none'), 'guide');

  console.log('local-network guide smoke passed');
} finally {
  await rm(scratch, { recursive: true, force: true });
}
