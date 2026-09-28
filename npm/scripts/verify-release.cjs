#!/usr/bin/env node
'use strict';
const manifest = require('../release-manifest.json');
const pkg = require('../package.json');
const { release } = require('../lib/install.cjs');
const { download } = require('../lib/download.cjs');
const { commands, extract, sha256 } = require('../lib/archive.cjs');
const fs = require('node:fs');

async function verifyPublishedAssets(releaseManifest = manifest) {
  for (const [platform, arch] of [['darwin', 'arm64'], ['linux', 'arm64'], ['linux', 'x64']]) {
    const { key, entry } = release(platform, arch, releaseManifest);
    const name = `sessions_${pkg.version}_${key}.tar.gz`;
    const expectedURL = `https://github.com/Somewhere-Tech/sessions/releases/download/v${pkg.version}/${name}`;
    if (entry.url !== expectedURL) throw new Error(`release ${key} URL is not the exact official versioned asset`);
    const archive = await download(entry.url);
    if (sha256(archive) !== entry.sha256) throw new Error(`${key} published archive checksum mismatch`);
    const files = extract(archive);
    for (const command of commands) {
      if (sha256(files.get(command)) !== entry.binaries[command]) throw new Error(`${key} ${command} checksum mismatch`);
    }
  }
  if (Object.keys(releaseManifest.platforms).length !== 3) throw new Error('release manifest must contain exactly three supported platforms');
}

if (require.main === module) Promise.resolve().then(() => {
  const args = process.argv.slice(2);
  if (args.length === 0) return verifyPublishedAssets();
  if (args.length !== 2 || args[0] !== '--manifest') throw new Error('Usage: verify-release.cjs [--manifest MANIFEST_PATH]');
  return verifyPublishedAssets(JSON.parse(fs.readFileSync(args[1], 'utf8')));
}).then(() => {
  console.log('All versioned published runtime assets match the npm pins.');
}).catch((error) => { console.error(`Refusing npm publication: ${error.message}`); process.exitCode = 1; });

module.exports = { verifyPublishedAssets };
