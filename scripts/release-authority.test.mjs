import assert from 'node:assert/strict';
import { mkdtemp, writeFile, mkdir, rm, symlink, readFile } from 'node:fs/promises';
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

releaseTest('release jobs separate dependency execution, signing keys, and publication authority', async () => {
  const { stdout } = await run('ruby', ['-rjson', '-ryaml', '-e', 'puts YAML.safe_load(File.read(ARGV[0])).to_json', '.github/workflows/release.yml'], { cwd: root });
  const workflow = JSON.parse(stdout);
  assert.equal(workflow.permissions.contents, 'read');
  assert.doesNotMatch(JSON.stringify(workflow.env), /secrets\.|APPLE_|SIGNING_|GITHUB_TOKEN/);
  const { build, sign, package: packaging, publish } = workflow.jobs;
  for (const job of [build, sign, packaging]) assert.equal(job.permissions.contents, 'read');
  for (const job of [build, packaging]) {
    assert.equal(job.environment, undefined);
    assert.doesNotMatch(JSON.stringify(job), /secrets\./);
  }
  assert.equal(sign.needs, 'build');
  assert.deepEqual(packaging.needs, ['build', 'sign']);
  assert.deepEqual(publish.needs, ['build', 'package']);
  assert.equal(publish.permissions.contents, 'write');
  const runs = (job) => job.steps.map((step) => step.run || '').join('\n');
  assert.match(runs(build), /npm ci/);
  assert.match(runs(build), /go test/);
  assert.match(runs(packaging), /test-packed-release\.cjs/);
  assert.doesNotMatch(runs(sign), /\bnpm\s+(ci|install|exec|run|pack)|\bcargo\b|\bgo\s+(build|test)|tauri\s+build|release-app\.sh/);
  assert.doesNotMatch(runs(publish), /\bnpm\s+(ci|install|exec|run|pack)|\bcargo\b|\bgo\s+(build|test)/);
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
