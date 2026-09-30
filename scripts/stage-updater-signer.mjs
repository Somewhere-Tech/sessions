#!/usr/bin/env node
import { createHash } from 'node:crypto';
import { readFile, mkdir, writeFile } from 'node:fs/promises';
import { gunzipSync } from 'node:zlib';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const { download } = require('../npm/lib/download.cjs');
const root = fileURLToPath(new URL('../', import.meta.url));

export function verifyToolArchive(archive, entry) {
  if (!/^sha512-[A-Za-z0-9+/]+={0,2}$/.test(entry.integrity)) throw new Error('signer tool must have a SHA-512 lock pin');
  const actual = `sha512-${createHash('sha512').update(archive).digest('base64')}`;
  if (actual !== entry.integrity) throw new Error('signer tool archive does not match the reviewed lockfile');
  const bytes = gunzipSync(archive, { maxOutputLength: 128 * 1024 * 1024 });
  let result;
  for (let offset = 0; offset + 512 <= bytes.length;) {
    const header = bytes.subarray(offset, offset + 512);
    if (header.every((byte) => byte === 0)) break;
    const string = (start, size) => header.subarray(start, start + size).toString().replace(/\0.*$/s, '');
    const name = string(0, 100);
    const size = parseInt(string(124, 12).trim(), 8);
    if (!Number.isSafeInteger(size) || size < 0 || offset + 512 + size > bytes.length) throw new Error('invalid signer tool archive');
    if (name === 'package/cli.darwin-arm64.node') {
      if (result || !['', '0'].includes(string(156, 1))) throw new Error('signer native addon must be one regular file');
      result = bytes.subarray(offset + 512, offset + 512 + size);
    }
    offset += 512 + Math.ceil(size / 512) * 512;
  }
  if (!result?.length) throw new Error('locked signer archive is missing its native addon');
  return result;
}

export async function stage(output) {
  const lock = JSON.parse(await readFile(resolve(root, 'package-lock.json'), 'utf8'));
  const entry = lock.packages['node_modules/@tauri-apps/cli-darwin-arm64'];
  const expected = `https://registry.npmjs.org/@tauri-apps/cli-darwin-arm64/-/cli-darwin-arm64-${entry.version}.tgz`;
  if (entry.resolved !== expected) throw new Error('signer tool URL is not the official versioned registry package');
  const addon = verifyToolArchive(await download(entry.resolved), entry);
  await mkdir(output, { recursive: false, mode: 0o700 });
  await writeFile(resolve(output, 'signer.node'), addon, { mode: 0o600, flag: 'wx' });
  await writeFile(resolve(output, 'signer.sha256'), createHash('sha256').update(addon).digest('hex'), { mode: 0o600, flag: 'wx' });
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  if (process.argv.length !== 3) { console.error('Usage: stage-updater-signer.mjs NEW_OUTPUT_DIRECTORY'); process.exitCode = 2; }
  else stage(resolve(process.argv[2])).catch((error) => { console.error(error.message); process.exitCode = 1; });
}
