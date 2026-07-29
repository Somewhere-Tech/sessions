// Resolving which Claude Code JSONL file belongs to a prettyd session.
//
// Claude persists each session as ~/.claude/projects/<encoded-cwd>/<id>.jsonl.
// prettyd pins an id at spawn via `--session-id <uuid>` (fresh) or
// `--resume <uuid>` (resume), and that uuid is normally the JSONL
// filename. But in practice the file we want can diverge from the pinned
// id:
//   - the user resumed a pre-existing conversation, so Claude appended to
//     an older file and never created <pinnedId>.jsonl;
//   - the pinned file was cleaned up (empty) and a different one carries
//     the conversation.
//
// This module is the pure, testable resolution policy. The watcher
// (sessionFileWatcher.ts) drives the filesystem watching/tailing and
// calls in here to decide *which* file to follow.

import * as fs from 'node:fs';
import * as path from 'node:path';
import * as os from 'node:os';

// Convert a cwd like /Users/example/Projects/rail-me into Claude's
// directory-name encoding "-Users-example-Projects-rail-me". Every '/'
// becomes '-'.
export function encodeCwd(cwd: string): string {
  return cwd.replace(/\//g, '-');
}

export function projectDirFor(cwd: string): string {
  return path.join(os.homedir(), '.claude', 'projects', encodeCwd(cwd));
}

export type ResolveReason =
  | 'exact'       // <launchUuid>.jsonl exists — authoritative
  | 'sole-file'   // launch file absent, exactly one .jsonl in the dir
  | 'ambiguous'   // launch file absent, multiple .jsonl — refuse to guess
  | 'empty-dir'   // dir exists, no .jsonl yet
  | 'no-dir';     // project dir doesn't exist yet

export interface JsonlResolution {
  // Absolute path to the JSONL we should follow, or null if unresolved.
  path: string | null;
  reason: ResolveReason;
}

// List *.jsonl basenames in a project dir. Empty array if the dir is
// missing or unreadable.
export function listJsonlFiles(dir: string): string[] {
  try {
    return fs.readdirSync(dir).filter((n) => n.endsWith('.jsonl'));
  } catch {
    return [];
  }
}

// Decide which JSONL to follow for a session.
//
// Priority:
//   1. <launchUuid>.jsonl if present — Claude was explicitly told this id.
//   2. If exactly one .jsonl exists in the dir, follow it. Unambiguous,
//      and the common case when the user resumed an existing conversation
//      (so Claude wrote to a pre-existing file rather than our pinned id).
//   3. Otherwise unresolved (null). We deliberately do NOT pick the
//      "newest" among multiple unrelated files: two Claude sessions open
//      in the same folder is common, and the newest is by definition the
//      OTHER one — tailing it would show the wrong conversation with no
//      way for the user to tell. An empty Pretty pane is the correct
//      failure mode; a wrong one is not.
export function resolveJsonlPath(dir: string, launchUuid?: string): JsonlResolution {
  let files: string[];
  try {
    files = fs.readdirSync(dir).filter((n) => n.endsWith('.jsonl'));
  } catch {
    return { path: null, reason: 'no-dir' };
  }
  if (launchUuid) {
    const exact = `${launchUuid}.jsonl`;
    if (files.includes(exact)) {
      return { path: path.join(dir, exact), reason: 'exact' };
    }
  }
  if (files.length === 0) return { path: null, reason: 'empty-dir' };
  if (files.length === 1) return { path: path.join(dir, files[0]!), reason: 'sole-file' };
  return { path: null, reason: 'ambiguous' };
}
