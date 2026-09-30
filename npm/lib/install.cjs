'use strict';
const fs = require('node:fs/promises');
const path = require('node:path');
const os = require('node:os');
const manifest = require('../release-manifest.json');
const pkg = require('../package.json');
const { download } = require('./download.cjs');
const { commands, extract, sha256 } = require('./archive.cjs');

function release(platform = process.platform, architecture = process.arch, releaseManifest = manifest) {
  const arch = { x64: 'amd64', arm64: 'arm64' }[architecture];
  const key = `${platform}_${arch || architecture}`;
  if (!['darwin_arm64', 'linux_arm64', 'linux_amd64'].includes(key)) {
    throw new Error(`unsupported platform ${platform}/${architecture}; supported archives: macOS ARM64, Linux ARM64 and AMD64 (WSL2 uses Linux archives; native Windows is not included)`);
  }
  const entry = releaseManifest.platforms[key];
  if (releaseManifest.version !== pkg.version || !entry || !/^[a-f0-9]{64}$/.test(entry.sha256)) {
    throw new Error(`release ${pkg.version} has not been prepared for ${key}; use a published package with verified runtime assets`);
  }
  for (const command of commands) {
    if (!/^[a-f0-9]{64}$/.test(entry.binaries?.[command])) throw new Error(`release manifest has no checksum for ${command}`);
  }
  return { key, entry };
}

function directory() {
  const { key, entry } = release();
  return path.join(os.homedir(), '.local', 'share', 'sessions', 'npm', `${pkg.version}-${key}-${entry.sha256}`);
}

async function verify(directoryPath, entry) {
  for (const command of commands) {
    const file = path.join(directoryPath, command);
    const stat = await fs.lstat(file);
    if (!stat.isFile() || (stat.mode & 0o111) === 0 || sha256(await fs.readFile(file)) !== entry.binaries[command]) {
      throw new Error(`cached runtime failed verification at ${file}; keep live processes running and reinstall into a clean cache after they end`);
    }
  }
}

async function install() {
  const { entry } = release();
  const target = directory();
  let existing;
  try { existing = await fs.lstat(target); } catch (error) {
    if (error.code !== 'ENOENT') throw error;
  }
  if (existing) {
    if (!existing.isDirectory()) throw new Error(`runtime cache is not a directory: ${target}`);
    await verify(target, entry);
    return target;
  }
  await fs.mkdir(path.dirname(target), { recursive: true, mode: 0o700 });
  const stage = await fs.mkdtemp(path.join(path.dirname(target), '.staging-'));
  try {
    const archive = await download(entry.url);
    if (sha256(archive) !== entry.sha256) throw new Error('release archive checksum mismatch; downloaded bytes were refused, retry with a verified release');
    const files = extract(archive);
    for (const command of commands) {
      if (sha256(files.get(command)) !== entry.binaries[command]) throw new Error(`release ${command} checksum mismatch`);
      await fs.writeFile(path.join(stage, command), files.get(command), { mode: 0o755, flag: 'wx' });
    }
    try { await fs.rename(stage, target); }
    catch (error) {
      if (!['EEXIST', 'ENOTEMPTY'].includes(error.code)) throw error;
      await verify(target, entry);
    }
    return target;
  } finally { await fs.rm(stage, { recursive: true, force: true }); }
}

module.exports = { release, directory, install, verify };
