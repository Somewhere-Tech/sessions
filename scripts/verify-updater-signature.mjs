#!/usr/bin/env node

import crypto from 'node:crypto';
import fs from 'node:fs';

function fail(message) {
  throw new Error(`verify-updater-signature: ${message}`);
}

function readArgument(name) {
  const index = process.argv.indexOf(name);
  if (index < 0 || index + 1 >= process.argv.length || process.argv[index + 1].startsWith('--')) {
    fail(`${name} is required`);
  }
  return process.argv[index + 1];
}

function decodeBase64(value, description) {
  const normalized = value.trim();
  if (!normalized || !/^[A-Za-z0-9+/]+={0,2}$/.test(normalized)) {
    fail(`${description} is not valid base64`);
  }
  const decoded = Buffer.from(normalized, 'base64');
  if (decoded.toString('base64') !== normalized) {
    fail(`${description} is not canonical base64`);
  }
  return decoded;
}

function parsePublicKey(encoded) {
  const text = decodeBase64(encoded, 'public key').toString('utf8').replace(/\n$/, '');
  const lines = text.split('\n');
  if (lines.length !== 2 || !lines[0].startsWith('untrusted comment: ')) {
    fail('public key has an invalid Minisign envelope');
  }
  const raw = decodeBase64(lines[1], 'public key payload');
  if (raw.length !== 42 || !['Ed', 'ED'].includes(raw.subarray(0, 2).toString('ascii'))) {
    fail('public key has an unsupported Minisign payload');
  }
  const spkiPrefix = Buffer.from('302a300506032b6570032100', 'hex');
  return {
    keyId: raw.subarray(2, 10),
    key: crypto.createPublicKey({
      key: Buffer.concat([spkiPrefix, raw.subarray(10)]),
      format: 'der',
      type: 'spki'
    })
  };
}

function parseSignature(encoded) {
  const text = decodeBase64(encoded, 'signature').toString('utf8').replace(/\n$/, '');
  const lines = text.split('\n');
  if (
    lines.length !== 4 ||
    !lines[0].startsWith('untrusted comment: ') ||
    !lines[2].startsWith('trusted comment: ')
  ) {
    fail('signature has an invalid Minisign envelope');
  }
  const raw = decodeBase64(lines[1], 'signature payload');
  if (raw.length !== 74 || raw.subarray(0, 2).toString('ascii') !== 'ED') {
    fail('signature must use prehashed Minisign');
  }
  const global = decodeBase64(lines[3], 'global signature');
  if (global.length !== 64) {
    fail('global signature has an invalid length');
  }
  return {
    keyId: raw.subarray(2, 10),
    signature: raw.subarray(10),
    trustedComment: lines[2].slice('trusted comment: '.length),
    global
  };
}

function main() {
  const publicKeyPath = readArgument('--public-key');
  const artifactPath = readArgument('--artifact');
  const signaturePath = readArgument('--signature');
  const publicKey = parsePublicKey(fs.readFileSync(publicKeyPath, 'utf8'));
  const signature = parseSignature(fs.readFileSync(signaturePath, 'utf8'));

  if (!crypto.timingSafeEqual(publicKey.keyId, signature.keyId)) {
    fail('signature was created by a different key');
  }
  const globalPayload = Buffer.concat([
    signature.signature,
    Buffer.from(signature.trustedComment, 'utf8')
  ]);
  if (!crypto.verify(null, globalPayload, publicKey.key, signature.global)) {
    fail('trusted-comment verification failed');
  }
  const digest = crypto.createHash('blake2b512').update(fs.readFileSync(artifactPath)).digest();
  if (!crypto.verify(null, digest, publicKey.key, signature.signature)) {
    fail('artifact failed pinned Minisign verification');
  }
  process.stdout.write(`verified ${artifactPath}\n`);
}

try {
  main();
} catch (error) {
  process.stderr.write(`${error.message}\n`);
  process.exitCode = 1;
}
