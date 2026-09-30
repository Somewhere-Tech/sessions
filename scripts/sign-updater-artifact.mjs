#!/usr/bin/env node
import { createHash } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { resolve } from 'node:path';
const require = createRequire(import.meta.url);
const [toolDirectory, keyPath, artifact] = process.argv.slice(2);
try {
  if (!toolDirectory || !keyPath || !artifact || process.argv.length !== 5) throw new Error('Usage: sign-updater-artifact.mjs TOOL_DIRECTORY KEY_PATH ARTIFACT');
  const addon = resolve(toolDirectory, 'signer.node');
  const expected = (await readFile(resolve(toolDirectory, 'signer.sha256'), 'utf8')).trim();
  if (createHash('sha256').update(await readFile(addon)).digest('hex') !== expected) throw new Error('staged signing tool changed after integrity verification');
  const binding = require(addon);
  const args = ['signer', 'sign', '--private-key-path', resolve(keyPath), resolve(artifact)];
  await new Promise((accept, reject) => binding.run(args, 'sessions-updater-signer', (error, result) => error ? reject(error) : accept(result)));
} catch (error) { console.error(`Updater signing failed: ${error.message}`); process.exitCode = 1; }
