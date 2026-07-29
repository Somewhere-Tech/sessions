'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { createRequire } = require('node:module');

const repoRoot = path.resolve(__dirname, '../../../..');
const requireFromLegacyRuntime = createRequire(path.join(repoRoot, 'runtime/testdata/node-runtime/package.json'));
const { Terminal } = requireFromLegacyRuntime('@xterm/headless');
const { SerializeAddon } = requireFromLegacyRuntime('@xterm/addon-serialize');

function plainViewport(term) {
  const lines = [];
  const viewportY = term.buffer.active.viewportY;
  for (let y = 0; y < term.rows; y++) {
    const line = term.buffer.active.getLine(viewportY + y);
    lines.push((line?.translateToString(true) ?? '').replace(/ +$/u, ''));
  }
  while (lines.length > 0 && lines.at(-1) === '') lines.pop();
  return lines.join('\n');
}

function canonicalRuns(term) {
  const runs = [];
  let previous = null;
  let count = 0;
  const viewportY = term.buffer.active.viewportY;
  for (let y = 0; y < term.rows; y++) {
    const line = term.buffer.active.getLine(viewportY + y);
    for (let x = 0; x < term.cols; x++) {
      const cell = line?.getCell(x);
      const width = cell?.getWidth() ?? 1;
      let chars = cell?.getChars() ?? '';
      // A null cell and a painted ordinary space are visually equivalent.
      if (width !== 0 && chars === '') chars = ' ';
      const fgMode = cell?.isFgDefault() ? 'default' : cell?.isFgPalette() ? 'palette' : 'rgb';
      const bgMode = cell?.isBgDefault() ? 'default' : cell?.isBgPalette() ? 'palette' : 'rgb';
      const signature = JSON.stringify([
        chars, width,
        fgMode, cell?.getFgColor() ?? -1,
        bgMode, cell?.getBgColor() ?? -1,
        cell?.isBold() ?? 0, cell?.isDim() ?? 0, cell?.isItalic() ?? 0,
        cell?.isUnderline() ?? 0, cell?.isBlink() ?? 0,
        cell?.isInverse() ?? 0, cell?.isInvisible() ?? 0,
        cell?.isStrikethrough() ?? 0, cell?.isOverline() ?? 0
      ]);
      if (signature === previous) {
        count++;
      } else {
        if (previous !== null) runs.push([count, previous]);
        previous = signature;
        count = 1;
      }
    }
  }
  if (previous !== null) runs.push([count, previous]);
  return runs;
}

async function evaluate(raw, cols, rows) {
  const term = new Terminal({
    cols,
    rows,
    scrollback: 5000,
    allowProposedApi: true
  });
  const serialize = new SerializeAddon();
  term.loadAddon(serialize);
  await new Promise((resolve) => term.write(raw, resolve));
  const result = {
    snapshot: plainViewport(term),
    serialized: serialize.serialize({ scrollback: 0 }),
    canonical: canonicalRuns(term)
  };
  term.dispose();
  return result;
}

async function main() {
  const input = process.argv[2];
  const cols = Number(process.argv[3] ?? 300);
  const rows = Number(process.argv[4] ?? 50);
  if (!input) throw new Error('usage: node oracle.cjs INPUT [COLS ROWS]');
  const result = await evaluate(fs.readFileSync(input), cols, rows);
  process.stdout.write(`${JSON.stringify(result)}\n`);
}

main().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
