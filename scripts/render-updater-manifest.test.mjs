import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import test from 'node:test';

const script = path.resolve(import.meta.dirname, 'render-updater-manifest.mjs');

function fixture() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'sessions-updater-manifest-'));
  const artifact = path.join(root, 'Sessions_0.2.6_x64-setup.exe');
  fs.writeFileSync(artifact, 'signed installer');
  fs.writeFileSync(`${artifact}.sig`, 'tauri updater signature\n');
  return { root, artifact };
}

function render(argumentsList) {
  return spawnSync(process.execPath, [script, ...argumentsList], { encoding: 'utf8' });
}

test('merges a Windows target into the same-version macOS manifest', () => {
  const { root, artifact } = fixture();
  const base = path.join(root, 'base.json');
  const output = path.join(root, 'latest.json');
  fs.writeFileSync(base, `${JSON.stringify({
    version: '0.2.6',
    notes: 'Existing notes',
    pub_date: '2026-07-26T12:00:00.000Z',
    platforms: {
      'darwin-aarch64': {
        signature: 'mac signature',
        url: 'https://github.com/somewhere-tech/sessions/releases/download/v0.2.6/Sessions.app.tar.gz'
      }
    }
  })}\n`);

  const result = render([
    '--version', '0.2.6',
    '--artifact', artifact,
    '--url', 'https://github.com/somewhere-tech/sessions/releases/download/v0.2.6/Sessions_0.2.6_x64-setup.exe',
    '--target', 'windows-x86_64',
    '--base', base,
    '--output', output
  ]);
  assert.equal(result.status, 0, result.stderr);
  const manifest = JSON.parse(fs.readFileSync(output, 'utf8'));
  assert.equal(manifest.notes, 'Existing notes');
  assert.equal(manifest.pub_date, '2026-07-26T12:00:00.000Z');
  assert.deepEqual(Object.keys(manifest.platforms), ['darwin-aarch64', 'windows-x86_64']);
  assert.equal(manifest.platforms['windows-x86_64'].signature, 'tauri updater signature');
});

test('refuses a mismatched base version or an existing target', () => {
  const { root, artifact } = fixture();
  const base = path.join(root, 'base.json');
  const common = [
    '--version', '0.2.6',
    '--artifact', artifact,
    '--url', 'https://github.com/somewhere-tech/sessions/releases/download/v0.2.6/Sessions_0.2.6_x64-setup.exe',
    '--target', 'windows-x86_64',
    '--base', base,
    '--output', path.join(root, 'latest.json')
  ];

  fs.writeFileSync(base, JSON.stringify({ version: '0.2.5', platforms: {} }));
  assert.notEqual(render(common).status, 0);

  fs.writeFileSync(base, JSON.stringify({
    version: '0.2.6',
    platforms: {
      'windows-x86_64': { signature: 'old', url: 'https://example.test/old.exe' }
    }
  }));
  assert.notEqual(render(common).status, 0);
});

test('requires the immutable URL basename to match the artifact', () => {
  const { root, artifact } = fixture();
  const result = render([
    '--version', '0.2.6',
    '--artifact', artifact,
    '--url', 'https://github.com/somewhere-tech/sessions/releases/download/v0.2.6/different.exe',
    '--target', 'windows-x86_64',
    '--output', path.join(root, 'latest.json')
  ]);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /basename must match/);
});
