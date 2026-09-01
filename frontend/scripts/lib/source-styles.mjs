import { readFileSync } from 'node:fs';
import { readFile } from 'node:fs/promises';

const importLine = /^\s*@import\s+['"]([^'"]+)['"]\s*;\s*$/;

async function expandStylesheet(url, seen) {
  const key = url.href;
  if (seen.has(key)) throw new Error(`Stylesheet import cycle at ${key}`);
  seen.add(key);

  const source = await readFile(url, 'utf8');
  const output = [];
  for (const line of source.split('\n')) {
    const match = line.match(importLine);
    if (!match) {
      output.push(line);
      continue;
    }
    output.push(await expandStylesheet(new URL(match[1], url), seen));
  }
  seen.delete(key);
  return output.join('\n');
}

function expandStylesheetSync(url, seen) {
  const key = url.href;
  if (seen.has(key)) throw new Error(`Stylesheet import cycle at ${key}`);
  seen.add(key);

  const source = readFileSync(url, 'utf8');
  const output = [];
  for (const line of source.split('\n')) {
    const match = line.match(importLine);
    output.push(match
      ? expandStylesheetSync(new URL(match[1], url), seen)
      : line);
  }
  seen.delete(key);
  return output.join('\n');
}

export function readStylesheetTree(url) {
  return expandStylesheet(url, new Set());
}

export function readStylesheetTreeSync(url) {
  return expandStylesheetSync(url, new Set());
}
