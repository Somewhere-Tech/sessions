'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs/promises');
const path = require('node:path');
const os = require('node:os');
const https = require('node:https');
const { promisify } = require('node:util');
const { execFile, spawn } = require('node:child_process');
const run = promisify(execFile);
const { commands, extract, sha256 } = require('../lib/archive.cjs');
const { release } = require('../lib/install.cjs');
const { prepare } = require('../scripts/prepare-release.cjs');
const { testPackedRelease } = require('../scripts/test-packed-release.cjs');
const packageSource = path.resolve(__dirname, '..');

async function fixture(root, version = '0.2.27') {
  const source = path.join(root, `files-${version}`);
  const archives = path.join(root, `archives-${version}`);
  await fs.mkdir(source, { recursive: true });
  await fs.mkdir(archives, { recursive: true });
  for (const command of commands) {
    const script = `#!/bin/sh\nif [ "$1" = version ]; then echo '${version}'; exit 0; fi\nif [ "$1" = --hold ]; then\n  echo $$\n  trap 'exit 0' TERM INT\n  while :; do sleep 1; done\nfi\nprintf '%s\\n' '${command}-${version}' "$@"\nif [ "$1" = --failure ]; then exit 17; fi\n`;
    await fs.writeFile(path.join(source, command), script, { mode: 0o755 });
  }
  await fs.writeFile(path.join(source, 'LICENSE'), 'Fixture only\n');
  await fs.writeFile(path.join(source, 'README.md'), 'Fixture only\n');
  for (const platform of ['darwin_arm64', 'linux_arm64', 'linux_amd64']) {
    const name = `sessions_${version}_${platform}.tar.gz`;
    const archive = path.join(archives, name);
    await run('tar', ['--format=ustar', '-czf', archive, '-C', source, '.'], { env: { ...process.env, COPYFILE_DISABLE: '1' } });
    await fs.writeFile(`${archive}.sha256`, `${sha256(await fs.readFile(archive))}  ${name}\n`);
  }
  return archives;
}

test('rejects unsupported architectures and unprepared source release', () => {
  assert.throws(() => release('win32', 'x64'), /unsupported platform/);
  assert.throws(() => release('linux', 'ia32'), /unsupported platform/);
  assert.throws(() => release('darwin', 'x64'), /unsupported platform/);
  assert.throws(() => release('linux', 'x64', { version: require('../package.json').version, platforms: {} }), /has not been prepared/);
});

test('release preparation pins all archive and binary hashes and refuses bad checksums', async (t) => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'sN-'));
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const archives = await fixture(root);
  const packageDirectory = path.join(root, 'package');
  await fs.cp(packageSource, packageDirectory, { recursive: true });
  // This historical archive fixture is independent of the source candidate.
  const pkgPath = path.join(packageDirectory, 'package.json');
  const pkg = JSON.parse(await fs.readFile(pkgPath, 'utf8'));
  pkg.version = '0.2.27';
  await fs.writeFile(pkgPath, JSON.stringify(pkg));
  const manifest = await prepare(archives, packageDirectory);
  assert.equal(Object.keys(manifest.platforms).length, 3);
  for (const entry of Object.values(manifest.platforms)) {
    assert.match(entry.url, /\/v0\.2\.27\/sessions_0\.2\.27_/);
    assert.deepEqual(Object.keys(entry.binaries), commands);
  }
  const name = 'sessions_0.2.27_linux_arm64.tar.gz';
  await fs.writeFile(path.join(archives, `${name}.sha256`), `${'0'.repeat(64)}  ${name}\n`);
  await assert.rejects(prepare(archives, packageDirectory), /does not verify/);
});

test('archive extraction refuses symlinks and unexpected members', async (t) => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'sN-'));
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const archives = await fixture(root);
  const valid = await fs.readFile(path.join(archives, 'sessions_0.2.27_linux_arm64.tar.gz'));
  assert.equal(extract(valid).get('sessions').length > 0, true);
  const source = path.join(root, 'files-0.2.27');
  await fs.rm(path.join(source, 'sessions'));
  await fs.symlink('/bin/sh', path.join(source, 'sessions'));
  const archive = path.join(root, 'symlink.tar.gz');
  await run('tar', ['--format=ustar', '-czf', archive, '-C', source, '.'], { env: { ...process.env, COPYFILE_DISABLE: '1' } });
  assert.throws(() => extract(require('node:fs').readFileSync(archive)), /unexpected member/);
  assert.throws(() => extract(valid.subarray(0, valid.length - 10)), /unexpected end|incorrect|invalid|buffer/i);
});

test('actual packed package installs, launches offline, refuses corruption, and preserves runtime across upgrade/removal', async (t) => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'sN-'));
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const home = path.join(root, 'home');
  await fs.mkdir(home);
  const cert = path.join(root, 'cert.pem');
  const key = path.join(root, 'key.pem');
  await run('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', key, '-out', cert,
    '-days', '1', '-subj', '/CN=localhost', '-addext', 'subjectAltName=DNS:localhost']);
  let responseCode = 200;
  let responseBytes;
  let downloadCount = 0;
  const server = https.createServer({ key: await fs.readFile(key), cert: await fs.readFile(cert) }, (request, response) => {
    downloadCount++;
    response.writeHead(responseCode);
    response.end(responseBytes);
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  t.after(() => new Promise((resolve) => server.close(resolve)));
  const env = { ...process.env, HOME: home, SESSIONS_STATE_DIR: path.join(root, 'runners'),
    SESSIONS_LEDGER_PATH: path.join(root, 'lanes.sqlite3'), SESSIONS_PORT: '18879',
    NODE_EXTRA_CA_CERTS: cert, npm_config_cache: path.join(root, 'npm-cache'), npm_config_update_notifier: 'false' };
  const prefix = path.join(root, 'global');
  async function pack(version) {
    const archives = await fixture(root, version);
    const packageDirectory = path.join(root, `package-${version}`);
    await fs.cp(packageSource, packageDirectory, { recursive: true });
    const pkgPath = path.join(packageDirectory, 'package.json');
    const pkg = JSON.parse(await fs.readFile(pkgPath, 'utf8'));
    pkg.version = version;
    await fs.writeFile(pkgPath, JSON.stringify(pkg));
    const manifest = await prepare(archives, packageDirectory);
    const platform = `${process.platform}_${process.arch === 'x64' ? 'amd64' : process.arch}`;
    responseBytes = await fs.readFile(path.join(archives, `sessions_${version}_${platform}.tar.gz`));
    for (const entry of Object.values(manifest.platforms)) entry.url = `https://localhost:${server.address().port}/runtime.tar.gz`;
    await fs.writeFile(path.join(packageDirectory, 'release-manifest.json'), JSON.stringify(manifest));
    const packed = await run('npm', ['pack', packageDirectory, '--json', '--pack-destination', root], { env });
    const metadata = JSON.parse(packed.stdout)[0];
    assert.ok(metadata.files.some((file) => file.path === 'scripts/verify-release.cjs'));
    assert.ok(!metadata.files.some((file) => file.path.startsWith('test/')));
    return path.join(root, metadata.filename);
  }
  const packed = await pack('0.2.27');
  await testPackedRelease(packed, path.join(root, 'package-0.2.27/release-manifest.json'), path.join(root, 'archives-0.2.27'));
  await run('npm', ['install', '--global', '--prefix', prefix, '--offline', packed], { env });
  assert.equal(downloadCount, 1);
  const cli = path.join(prefix, 'bin', 'sessions');
  const output = await run(cli, ['help', 'argument with spaces'], { env });
  assert.match(output.stdout, /sessions-0\.2\.27\nhelp\nargument with spaces/);
  await assert.rejects(run(cli, ['--failure'], { env }), (error) => error.code === 17);
  assert.equal(downloadCount, 1, 'cached command performs no download');
  const library = path.join(prefix, 'lib/node_modules/@somewhere-tech/sessions/lib/install.cjs');
  const cache = (await run(process.execPath, ['-e', `process.stdout.write(require(${JSON.stringify(library)}).directory())`], { env })).stdout;
  const bytes = await fs.readFile(path.join(cache, 'sessions'));
  await fs.writeFile(path.join(cache, 'sessions'), 'corrupt');
  await assert.rejects(run(cli, ['help'], { env }), /cached runtime failed verification/);
  await fs.writeFile(path.join(cache, 'sessions'), bytes, { mode: 0o755 });
  const wrapper = spawn(path.join(prefix, 'bin/sessionsd'), ['--hold'], { env, stdio: ['ignore', 'pipe', 'pipe'] });
  t.after(() => wrapper.kill('SIGTERM'));
  const pid = Number(await new Promise((resolve, reject) => {
    wrapper.stdout.once('data', (chunk) => resolve(chunk.toString().trim()));
    wrapper.once('error', reject);
  }));
  const upgraded = await pack('0.2.28');
  await run('npm', ['install', '--global', '--prefix', prefix, '--offline', upgraded], { env });
  process.kill(pid, 0);
  assert.match((await run(cli, ['help'], { env })).stdout, /sessions-0\.2\.28/);
  await fs.access(path.join(cache, 'sessionsd'));
  await run('npm', ['uninstall', '--global', '--prefix', prefix, '@somewhere-tech/sessions', '--offline'], { env });
  process.kill(pid, 0);
  await fs.access(path.join(cache, 'sessionsd'));
  await new Promise((resolve) => { wrapper.once('exit', resolve); wrapper.kill('SIGTERM'); });
  const lazyPrefix = path.join(root, 'lazy');
  const lazyHome = path.join(root, 'lazy-home');
  await fs.mkdir(lazyHome);
  const beforeLazy = downloadCount;
  await run('npm', ['install', '--global', '--prefix', lazyPrefix, '--ignore-scripts', '--offline', upgraded],
    { env: { ...env, HOME: lazyHome } });
  assert.equal(downloadCount, beforeLazy, 'ignore-scripts installation does not acquire runtime');
  assert.match((await run(path.join(lazyPrefix, 'bin/sessions'), ['help'], { env: { ...env, HOME: lazyHome } })).stdout, /sessions-0\.2\.28/);
  assert.equal(downloadCount, beforeLazy + 1, 'first command stages verified runtime');
  const freshPrefix = path.join(root, 'fresh');
  const freshHome = path.join(root, 'fresh-home');
  await fs.mkdir(freshHome);
  responseCode = 403;
  await assert.rejects(run('npm', ['install', '--global', '--prefix', freshPrefix, '--offline', upgraded],
    { env: { ...env, HOME: freshHome } }), /HTTP 403/);
  responseCode = 200;
  responseBytes = Buffer.from('incorrect archive');
  await assert.rejects(run('npm', ['install', '--global', '--prefix', freshPrefix, '--offline', upgraded],
    { env: { ...env, HOME: freshHome } }), /checksum mismatch/);
  responseBytes = await fs.readFile(path.join(root, `archives-0.2.28/sessions_0.2.28_${process.platform}_${process.arch === 'x64' ? 'amd64' : process.arch}.tar.gz`));
  const tlsHome = path.join(root, 'tls-home');
  await fs.mkdir(tlsHome);
  const tlsEnv = { ...env, HOME: tlsHome };
  delete tlsEnv.NODE_EXTRA_CA_CERTS;
  await assert.rejects(run('npm', ['install', '--global', '--prefix', path.join(root, 'tls'), '--offline', upgraded],
    { env: tlsEnv }), /self.signed|certificate/i);
});
