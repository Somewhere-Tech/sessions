import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';

import { reflowAnsi } from '../../../testdata/node-runtime/src/reflow.ts';

const repoRoot = path.resolve(__dirname, '../../../..');
const requireFromLegacyRuntime = createRequire(path.join(repoRoot, 'runtime/testdata/node-runtime/package.json'));
const { Terminal } = requireFromLegacyRuntime('@xterm/headless') as typeof import('@xterm/headless');
const { SerializeAddon } = requireFromLegacyRuntime('@xterm/addon-serialize') as typeof import('@xterm/addon-serialize');

async function main(): Promise<void> {
  const input = process.argv[2];
  const width = Number(process.argv[3] ?? 60);
  if (!input) throw new Error('usage: tsx reflow-oracle.ts INPUT [WIDTH]');

  const term = new Terminal({
    cols: 300,
    rows: 50,
    scrollback: 5000,
    allowProposedApi: true
  });
  const serialize = new SerializeAddon();
  term.loadAddon(serialize);
  await new Promise<void>((resolve) => term.write(fs.readFileSync(input), resolve));
  const text = reflowAnsi(serialize.serialize({ scrollback: 0 }), { width });
  term.dispose();
  process.stdout.write(`${JSON.stringify({ text })}\n`);
}

void main();
