import { existsSync, readdirSync, statSync } from 'node:fs';
import path from 'node:path';
import os from 'node:os';

// Candidate directories for the New Session dialog's cwd dropdown.
// Free-text input still works; this just makes the common starting
// points one click away.

export interface DirectoryCandidate {
  path: string; // absolute
  label: string; // pretty form for the dropdown — `~/projects/foo`
  kind: 'home' | 'common' | 'project';
}

const HOME = os.homedir();

const COMMON_SUBDIRS = ['Desktop', 'Documents', 'Downloads', 'Code', 'code', 'projects', 'Projects', 'dev', 'src', 'work'];
const PROJECT_MARKERS = ['.git', 'package.json', 'pyproject.toml', 'Cargo.toml', 'go.mod'];

function tildify(p: string): string {
  if (p === HOME) return '~';
  if (p.startsWith(HOME + path.sep)) return '~' + p.slice(HOME.length);
  return p;
}

function isProjectDir(p: string): boolean {
  for (const marker of PROJECT_MARKERS) {
    if (existsSync(path.join(p, marker))) return true;
  }
  return false;
}

function listProjectChildren(parent: string, max: number): string[] {
  let entries: string[];
  try {
    entries = readdirSync(parent);
  } catch {
    return [];
  }
  const out: string[] = [];
  for (const name of entries) {
    if (name.startsWith('.')) continue;
    const full = path.join(parent, name);
    let st;
    try { st = statSync(full); } catch { continue; }
    if (!st.isDirectory()) continue;
    if (isProjectDir(full)) {
      out.push(full);
      if (out.length >= max) break;
    }
  }
  return out.sort();
}

export function listDirectoryCandidates(): DirectoryCandidate[] {
  const seen = new Set<string>();
  const out: DirectoryCandidate[] = [];

  const push = (p: string, kind: DirectoryCandidate['kind']): void => {
    if (seen.has(p)) return;
    if (!existsSync(p)) return;
    seen.add(p);
    out.push({ path: p, label: tildify(p), kind });
  };

  // 1. Home itself.
  push(HOME, 'home');

  // 2. Common top-level dirs.
  for (const name of COMMON_SUBDIRS) {
    push(path.join(HOME, name), 'common');
  }

  // 3. Project-shaped subdirs of $HOME (top-level).
  const homeProjects = listProjectChildren(HOME, 20);
  for (const p of homeProjects) push(p, 'project');

  // 4. Project-shaped subdirs of any common dir we found that exists.
  for (const c of [...out]) {
    if (c.kind !== 'common') continue;
    const projects = listProjectChildren(c.path, 15);
    for (const p of projects) push(p, 'project');
    if (out.length >= 50) break;
  }

  return out;
}
