import assert from 'node:assert/strict';
import { mkdtemp, writeFile, mkdir, rm, symlink, readFile, copyFile } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import { expectedFiles, record, verify } from './release-artifacts.mjs';
import { verifyToolArchive } from './stage-updater-signer.mjs';
const run = promisify(execFile);
const root = fileURLToPath(new URL('../', import.meta.url));
const commit = 'a'.repeat(40);
const releaseTest = (name, body) => test(name, { skip: process.platform === 'win32' ? 'macOS release helpers are gated on the macOS build host' : false }, body);

releaseTest('development app build is opt-in, verified, immutable, read-only, and explicitly ARM64', async () => {
  const { stdout } = await run('ruby', ['-rjson', '-ryaml', '-e', 'puts YAML.safe_load(File.read(ARGV[0])).to_json', '.github/workflows/ci.yml'], { cwd: root });
  const workflow = JSON.parse(stdout);
  // Ruby YAML 1.1 represents the unquoted GitHub Actions `on` key as true.
  const trigger = workflow.on || workflow.true;
  assert.equal(trigger.workflow_dispatch.inputs.build_dev_app.type, 'boolean');
  assert.equal(trigger.workflow_dispatch.inputs.build_dev_app.default, false);
  const job = workflow.jobs.dev_app;
  assert.deepEqual(job.needs, ['verify', 'linux_amd64']);
  assert.equal(job.if, "github.event_name == 'workflow_dispatch' && inputs.build_dev_app == true && needs.verify.result == 'success' && needs.linux_amd64.result == 'success'");
  const linux = workflow.jobs.linux_amd64;
  assert.equal(linux['runs-on'], 'ubuntu-24.04');
  assert.equal(linux.environment, undefined);
  assert.doesNotMatch(JSON.stringify(linux), /secrets\.|npm publish|gh release/);
  assert.ok(linux.steps.some((step) => step.run?.includes('linux-runtime-acceptance.mjs')));
  const linuxCheckout = linux.steps.find((step) => step.uses?.startsWith('actions/checkout@'));
  assert.equal(linuxCheckout.with['persist-credentials'], false);
  for (const step of linux.steps) if (step.uses) assert.match(step.uses, /@[a-f0-9]{40}$/);
  assert.equal(job['runs-on'], 'macos-14');
  assert.equal(job.permissions.contents, 'read');
  assert.equal(job.environment, undefined);
  assert.equal(job.env.SESSIONS_RUNTIME_SIGN_MODE, 'adhoc');
  assert.equal(job.env.SESSIONS_RUNTIME_DEVELOPMENT, '1');
  assert.doesNotMatch(JSON.stringify(job), /secrets\.|APPLE_|SIGNING_PRIVATE|GITHUB_TOKEN|npm publish|gh release|notarytool|import.*keychain/);
  const checkout = job.steps.find((step) => step.uses?.startsWith('actions/checkout@'));
  assert.equal(checkout.with.ref, '${{ github.sha }}');
  assert.equal(checkout.with['persist-credentials'], false);
  const runs = job.steps.map((step) => step.run || '').join('\n');
  assert.match(runs, /npm ci/);
  assert.match(runs, /uname -s.*uname -m.*Darwin\/arm64/);
  assert.match(runs, /tauri build -- --target aarch64-apple-darwin/);
  assert.match(runs, /createUpdaterArtifacts":false/);
  assert.match(runs, /target\/aarch64-apple-darwin\/release\/bundle\/macos/);
  assert.match(runs, /development-app-manifest\.mjs record/);
  assert.match(runs, /development-app-manifest\.mjs verify/);
  assert.match(runs, /shasum -a 256/);
  assert.ok(job.steps.some((step) => step.uses?.startsWith('actions/upload-artifact@')));
  for (const step of job.steps) if (step.uses) assert.match(step.uses, /@[a-f0-9]{40}$/);
  const builder = await readFile(join(root, 'scripts/build-app-runtime.sh'), 'utf8');
  assert.match(builder, /SESSIONS_RUNTIME_DEVELOPMENT:-0/);
  assert.match(builder, /development_mode" == "0" && "\$exact_tag" == "v\$app_version"/);
});

releaseTest('release jobs separate dependency execution, signing keys, and publication authority', async () => {
  const { stdout } = await run('ruby', ['-rjson', '-ryaml', '-e', 'puts YAML.safe_load(File.read(ARGV[0])).to_json', '.github/workflows/release.yml'], { cwd: root });
  const workflow = JSON.parse(stdout);
  assert.equal(workflow.permissions.contents, 'read');
  assert.doesNotMatch(JSON.stringify(workflow.env), /secrets\.|APPLE_|SIGNING_|GITHUB_TOKEN/);
  const { build, sign, package: packaging, publish } = workflow.jobs;
  assert.equal(build.env.PUPPETEER_CACHE_DIR, '${{ runner.temp }}/puppeteer',
    'dependency installation and isolated-home smoke tests must use the same browser cache');
  for (const job of [build, sign, packaging]) assert.equal(job.permissions.contents, 'read');
  for (const job of [build, packaging]) {
    assert.equal(job.environment, undefined);
    assert.doesNotMatch(JSON.stringify(job), /secrets\./);
  }
  assert.equal(sign.needs, 'build');
  assert.deepEqual(packaging.needs, ['build', 'sign']);
  assert.deepEqual(publish.needs, ['build', 'package', 'linux_acceptance']);
  assert.equal(publish.permissions.contents, 'write');
  const runs = (job) => job.steps.map((step) => step.run || '').join('\n');
  assert.match(runs(build), /npm ci/);
  assert.match(runs(build), /go test/);
  assert.match(runs(build), /uname -s.*uname -m.*Darwin\/arm64/);
  assert.match(runs(build), /tauri build -- --target aarch64-apple-darwin/);
  assert.match(runs(build), /target\/aarch64-apple-darwin\/release\/bundle\/macos/);
  assert.match(runs(build), /lipo -archs.*== arm64/);
  assert.match(runs(packaging), /test-packed-release\.cjs/);
  assert.doesNotMatch(runs(sign), /\bnpm\s+(ci|install|exec|run|pack)|\bcargo\b|\bgo\s+(build|test)|tauri\s+build|release-app\.sh/);
  assert.doesNotMatch(runs(publish), /\bnpm\s+(ci|install|exec|run|pack)|\bcargo\b|\bgo\s+(build|test)/);
  assert.match(runs(publish), /gh release create[^\n]*--draft[^\n]*--verify-tag/);
  assert.match(runs(publish), /gh release upload/);
  assert.doesNotMatch(JSON.stringify(workflow), /gh release edit|--draft(?:=|\s+)false|--latest|--clobber|\bnpm\s+publish\b/,
    'no job may bypass exact-byte acceptance by automatically promoting a release');
  assert.doesNotMatch(runs(publish), /node npm\/scripts\/verify-release\.cjs/,
    'public asset verification follows explicit publication, not draft staging');
  assert.match(runs(publish), /Install and test those exact signed bytes/);
  assert.match(runs(publish), /Only after acceptance, explicitly authorize publication/);
  const credentialSteps = sign.steps.filter((step) => /secrets\./.test(JSON.stringify(step.env)));
  assert.equal(credentialSteps.length, 1, 'all signing credentials are scoped to one artifact signing step');
  assert.doesNotMatch(JSON.stringify(credentialSteps[0]), /GITHUB_TOKEN/);
  const stageIndex = sign.steps.findIndex((step) => step.run?.includes('stage-updater-signer.mjs'));
  assert.ok(stageIndex >= 0 && stageIndex < sign.steps.indexOf(credentialSteps[0]));
  assert.match(sign.steps[stageIndex].run, /release-artifacts\.mjs verify/);
  assert.match(sign.steps[stageIndex].run, /runtime-signing-manifest\.mjs verify/);
  assert.match(credentialSteps[0].run, /trap cleanup EXIT/);
  assert.ok(sign.steps.some((step) => step.if === 'always()' && step.run.includes('delete-keychain')));
  const publishKeys = publish.steps.flatMap((step) => Object.keys(step.env || {}).filter((key) => /secrets\./.test(step.env[key])));
  assert.deepEqual(publishKeys, ['GITHUB_TOKEN']);
  for (const job of [sign, packaging, publish]) {
    const download = job.steps.find((step) => step.uses?.startsWith('actions/download-artifact@'));
    assert.match(download.with['artifact-ids'], /needs\.[a-z]+\.outputs\.artifact/);
    const checkout = job.steps.find((step) => step.uses?.startsWith('actions/checkout@'));
    assert.equal(checkout.with.ref, '${{ needs.build.outputs.commit }}');
    assert.equal(checkout.with['persist-credentials'], false);
  }
  for (const job of Object.values(workflow.jobs)) {
    for (const step of job.steps) if (step.uses) assert.match(step.uses, /@[a-f0-9]{40}$/);
  }
  const signer = await readFile(join(root, 'scripts/sign-release-assets.sh'), 'utf8');
  assert.doesNotMatch(signer, /\bnpm\s|\bcargo\s|\bgo\s+(build|test)|tauri\s+build/);
  assert.match(signer, /verify-updater-signature\.mjs/);
  assert.match(signer, /stapler validate/);
  assert.match(signer, /tar -xzf "\$OUTPUT\/Sessions\.app\.tar\.gz"/);
  assert.match(signer, /stapler validate "\$OUTPUT\/verified-package\/Sessions\.app"/);
});

releaseTest('final ZIP is clean and round-trip verified before delivery checksums', async () => {
  const signer = await readFile(join(root, 'scripts/sign-release-assets.sh'), 'utf8');
  const pack = signer.indexOf('ditto -c -k --norsrc --noextattr --noqtn --noacl --keepParent "$APP" "$OUTPUT/Sessions_${VERSION}_darwin_arm64.zip"');
  assert.ok(pack >= 0);
  const unpack = signer.indexOf('python3 "$ROOT/scripts/unpack-release-app.py" "$OUTPUT/Sessions_${VERSION}_darwin_arm64.zip" "$OUTPUT/verified-zip"');
  assert.ok(unpack > pack);
  let previous = unpack;
  for (const command of [
    'codesign --verify --deep --strict --verbose=2 "$OUTPUT/verified-zip/Sessions.app"',
    'xcrun stapler validate "$OUTPUT/verified-zip/Sessions.app"',
    'spctl --assess --type execute --verbose=4 "$OUTPUT/verified-zip/Sessions.app"',
    'node "$ROOT/scripts/runtime-signing-manifest.mjs" verify "$OUTPUT/verified-zip/Sessions.app/Contents/Resources/runtime" "$VERSION"',
    'python3 -c \'import shutil,sys; shutil.rmtree(sys.argv[1])\' "$OUTPUT/verified-zip"',
  ]) {
    const index = signer.indexOf(command);
    assert.ok(index > previous, `Missing or misordered ZIP check: ${command}`);
    previous = index;
  }
  assert.ok(signer.indexOf('for artifact in ') > previous);
});

test('clean ZIP extraction preserves native signature and regular ticket-location bytes', { skip: process.platform !== 'darwin' }, async (t) => {
  const directory = await mkdtemp(join(tmpdir(), 'sZ-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const app = join(directory, 'Sessions.app');
  await mkdir(join(app, 'Contents/MacOS'), { recursive: true });
  await copyFile('/usr/bin/true', join(app, 'Contents/MacOS/fixture'));
  await writeFile(join(app, 'Contents/Info.plist'), '<?xml version="1.0"?><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>tech.somewhere.sessions.zip.fixture</string><key>CFBundleExecutable</key><string>fixture</string><key>CFBundlePackageType</key><string>APPL</string></dict></plist>');
  // A regular marker exercises the ticket location's file preservation only;
  // it is not a real notarization ticket and claims no notarization acceptance.
  const marker = Buffer.from('synthetic ticket-location fixture');
  await writeFile(join(app, 'Contents/CodeResources'), marker);
  await run('/usr/bin/codesign', ['--force', '--timestamp=none', '--sign', '-', app]);
  await run('/usr/bin/codesign', ['--verify', '--deep', '--strict', app]);
  const executable = join(app, 'Contents/MacOS/fixture');
  await run('/usr/bin/xattr', ['-wx', 'com.apple.FinderInfo', `54455354${'00'.repeat(28)}`, executable]);
  assert.match((await run('/usr/bin/xattr', [executable])).stdout, /com\.apple\.FinderInfo/);
  const archive = join(directory, 'final.zip');
  await run('/usr/bin/ditto', ['-c', '-k', '--norsrc', '--noextattr', '--noqtn', '--noacl', '--keepParent', app, archive]);
  const stage = join(directory, 'extracted');
  await run('python3', ['scripts/unpack-release-app.py', archive, stage], { cwd: root });
  const extracted = join(stage, 'Sessions.app');
  await run('/usr/bin/codesign', ['--verify', '--deep', '--strict', extracted]);
  assert.deepEqual(await readFile(join(extracted, 'Contents/CodeResources')), marker);
  assert.deepEqual(await readFile(join(extracted, 'Contents/MacOS/fixture')), await readFile(join(app, 'Contents/MacOS/fixture')));
  assert.doesNotMatch((await run('/usr/bin/xattr', [join(extracted, 'Contents/MacOS/fixture')])).stdout, /com\.apple\.FinderInfo/);
});

releaseTest('draft staging requires native acceptance of the exact packaged Linux delivery', async () => {
  const { stdout } = await run('ruby', ['-rjson', '-ryaml', '-e', 'puts YAML.safe_load(File.read(ARGV[0])).to_json', '.github/workflows/release.yml'], { cwd: root });
  const workflow = JSON.parse(stdout);
  const job = workflow.jobs.linux_acceptance;
  assert.deepEqual(job.needs, ['build', 'package']);
  assert.equal(job['runs-on'], 'ubuntu-24.04');
  assert.equal(job['timeout-minutes'], 15);
  assert.equal(job.permissions.contents, 'read');
  assert.equal(job.environment, undefined);
  const source = '${{ needs.build.outputs.commit }}';
  const checkout = job.steps.find((step) => step.uses?.startsWith('actions/checkout@'));
  assert.equal(checkout.with.ref, source);
  assert.equal(checkout.with['persist-credentials'], false);
  const downloads = job.steps.filter((step) => step.uses?.startsWith('actions/download-artifact@'));
  assert.equal(downloads.length, 1);
  assert.equal(downloads[0].with['artifact-ids'], '${{ needs.package.outputs.artifact }}');
  const index = (text) => job.steps.findIndex((step) => step.run?.includes(text));
  const verify = index('release-artifacts.mjs verify');
  const unpack = index('unpack-release-cli.cjs');
  const runtime = index('linux-runtime-acceptance.mjs');
  const npm = index('test-packed-release.cjs');
  assert.ok(verify >= 0 && unpack > verify && runtime > unpack && npm > runtime);
  assert.equal(job.steps[verify].env.MANIFEST, '${{ needs.package.outputs.manifest }}');
  assert.equal(job.steps[verify].env.SOURCE_SHA, source);
  assert.match(job.steps[verify].run, /Linux\/x86_64/);
  assert.match(job.steps[verify].run, /git rev-parse HEAD.*SOURCE_SHA/);
  assert.match(job.steps[verify].run, /delivery "\$MANIFEST"/);
  assert.match(job.steps[unpack].run, /delivery\/cli\/sessions_\$\{VERSION\}_linux_amd64\.tar\.gz/);
  assert.equal(job.steps[runtime].env.SOURCE_SHA, source);
  assert.match(job.steps[npm].run, /delivery\/somewhere-tech-sessions-\$\{VERSION\}\.tgz.*delivery\/npm-manifest\.json.*delivery\/cli/);
  assert.doesNotMatch(JSON.stringify(job), /secrets\.|SESSIONS_ACCEPTANCE_FIXTURE|setup-go|\bgo (build|test)|npm (ci|publish)|gh release|verify-release\.cjs|https:\/\/github\.com/);
  const upload = job.steps.find((step) => step.uses?.startsWith('actions/upload-artifact@'));
  assert.equal(upload.if, 'always()');
  for (const name of ['linux-release-acceptance.json', 'linux-release-acceptance.json.daemon.log', 'linux-npm-acceptance.log']) assert.ok(upload.with.path.includes(name));
  assert.equal(upload.with['retention-days'], 7);
  assert.deepEqual(workflow.jobs.publish.needs, ['build', 'package', 'linux_acceptance']);
  assert.equal(workflow.jobs.publish.if, undefined, 'normal success dependency must not be bypassed');
});

releaseTest('artifact handoff refuses tampering, foreign source, extra files, and symlinks', async (t) => {
  const directory = await mkdtemp(join(tmpdir(), 'sA-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  await mkdir(join(directory, 'cli'));
  for (const file of expectedFiles('0.2.27', 'unsigned')) await writeFile(join(directory, file), `fixture ${file}`);
  const digest = await record(directory, '0.2.27', commit, 'unsigned');
  await verify(directory, '0.2.27', commit, 'unsigned', digest);
  await assert.rejects(verify(directory, '0.2.27', 'b'.repeat(40), 'unsigned', digest), /provenance/);
  await assert.rejects(verify(directory, '0.2.27', commit, 'unsigned', '0'.repeat(64)), /producing job/);
  await writeFile(join(directory, 'Sessions-unsigned.zip'), 'changed');
  await assert.rejects(verify(directory, '0.2.27', commit, 'unsigned', digest), /checksum mismatch/);
  await writeFile(join(directory, 'extra'), 'extra');
  await assert.rejects(record(directory, '0.2.27', commit, 'unsigned'), /expected stage/);
  await rm(join(directory, 'extra'));
  await symlink('/etc/passwd', join(directory, 'link'));
  await assert.rejects(record(directory, '0.2.27', commit, 'unsigned'), /symlink/);
});

releaseTest('app extraction refuses traversal and symlinks before signing authority exists', async (t) => {
  const directory = await mkdtemp(join(tmpdir(), 'sA-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const create = String.raw`import sys,zipfile,stat
with zipfile.ZipFile(sys.argv[1], 'w') as z:
    i=zipfile.ZipInfo(sys.argv[2]); i.external_attr=(stat.S_IFLNK|0o777)<<16 if sys.argv[3]=='link' else (stat.S_IFREG|0o755)<<16; z.writestr(i,b'fixture')`;
  for (const [name, kind, expected] of [['../escape', 'file', /unexpected path/], ['Sessions.app/link', 'link', /symlink/]]) {
    const archive = join(directory, `${kind}.zip`);
    await run('python3', ['-c', create, archive, name, kind]);
    await assert.rejects(run('python3', ['scripts/unpack-release-app.py', archive, join(directory, kind)], { cwd: root }), expected);
  }
  const archive = join(directory, 'valid.zip');
  await run('python3', ['-c', create, archive, 'Sessions.app/Contents/MacOS/fixture', 'file']);
  await run('python3', ['scripts/unpack-release-app.py', archive, join(directory, 'valid')], { cwd: root });
  assert.equal((await readFile(join(directory, 'valid/Sessions.app/Contents/MacOS/fixture'), 'utf8')), 'fixture');
});

releaseTest('updater signer tool rejects bytes outside the reviewed integrity pin', async (t) => {
  const directory = await mkdtemp(join(tmpdir(), 'sA-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  await mkdir(join(directory, 'package'));
  await writeFile(join(directory, 'package/cli.darwin-arm64.node'), 'native fixture');
  const archivePath = join(directory, 'tool.tgz');
  await run('tar', ['--format=ustar', '-czf', archivePath, '-C', directory, 'package/cli.darwin-arm64.node']);
  const bytes = await readFile(archivePath);
  const integrity = `sha512-${createHash('sha512').update(bytes).digest('base64')}`;
  assert.equal(verifyToolArchive(bytes, { integrity }).toString(), 'native fixture');
  assert.throws(() => verifyToolArchive(Buffer.concat([bytes, Buffer.from('changed')]), { integrity }), /reviewed lockfile/);
  assert.throws(() => verifyToolArchive(bytes, { integrity: 'sha256-unsafe' }), /SHA-512/);
});

releaseTest('runtime signing manifest follows the signed bytes and rejects stale hashes', async (t) => {
  const directory = await mkdtemp(join(tmpdir(), 'sA-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  for (const name of ['sessions', 'sessionsd', 'sessions-runner']) await writeFile(join(directory, name), `${name} unsigned fixture`);
  const tool = 'scripts/runtime-signing-manifest.mjs';
  await run(process.execPath, [tool, 'render', directory, '0.2.27'], { cwd: root });
  await run(process.execPath, [tool, 'verify', directory, '0.2.27'], { cwd: root });
  await writeFile(join(directory, 'sessionsd'), 'signed fixture changed bytes');
  await assert.rejects(run(process.execPath, [tool, 'verify', directory, '0.2.27'], { cwd: root }), /expected release and bytes/);
  await run(process.execPath, [tool, 'render', directory, '0.2.27'], { cwd: root });
  await run(process.execPath, [tool, 'verify', directory, '0.2.27'], { cwd: root });
});
