import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

test('client-only Android builds replace desktop runtime resources', async () => {
  const readConfig = async (name) => JSON.parse(await readFile(new URL(`../src-tauri/${name}`, import.meta.url), 'utf8'));
  const desktop = await readConfig('tauri.conf.json');
  const android = await readConfig('tauri.android.conf.json');
  assert.ok(desktop.bundle.resources['runtime/'], 'desktop still needs its durable runner bundle');
  // An empty object would merge with and retain the desktop resource map.
  // An empty array replaces that map in Tauri's platform configuration merge.
  assert.deepEqual(android.bundle.resources, [], 'phones must not package desktop executables');
});
