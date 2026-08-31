import assert from 'node:assert/strict';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { build } from 'esbuild';

const scratch = await mkdtemp(join(tmpdir(), 'sessions-title-clarity-'));
const entry = join(scratch, 'entry.ts');
const output = join(scratch, 'entry.mjs');

try {
  await writeFile(entry, [
    `export { sessionLabel, sessionTitleFromPrompt } from ${JSON.stringify(new URL('../src/lib/tabLabels.ts', import.meta.url).pathname)};`,
    `export { serverDisplayName } from ${JSON.stringify(new URL('../src/lib/servers.ts', import.meta.url).pathname)};`
  ].join('\n'));
  await build({
    entryPoints: [entry], bundle: true, format: 'esm', outfile: output,
    platform: 'node', target: 'node20', logLevel: 'silent'
  });
  globalThis.window = {
    localStorage: { getItem: () => null, setItem: () => {}, removeItem: () => {} },
    location: { protocol: 'file:', port: '', hostname: 'localhost' }
  };
  const { sessionLabel, sessionTitleFromPrompt, serverDisplayName } = await import(
    `${pathToFileURL(output).href}?v=${Date.now()}`
  );

  assert.equal(
    sessionTitleFromPrompt('Hey, I want you to check the external hard drives I have connected'),
    'Check the external hard drives I have connected'
  );
  assert.equal(
    sessionTitleFromPrompt('can you look at this repo and tell me whether it is live on Somewhere?'),
    'Check whether this repo is live on Somewhere?'
  );
  assert.equal(
    sessionTitleFromPrompt('I want to check the external hard drives I have connected'),
    'Check the external hard drives I have connected'
  );
  assert.equal(
    sessionTitleFromPrompt('This chat is about TagTheWall and its deployment'),
    'TagTheWall and its deployment'
  );
  assert.equal(sessionLabel({ id: '1', name: 'PM', name_source: 'explicit' }), 'PM');
  assert.equal(sessionLabel({
    id: '2', name: 'hey I want you to inspect this', name_source: 'launch',
    description: 'hey I want you to inspect this repository carefully'
  }), 'Inspect this repository carefully');
  assert.equal(sessionLabel({
    id: '3', name: 'Fable — Sessions analysis',
    description: 'Independent product and lifecycle review for organizing stale windows'
  }), 'Fable — Sessions analysis');
  assert.equal(sessionLabel({
    id: '4', name: 'Hey i want to check the external hard drives I have connected'
  }), 'Check the external hard drives I have connected');

  const local = {
    id: 'local', name: 'This machine', systemName: 'MacBook-Pro-10.local',
    host: 'localhost', port: 8787, isDefault: true
  };
  const mini = {
    id: 'mini', name: 'Mac-mini-249.local', systemName: 'Mac-mini-249.local',
    host: 'mini', port: 8787, isDefault: false
  };
  assert.equal(serverDisplayName(local, true), 'This Mac');
  assert.equal(serverDisplayName(mini, true), 'Mac mini');
  assert.equal(serverDisplayName({ ...mini, customName: 'Studio' }, true), 'Studio');

  console.log('title clarity smoke passed');
} finally {
  delete globalThis.window;
  await rm(scratch, { recursive: true, force: true });
}
