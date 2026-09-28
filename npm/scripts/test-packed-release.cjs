#!/usr/bin/env node
'use strict';
const fs = require('node:fs/promises');
const path = require('node:path');
const os = require('node:os');
const { execFile } = require('node:child_process');
const { promisify } = require('node:util');
const run = promisify(execFile);
const { extract, sha256, commands } = require('../lib/archive.cjs');

async function testPackedRelease(tarball, manifestPath, archiveDirectory) {
  const manifest = JSON.parse(await fs.readFile(manifestPath, 'utf8'));
  const platform = `${process.platform}_${process.arch === 'x64' ? 'amd64' : process.arch}`;
  const entry = manifest.platforms[platform];
  if (!entry) throw new Error(`packed release test has no native target for ${platform}`);
  const archive = await fs.readFile(path.join(archiveDirectory, `sessions_${manifest.version}_${platform}.tar.gz`));
  if (sha256(archive) !== entry.sha256) throw new Error('native acceptance archive does not match npm manifest');
  const files = extract(archive);
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'sP-'));
  try {
    const home = path.join(root, 'home');
    const cache = path.join(home, '.local/share/sessions/npm', `${manifest.version}-${platform}-${entry.sha256}`);
    await fs.mkdir(cache, { recursive: true, mode: 0o700 });
    for (const command of commands) {
      if (sha256(files.get(command)) !== entry.binaries[command]) throw new Error(`native acceptance ${command} does not match npm pin`);
      await fs.writeFile(path.join(cache, command), files.get(command), { mode: 0o755 });
    }
    const prefix = path.join(root, 'global');
    const env = { ...process.env, HOME: home, SESSIONS_STATE_DIR: path.join(root, 'runners'),
      SESSIONS_LEDGER_PATH: path.join(root, 'lanes.sqlite3'), SESSIONS_PORT: '18879', npm_config_cache: path.join(root, 'npm-cache') };
    await run('npm', ['install', '--global', '--prefix', prefix, '--ignore-scripts', '--offline', path.resolve(tarball)], { env });
    const executable = path.join(prefix, 'bin/sessions');
    await run(executable, ['help'], { env });
    const version = (await run(executable, ['version'], { env })).stdout.trim();
    if (version !== manifest.version && version !== `v${manifest.version}`) throw new Error(`packed native CLI reports ${version}, expected ${manifest.version}`);
    console.log(`Packed release CLI verified on ${platform}. Service, provider, reboot, and other architecture acceptance remain separate.`);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
}

if (require.main === module) {
  if (process.argv.length !== 5) { console.error('Usage: test-packed-release.cjs PACKAGE_TARBALL MANIFEST_PATH ARCHIVE_DIRECTORY'); process.exitCode = 2; }
  else testPackedRelease(...process.argv.slice(2)).catch((error) => { console.error(error.message); process.exitCode = 1; });
}
module.exports = { testPackedRelease };
