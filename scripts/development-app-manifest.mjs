#!/usr/bin/env node
// Development artifacts are receipts from CI, never stable release manifests.
import { createHash } from 'node:crypto';
import { createReadStream } from 'node:fs';
import { lstat, readFile, writeFile } from 'node:fs/promises';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { basename, join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';

const run = promisify(execFile);
export const runtimeNames = ['sessions', 'sessionsd', 'sessions-runner'];
const digest = (value) => createHash('sha256').update(value).digest('hex');
const same = (left, right) => JSON.stringify(left) === JSON.stringify(right);

export function identity(sourceCommit, runId, runAttempt, appVersion) {
  if (!/^[a-f0-9]{40}$/.test(sourceCommit) || !/^[1-9][0-9]*$/.test(runId) || !/^[1-9][0-9]*$/.test(runAttempt) || !/^\d+\.\d+\.\d+$/.test(appVersion)) {
    throw new Error('Expected full source SHA, positive workflow run ID and attempt, and app version');
  }
  return { repository: 'Somewhere-Tech/sessions', sourceCommit, workflow: '.github/workflows/ci.yml', job: 'dev_app', runId, runAttempt, appVersion };
}

export function developmentRuntime(appVersion, sourceCommit, binaries) {
  if (!same(Object.keys(binaries).sort(), [...runtimeNames].sort()) || Object.values(binaries).some((value) => !/^[a-f0-9]{64}$/.test(value))) throw new Error('Expected exactly three runtime SHA-256 hashes');
  const fingerprint = digest(`${runtimeNames.map((name) => binaries[name]).join('\n')}\n`).slice(0, 12);
  return { schemaVersion: 1, runtimeVersion: `v${appVersion}-dev.g${sourceCommit.slice(0, 12)}-bin.${fingerprint}`, target: 'darwin-arm64', binaries: Object.fromEntries(runtimeNames.map((name) => [name, binaries[name]])) };
}

export function checkRuntime(manifest, expected) {
  if (!same(manifest, expected)) throw new Error('Runtime manifest does not match development source and binary hashes');
}

export function checkReceipt(receipt, expected) {
  if (receipt.schemaVersion !== 1 || !same(receipt.provenance, expected) || receipt.target !== 'darwin-arm64' || receipt.bundleIdentifier !== 'tech.somewhere.sessions' || receipt.signing !== 'adhoc' || receipt.notarized !== false) throw new Error('Development artifact provenance does not match independently expected source and workflow run');
  checkRuntime(receipt.runtime, developmentRuntime(expected.appVersion, expected.sourceCommit, receipt.runtime?.binaries || {}));
  if (!/^[a-f0-9]{64}$/.test(receipt.appExecutableSHA256) || receipt.archive?.name !== `Sessions-development-${expected.sourceCommit}-darwin-arm64.zip` || !/^[a-f0-9]{64}$/.test(receipt.archive?.sha256) || !Number.isSafeInteger(receipt.archive?.bytes) || receipt.archive.bytes < 1 || receipt.archive.bytes > 512 * 1024 * 1024) throw new Error('Invalid development artifact checksums');
}

async function hashFile(file) {
  const stat = await lstat(file);
  if (!stat.isFile()) throw new Error(`Expected regular file: ${file}`);
  const hash = createHash('sha256');
  for await (const chunk of createReadStream(file)) hash.update(chunk);
  return { sha256: hash.digest('hex'), bytes: stat.size };
}

// The injectable inspector is only for fixture tests; the CLI always uses macOS
// plist, architecture, and code signature inspection without executing the app.
export async function inspectBundle(app, expected, inspect = nativeInspect, allowOuterInvalid = false) {
  for (const path of [app, join(app, 'Contents'), join(app, 'Contents/MacOS'), join(app, 'Contents/Resources'), join(app, 'Contents/Resources/runtime')]) {
    if (!(await lstat(path)).isDirectory()) throw new Error(`Expected real bundle directory: ${path}`);
  }
  const plist = join(app, 'Contents/Info.plist');
  await hashFile(plist);
  if (await inspect.plist(plist, 'CFBundleIdentifier') !== 'tech.somewhere.sessions' || await inspect.plist(plist, 'CFBundleShortVersionString') !== expected.appVersion || await inspect.plist(plist, 'CFBundleVersion') !== expected.appVersion || await inspect.plist(plist, 'CFBundleExecutable') !== 'sessions-app') throw new Error('Bundle identifier, executable, or version mismatch');
  const appExecutable = join(app, 'Contents/MacOS/sessions-app');
  const appExecutableSHA256 = (await hashFile(appExecutable)).sha256;
  const binaries = {};
  for (const [name, file] of [['sessions-app', appExecutable], ...runtimeNames.map((name) => [name, join(app, 'Contents/Resources/runtime', name)])]) {
    if (await inspect.architecture(file) !== 'arm64') throw new Error(`Expected Darwin ARM64 executable: ${name}`);
    // Re-signing a nested binary invalidates the old outer resource seal.
    // During refresh, verify nested signatures before the app is signed again.
    if (name !== 'sessions-app' || !allowOuterInvalid) await inspect.signature(file);
    if (name !== 'sessions-app') binaries[name] = (await hashFile(file)).sha256;
  }
  return { runtime: developmentRuntime(expected.appVersion, expected.sourceCommit, binaries), appExecutableSHA256 };
}

const nativeInspect = {
  plist: async (file, key) => (await run('/usr/libexec/PlistBuddy', ['-c', `Print :${key}`, file])).stdout.trim(),
  architecture: async (file) => (await run('/usr/bin/lipo', ['-archs', file])).stdout.trim(),
  signature: async (file) => { await run('/usr/bin/codesign', ['--verify', '--deep', '--strict', file]); },
  signing: async (file) => (await run('/usr/bin/codesign', ['--display', '--verbose=2', file])).stderr.includes('Signature=adhoc') ? 'adhoc' : 'identity',
};

async function bundledManifest(app) {
  const file = join(app, 'Contents/Resources/runtime/runtime-manifest.json');
  await hashFile(file);
  return JSON.parse(await readFile(file, 'utf8'));
}

export async function operate(operation, app, archive, receiptFile, expected, inspect = nativeInspect) {
  const bundle = await inspectBundle(app, expected, inspect, operation === 'refresh-runtime');
  const manifest = await bundledManifest(app);
  if (operation === 'record') {
    checkRuntime(manifest, bundle.runtime);
    await inspect.signature(app);
    if (await inspect.signing(app) !== 'adhoc') throw new Error('Development CI bundle must have an ad-hoc signature');
    const receipt = { schemaVersion: 1, provenance: expected, target: 'darwin-arm64', bundleIdentifier: 'tech.somewhere.sessions', signing: 'adhoc', notarized: false, ...bundle, archive: { name: basename(archive), ...await hashFile(archive) } };
    checkReceipt(receipt, expected);
    await writeFile(receiptFile, `${JSON.stringify(receipt, null, 2)}\n`, { flag: 'wx' });
    return receipt;
  }
  await hashFile(receiptFile);
  const receipt = JSON.parse(await readFile(receiptFile, 'utf8'));
  checkReceipt(receipt, expected);
  if (operation === 'verify') {
    checkRuntime(manifest, bundle.runtime);
    if (!same(bundle.runtime, receipt.runtime) || bundle.appExecutableSHA256 !== receipt.appExecutableSHA256) throw new Error('Bundle binary checksum mismatch against CI receipt');
    const bytes = await hashFile(archive);
    if (basename(archive) !== receipt.archive.name || !same(bytes, { sha256: receipt.archive.sha256, bytes: receipt.archive.bytes })) throw new Error('Archive checksum mismatch against CI receipt');
    if (await inspect.signing(app) !== 'adhoc') throw new Error('Original CI bundle must have an ad-hoc signature');
  } else if (operation === 'refresh-runtime') {
    // Call after signing the three runtime binaries and before the outer app.
    // Retain the immutable CI receipt; only derive hashes from the signed bytes.
    checkRuntime(manifest, receipt.runtime);
    await writeFile(join(app, 'Contents/Resources/runtime/runtime-manifest.json'), `${JSON.stringify(bundle.runtime, null, 2)}\n`);
    return bundle;
  } else if (operation === 'verify-local') {
    checkRuntime(manifest, bundle.runtime);
  } else throw new Error('Unknown development manifest operation');
  await inspect.signature(app);
  return { provenance: expected, ...bundle };
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  try {
    const [operation, app, ...args] = process.argv.slice(2);
    if (!['record', 'verify', 'refresh-runtime', 'verify-local'].includes(operation)) throw new Error('Usage: development-app-manifest.mjs record|verify APP ZIP RECEIPT SHA RUN ATTEMPT VERSION; refresh-runtime|verify-local APP RECEIPT SHA RUN ATTEMPT VERSION');
    const needsArchive = ['record', 'verify'].includes(operation);
    if (args.length !== (needsArchive ? 6 : 5)) throw new Error('Missing development manifest arguments');
    const [archive, receipt, sha, runId, attempt, version] = needsArchive ? args : [undefined, ...args];
    const result = await operate(operation, resolve(app), archive && resolve(archive), resolve(receipt), identity(sha, runId, attempt, version));
    console.log(JSON.stringify(result, null, 2));
  } catch (error) { console.error(error.message); process.exitCode = 1; }
}
