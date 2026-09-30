#!/usr/bin/env node
'use strict';
const fs = require('node:fs/promises');
const path = require('node:path');
const { extract } = require('../npm/lib/archive.cjs');
const [archive, destination] = process.argv.slice(2);
(async () => {
  if (!archive || !destination || process.argv.length !== 4) throw new Error('Usage: unpack-release-cli.cjs ARCHIVE NEW_DESTINATION');
  const files = extract(await fs.readFile(archive));
  const expected = ['sessions', 'sessionsd', 'sessions-runner', 'sessions-relay', 'LICENSE', 'README.md'];
  if (JSON.stringify([...files.keys()].sort()) !== JSON.stringify(expected.sort())) throw new Error('standalone release archive has an unexpected file set');
  await fs.mkdir(destination, { recursive: false, mode: 0o700 });
  for (const [name, bytes] of files) {
    await fs.writeFile(path.join(destination, name), bytes, { mode: name.startsWith('sessions') ? 0o755 : 0o644, flag: 'wx' });
  }
})().catch((error) => { console.error(error.message); process.exitCode = 1; });
