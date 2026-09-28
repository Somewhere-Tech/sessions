#!/usr/bin/env node
'use strict';
const fs = require('node:fs/promises');
const path = require('node:path');
const { commands, extract, sha256 } = require('../lib/archive.cjs');

async function prepare(archiveDirectory, packageDirectory) {
  const pkg = JSON.parse(await fs.readFile(path.join(packageDirectory, 'package.json'), 'utf8'));
  if (!/^\d+\.\d+\.\d+$/.test(pkg.version)) throw new Error('npm release version must be stable semver');
  const manifest = { version: pkg.version, platforms: {} };
  for (const platform of ['darwin_arm64', 'linux_arm64', 'linux_amd64']) {
    const name = `sessions_${pkg.version}_${platform}.tar.gz`;
    const archive = await fs.readFile(path.join(archiveDirectory, name));
    const checksum = (await fs.readFile(path.join(archiveDirectory, `${name}.sha256`), 'utf8')).trim();
    const match = checksum.match(/^([a-f0-9]{64})  (\S+)$/);
    if (!match || match[2] !== name || match[1] !== sha256(archive)) throw new Error(`release checksum file does not verify ${name}`);
    const files = extract(archive);
    manifest.platforms[platform] = {
      url: `https://github.com/Somewhere-Tech/sessions/releases/download/v${pkg.version}/${name}`,
      sha256: match[1],
      binaries: Object.fromEntries(commands.map((command) => [command, sha256(files.get(command))])),
    };
  }
  await fs.writeFile(path.join(packageDirectory, 'release-manifest.json'), `${JSON.stringify(manifest, null, 2)}\n`);
  return manifest;
}

if (require.main === module) {
  const args = process.argv.slice(2);
  if (args.length !== 2) {
    console.error('Usage: node npm/scripts/prepare-release.cjs ARCHIVE_DIRECTORY PACKAGE_DIRECTORY');
    process.exitCode = 2;
  } else prepare(path.resolve(args[0]), path.resolve(args[1])).then(() => {
    console.log('npm release manifest prepared from checksum-verified archives; publication has not occurred.');
  }).catch((error) => { console.error(`Cannot prepare npm release: ${error.message}`); process.exitCode = 1; });
}

module.exports = { prepare };
