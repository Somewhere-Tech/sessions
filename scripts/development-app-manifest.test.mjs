import assert from 'node:assert/strict';
import { mkdtemp, mkdir, writeFile, readFile, rm, copyFile } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { fileURLToPath } from 'node:url';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { runtimeNames, identity, developmentRuntime, checkRuntime, operate } from './development-app-manifest.mjs';

const sha = 'a'.repeat(40);
const expected = identity(sha, '12345', '2', '0.2.27');
const hash = (bytes) => createHash('sha256').update(bytes).digest('hex');
const inspector = {
  plist: async (_, key) => ({ CFBundleIdentifier: 'tech.somewhere.sessions', CFBundleShortVersionString: '0.2.27', CFBundleVersion: '0.2.27', CFBundleExecutable: 'sessions-app' })[key],
  architecture: async () => 'arm64',
  signature: async () => {},
  signing: async () => 'adhoc',
};

async function fixture(t) {
  const directory = await mkdtemp(join(tmpdir(), 'sD-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const app = join(directory, 'Sessions.app');
  const runtime = join(app, 'Contents/Resources/runtime');
  await mkdir(runtime, { recursive: true });
  await mkdir(join(app, 'Contents/MacOS'));
  await writeFile(join(app, 'Contents/Info.plist'), 'fixture plist');
  await writeFile(join(app, 'Contents/MacOS/sessions-app'), 'fixture app');
  const binaries = {};
  for (const name of runtimeNames) {
    const bytes = `original ${name}`;
    await writeFile(join(runtime, name), bytes);
    binaries[name] = hash(bytes);
  }
  await writeFile(join(runtime, 'runtime-manifest.json'), JSON.stringify(developmentRuntime(expected.appVersion, sha, binaries)));
  const archive = join(directory, `Sessions-development-${sha}-darwin-arm64.zip`);
  await writeFile(archive, 'fixture archive bytes');
  return { directory, app, runtime, archive, receipt: join(directory, 'development-app.json') };
}

test('development receipt rejects independent source, workflow run, attempt, and bytes mismatches', async (t) => {
  const f = await fixture(t);
  await operate('record', f.app, f.archive, f.receipt, expected, inspector);
  await operate('verify', f.app, f.archive, f.receipt, expected, inspector);
  for (const wrong of [identity('b'.repeat(40), '12345', '2', '0.2.27'), identity(sha, '12346', '2', '0.2.27'), identity(sha, '12345', '3', '0.2.27')]) {
    await assert.rejects(operate('verify', f.app, f.archive, f.receipt, wrong, inspector), /provenance/);
  }
  await writeFile(f.archive, 'tampered');
  await assert.rejects(operate('verify', f.app, f.archive, f.receipt, expected, inspector), /Archive checksum/);
  await writeFile(join(f.app, 'Contents/MacOS/sessions-app'), 'tampered');
  await assert.rejects(operate('verify', f.app, f.archive, f.receipt, expected, inspector), /Bundle binary checksum/);
});

test('local signature refresh preserves development source prefix and immutable CI receipt', async (t) => {
  const f = await fixture(t);
  await operate('record', f.app, f.archive, f.receipt, expected, inspector);
  const originalReceipt = await readFile(f.receipt, 'utf8');
  const original = JSON.parse(originalReceipt).runtime;
  await writeFile(join(f.runtime, 'sessionsd'), 'locally signed fixture bytes');
  await assert.rejects(operate('verify-local', f.app, undefined, f.receipt, expected, inspector), /Runtime manifest/);
  const refreshed = await operate('refresh-runtime', f.app, undefined, f.receipt, expected, inspector);
  assert.match(refreshed.runtime.runtimeVersion, /^v0\.2\.27-dev\.gaaaaaaaaaaaa-bin\.[a-f0-9]{12}$/);
  assert.notEqual(refreshed.runtime.runtimeVersion, original.runtimeVersion);
  assert.equal(await readFile(f.receipt, 'utf8'), originalReceipt);
  await operate('verify-local', f.app, undefined, f.receipt, expected, inspector);
  await assert.rejects(operate('verify', f.app, f.archive, f.receipt, expected, inspector), /Bundle binary checksum/);
  await assert.rejects(operate('refresh-runtime', f.app, undefined, f.receipt, expected, inspector), /Runtime manifest/);
});

test('development verification refuses Intel, wrong bundle version, stable labels, and extra runtimes', async (t) => {
  const f = await fixture(t);
  await assert.rejects(operate('record', f.app, f.archive, f.receipt, expected, { ...inspector, architecture: async () => 'x86_64' }), /Darwin ARM64/);
  await assert.rejects(operate('record', f.app, f.archive, f.receipt, expected, { ...inspector, plist: async (_, key) => key === 'CFBundleVersion' ? '0.2.26' : inspector.plist(_, key) }), /version mismatch/);
  const manifest = JSON.parse(await readFile(join(f.runtime, 'runtime-manifest.json'), 'utf8'));
  assert.throws(() => checkRuntime({ ...manifest, runtimeVersion: manifest.runtimeVersion.replace('-dev.gaaaaaaaaaaaa', '') }, manifest), /development source/);
  assert.throws(() => developmentRuntime('0.2.27', sha, { ...manifest.binaries, relay: 'b'.repeat(64) }), /exactly three/);
});

test('native macOS tools verify a tiny fixture before and after local signature refresh', { skip: process.platform !== 'darwin' }, async (t) => {
  const f = await fixture(t);
  const home = join(f.directory, 'home');
  await mkdir(home);
  const env = { ...process.env, HOME: home, SESSIONS_STATE_DIR: join(f.directory, 'runners'), SESSIONS_LEDGER_PATH: join(f.directory, 'lanes.sqlite3'), SESSIONS_PORT: '18898' };
  const exec = promisify(execFile);
  const run = (command, args) => exec(command, args, { env });
  const plist = '<?xml version="1.0"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>tech.somewhere.sessions</string><key>CFBundleExecutable</key><string>sessions-app</string><key>CFBundleShortVersionString</key><string>0.2.27</string><key>CFBundleVersion</key><string>0.2.27</string><key>CFBundlePackageType</key><string>APPL</string></dict></plist>';
  await writeFile(join(f.app, 'Contents/Info.plist'), plist);
  const source = join(f.directory, 'fixture.c');
  await writeFile(source, 'int main(void) { return 0; }\n');
  const appExecutable = join(f.app, 'Contents/MacOS/sessions-app');
  await run('/usr/bin/clang', ['-arch', 'arm64', source, '-o', appExecutable]);
  const binaries = {};
  for (const name of runtimeNames) {
    const file = join(f.runtime, name);
    await copyFile(appExecutable, file);
    await run('/usr/bin/codesign', ['--force', '--timestamp=none', '--sign', '-', '--identifier', `tech.somewhere.sessions.fixture.${name}`, file]);
    binaries[name] = hash(await readFile(file));
  }
  await writeFile(join(f.runtime, 'runtime-manifest.json'), JSON.stringify(developmentRuntime(expected.appVersion, sha, binaries)));
  const signOuter = () => run('/usr/bin/codesign', ['--force', '--timestamp=none', '--sign', '-', f.app]);
  await signOuter();
  await rm(f.archive);
  await run('/usr/bin/ditto', ['-c', '-k', '--norsrc', '--noextattr', '--noqtn', '--noacl', '--keepParent', f.app, f.archive]);
  const helper = fileURLToPath(new URL('./development-app-manifest.mjs', import.meta.url));
  const invoke = (operation, archive = false) => run(process.execPath, [helper, operation, f.app, ...(archive ? [f.archive] : []), f.receipt, sha, expected.runId, expected.runAttempt, expected.appVersion]);
  await invoke('record', true);
  await invoke('verify', true);
  await run('/usr/bin/codesign', ['--force', '--timestamp=none', '--sign', '-', '--identifier', 'tech.somewhere.sessions.fixture.resigned', join(f.runtime, 'sessionsd')]);
  await invoke('refresh-runtime');
  await signOuter();
  const { stdout } = await invoke('verify-local');
  assert.match(JSON.parse(stdout).runtime.runtimeVersion, /^v0\.2\.27-dev\.gaaaaaaaaaaaa-bin\.[a-f0-9]{12}$/);
});
