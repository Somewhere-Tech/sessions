import { readFileSync } from 'node:fs';
import { readFile } from 'node:fs/promises';

const sessionsdFiles = [
  '../../src/api/sessionsd.ts',
  '../../src/api/sessionsd/core.ts',
  '../../src/api/sessionsd/sessions.ts',
  '../../src/api/sessionsd/search.ts',
  '../../src/api/sessionsd/operations.ts',
  '../../src/api/sessionsd/team.ts'
];

export async function readSessionsdSource() {
  return (await Promise.all(sessionsdFiles.map((path) => readFile(new URL(path, import.meta.url), 'utf8')))).join('\n');
}

export function readSessionsdSourceSync() {
  return sessionsdFiles.map((path) => readFileSync(new URL(path, import.meta.url), 'utf8')).join('\n');
}
