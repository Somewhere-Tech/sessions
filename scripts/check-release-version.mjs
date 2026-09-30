#!/usr/bin/env node
import { readFile } from 'node:fs/promises';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
const root = fileURLToPath(new URL('../', import.meta.url));
const version = process.argv[2];
try {
  if (!/^\d+\.\d+\.\d+$/.test(version)) throw new Error('release version must be stable semver');
  for (const file of ['package.json', 'frontend/package.json', 'src-tauri/tauri.conf.json', 'npm/package.json']) {
    if (JSON.parse(await readFile(resolve(root, file), 'utf8')).version !== version) throw new Error(`${file} does not match ${version}`);
  }
  const cargo = await readFile(resolve(root, 'src-tauri/Cargo.toml'), 'utf8');
  if (cargo.match(/^version = "([^"]+)"/m)?.[1] !== version) throw new Error('Cargo.toml does not match release version');
  const site = await readFile(resolve(root, 'site/index.html'), 'utf8');
  if (!site.includes(`v${version}/`)) throw new Error('site download links do not match release version');
  await readFile(resolve(root, `release/notes-v${version}.md`));
  for (const command of ['sessions', 'sessionsd', 'sessions-runner', 'sessions-relay']) {
    const main = await readFile(resolve(root, `runtime/cmd/${command}/main.go`), 'utf8');
    const declared = main.match(/^var version = "([^"]+)"/m)?.[1];
    if (declared && declared !== version) throw new Error(`${command} source version does not match release version`);
  }
} catch (error) { console.error(`Refusing release: ${error.message}`); process.exitCode = 1; }
