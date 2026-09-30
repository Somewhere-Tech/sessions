#!/usr/bin/env node
import { createHash } from 'node:crypto';
import { lstat, readFile, writeFile } from 'node:fs/promises';
import { resolve } from 'node:path';
const [operation, directory, version] = process.argv.slice(2);
const hash = (bytes) => createHash('sha256').update(bytes).digest('hex');
try {
  if (!['verify', 'render'].includes(operation) || !directory || !/^\d+\.\d+\.\d+$/.test(version)) throw new Error('Usage: runtime-signing-manifest.mjs verify|render DIRECTORY VERSION');
  const names = ['sessions', 'sessionsd', 'sessions-runner'];
  const binaries = {};
  for (const name of names) {
    const file = resolve(directory, name);
    if (!(await lstat(file)).isFile()) throw new Error(`runtime binary must be a regular file: ${name}`);
    binaries[name] = hash(await readFile(file));
  }
  const fingerprint = hash(`${names.map((name) => binaries[name]).join('\n')}\n`).slice(0, 12);
  const manifest = { schemaVersion: 1, runtimeVersion: `v${version}-bin.${fingerprint}`, target: 'darwin-arm64', binaries };
  const file = resolve(directory, 'runtime-manifest.json');
  if (operation === 'render') await writeFile(file, `${JSON.stringify(manifest, null, 2)}\n`);
  else {
    const existing = JSON.parse(await readFile(file, 'utf8'));
    if (existing.schemaVersion !== manifest.schemaVersion || existing.runtimeVersion !== manifest.runtimeVersion || existing.target !== manifest.target || JSON.stringify(existing.binaries) !== JSON.stringify(binaries)) throw new Error('bundled runtime manifest does not match the expected release and bytes');
  }
} catch (error) { console.error(error.message); process.exitCode = 1; }
