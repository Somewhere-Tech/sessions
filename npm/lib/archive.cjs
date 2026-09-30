'use strict';
const { gunzipSync } = require('node:zlib');
const { createHash } = require('node:crypto');
const commands = ['sessions', 'sessionsd', 'sessions-runner'];
const allowed = new Set([...commands, 'sessions-relay', 'LICENSE', 'README.md']);
const sha256 = (bytes) => createHash('sha256').update(bytes).digest('hex');

function extract(archive) {
  const bytes = gunzipSync(archive, { maxOutputLength: 256 * 1024 * 1024 });
  const files = new Map();
  let offset = 0;
  while (offset + 512 <= bytes.length) {
    const header = bytes.subarray(offset, offset + 512);
    if (header.every((byte) => byte === 0)) break;
    const text = (start, size) => header.subarray(start, start + size).toString('utf8').replace(/\0.*$/s, '');
    const prefix = text(345, 155);
    const rawName = `${prefix ? `${prefix}/` : ''}${text(0, 100)}`;
    const name = rawName.replace(/^\.\//, '');
    const type = text(156, 1);
    const sizeText = text(124, 12).trim();
    if (!/^[0-7]+$/.test(sizeText)) throw new Error('release archive has an invalid file size');
    const size = parseInt(sizeText, 8);
    let sum = 0;
    for (let i = 0; i < 512; i++) sum += i >= 148 && i < 156 ? 32 : header[i];
    if (parseInt(text(148, 8).trim(), 8) !== sum) throw new Error('release archive has an invalid header checksum');
    offset += 512;
    if (offset + size > bytes.length) throw new Error('release archive is truncated');
    if (type === '5' && (name === '' || name === '.' || name === './') && size === 0) continue;
    if ((type !== '' && type !== '0') || !allowed.has(name) || files.has(name)) {
      throw new Error(`release archive contains an unexpected member: ${rawName}`);
    }
    files.set(name, bytes.subarray(offset, offset + size));
    offset += Math.ceil(size / 512) * 512;
  }
  for (const command of commands) {
    if (!files.get(command)?.length) throw new Error(`release archive is missing ${command}`);
  }
  return files;
}

module.exports = { commands, extract, sha256 };
