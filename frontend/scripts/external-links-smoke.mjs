import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { build } from 'esbuild';

const scratch = await mkdtemp(join(tmpdir(), 'sessions-external-links-'));
const output = join(scratch, 'external-links.mjs');
try {
  await build({
    entryPoints: ['src/lib/externalLinks.ts'],
    bundle: true,
    format: 'esm',
    outfile: output,
    platform: 'node',
    target: 'node20',
    external: ['react'],
    logLevel: 'silent'
  });
  const { externalLinkTarget } = await import(`${pathToFileURL(output).href}?v=${Date.now()}`);
  const base = 'tauri://localhost/sessions';
  assert.equal(externalLinkTarget('https://somewhere.tech/docs', base), 'https://somewhere.tech/docs');
  assert.equal(externalLinkTarget('mailto:hello@somewhere.tech', base), 'mailto:hello@somewhere.tech');
  assert.equal(externalLinkTarget('vscode://file/Users/test/main.go:8', base), 'vscode://file/Users/test/main.go:8');
  assert.equal(externalLinkTarget('#message-4', base), null);
  assert.equal(externalLinkTarget('javascript:alert(1)', base), null);
  assert.equal(externalLinkTarget('data:text/html,hello', base), null);
} finally {
  await rm(scratch, { recursive: true, force: true });
}

console.log('external link smoke: ok');
