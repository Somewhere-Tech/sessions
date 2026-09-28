#!/usr/bin/env node
import { createHash } from 'node:crypto';
import { readdir, readFile, writeFile, lstat } from 'node:fs/promises';
import { resolve, relative } from 'node:path';
import { fileURLToPath } from 'node:url';
const hash = (bytes) => createHash('sha256').update(bytes).digest('hex');

export function expectedFiles(version, stage) {
  if (!/^\d+\.\d+\.\d+$/.test(version) || !['unsigned', 'signed', 'delivery'].includes(stage)) throw new Error('invalid release version or stage');
  const cli = ['darwin_arm64', 'linux_arm64', 'linux_amd64'].flatMap((platform) => {
    const name = `cli/sessions_${version}_${platform}.tar.gz`;
    return [name, `${name}.sha256`];
  });
  if (stage === 'unsigned') return ['Sessions-unsigned.zip', ...cli].sort();
  const result = ['Sessions.app.tar.gz', 'Sessions.app.tar.gz.sig', 'Sessions.app.tar.gz.sha256',
    `Sessions_${version}_darwin_arm64.zip`, `Sessions_${version}_darwin_arm64.zip.sha256`, 'latest.json', 'checksums.txt', ...cli];
  if (stage === 'delivery') result.push(`somewhere-tech-sessions-${version}.tgz`, `somewhere-tech-sessions-${version}.tgz.sha256`, 'npm-manifest.json');
  return result.sort();
}

async function inventory(directory) {
  const files = [];
  let total = 0;
  async function walk(subdirectory) {
    for (const entry of await readdir(subdirectory)) {
      const full = resolve(subdirectory, entry);
      const stat = await lstat(full);
      if (stat.isSymbolicLink()) throw new Error(`release artifact is a symlink: ${entry}`);
      if (stat.isDirectory()) await walk(full);
      else if (stat.isFile()) { files.push(relative(directory, full)); total += stat.size; }
      else throw new Error(`release artifact is not a regular file: ${entry}`);
    }
  }
  await walk(directory);
  if (total > 1024 * 1024 * 1024) throw new Error('release artifacts exceed 1 GiB');
  return files.filter((name) => name !== 'release-inputs.json').sort();
}

export async function record(directory, version, commit, stage) {
  if (!/^[a-f0-9]{40}$/.test(commit)) throw new Error('release inputs require a full source commit');
  const files = await inventory(directory);
  if (JSON.stringify(files) !== JSON.stringify(expectedFiles(version, stage))) throw new Error('release artifact set does not match the expected stage');
  const digests = {};
  for (const name of files) digests[name] = hash(await readFile(resolve(directory, name)));
  const manifest = `${JSON.stringify({ schema: 1, version, commit, stage, digests }, null, 2)}\n`;
  await writeFile(resolve(directory, 'release-inputs.json'), manifest);
  return hash(manifest);
}

export async function verify(directory, version, commit, stage, expectedDigest) {
  const bytes = await readFile(resolve(directory, 'release-inputs.json'));
  if (!/^[a-f0-9]{64}$/.test(expectedDigest) || hash(bytes) !== expectedDigest) throw new Error('release input manifest does not match the producing job');
  const manifest = JSON.parse(bytes);
  if (manifest.schema !== 1 || manifest.version !== version || manifest.commit !== commit || manifest.stage !== stage) throw new Error('release input provenance does not match the requested source and stage');
  const names = expectedFiles(version, stage);
  if (JSON.stringify(await inventory(directory)) !== JSON.stringify(names) || JSON.stringify(Object.keys(manifest.digests).sort()) !== JSON.stringify(names)) throw new Error('release inputs contain missing or unexpected files');
  for (const name of names) {
    if (hash(await readFile(resolve(directory, name))) !== manifest.digests[name]) throw new Error(`release input checksum mismatch: ${name}`);
  }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const [operation, directory, version, commit, stage, digest] = process.argv.slice(2);
  const action = operation === 'record' ? record : operation === 'verify' ? verify : undefined;
  if (!action || !directory || !stage) { console.error('Usage: release-artifacts.mjs record|verify DIRECTORY VERSION COMMIT STAGE [MANIFEST_SHA256]'); process.exitCode = 2; }
  else action(resolve(directory), version, commit, stage, digest).then((result) => { if (result) console.log(result); }).catch((error) => { console.error(error.message); process.exitCode = 1; });
}
