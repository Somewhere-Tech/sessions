import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import test from 'node:test';

const script = path.resolve(import.meta.dirname, 'verify-updater-signature.mjs');

function fixture() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'sessions-updater-signature-'));
  const artifact = path.join(root, 'Sessions_0.2.6_x64-setup.exe');
  const publicKeyPath = path.join(root, 'updater.pub');
  const signaturePath = `${artifact}.sig`;
  const keyId = Buffer.from('72f1f99a7884c0f2', 'hex');
  const { publicKey, privateKey } = crypto.generateKeyPairSync('ed25519');
  const publicDer = publicKey.export({ format: 'der', type: 'spki' });
  const publicPayload = Buffer.concat([
    Buffer.from('Ed'),
    keyId,
    publicDer.subarray(-32)
  ]);
  const publicEnvelope = [
    'untrusted comment: minisign public key',
    publicPayload.toString('base64'),
    ''
  ].join('\n');
  fs.writeFileSync(publicKeyPath, Buffer.from(publicEnvelope).toString('base64'));

  const artifactContents = Buffer.from('signed Windows installer');
  fs.writeFileSync(artifact, artifactContents);
  const digest = crypto.createHash('blake2b512').update(artifactContents).digest();
  const artifactSignature = crypto.sign(null, digest, privateKey);
  const signaturePayload = Buffer.concat([
    Buffer.from('ED'),
    keyId,
    artifactSignature
  ]);
  const trustedComment = 'timestamp:1785060457\tfile:Sessions_0.2.6_x64-setup.exe';
  const globalSignature = crypto.sign(
    null,
    Buffer.concat([artifactSignature, Buffer.from(trustedComment)]),
    privateKey
  );
  const signatureEnvelope = [
    'untrusted comment: signature from minisign secret key',
    signaturePayload.toString('base64'),
    `trusted comment: ${trustedComment}`,
    globalSignature.toString('base64'),
    ''
  ].join('\n');
  fs.writeFileSync(signaturePath, Buffer.from(signatureEnvelope).toString('base64'));
  return { artifact, publicKeyPath, signaturePath };
}

function verify({ artifact, publicKeyPath, signaturePath }) {
  return spawnSync(process.execPath, [
    script,
    '--public-key', publicKeyPath,
    '--artifact', artifact,
    '--signature', signaturePath
  ], { encoding: 'utf8' });
}

test('verifies a prehashed Minisign artifact and trusted comment', () => {
  const files = fixture();
  const result = verify(files);
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /^verified /);
});

test('rejects artifact tampering after the signature is created', () => {
  const files = fixture();
  fs.appendFileSync(files.artifact, 'tampered');
  const result = verify(files);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /artifact failed pinned Minisign verification/);
});

test('rejects an envelope signed by a different key id', () => {
  const files = fixture();
  const text = Buffer.from(fs.readFileSync(files.signaturePath, 'utf8'), 'base64').toString('utf8');
  const lines = text.trimEnd().split('\n');
  const payload = Buffer.from(lines[1], 'base64');
  payload[2] ^= 0xff;
  lines[1] = payload.toString('base64');
  fs.writeFileSync(files.signaturePath, Buffer.from(`${lines.join('\n')}\n`).toString('base64'));
  const result = verify(files);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /different key/);
});
