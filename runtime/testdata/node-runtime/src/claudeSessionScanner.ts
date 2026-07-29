// Scan ~/.claude/projects/* for resumable Claude sessions. Each
// project dir (named after the encoded cwd) contains one .jsonl per
// past session. We return a flat list grouped by decoded cwd, with
// just enough metadata to render a "pick a session to resume" UI:
// session UUID, mtime, first user message preview, event count.
//
// Read cost: we slurp at most the first 16 KB of each file to extract
// the first event (cheap), and use fs.stat for mtime. Even with
// hundreds of sessions, this returns in well under a second.

import * as fs from 'node:fs';
import * as fsp from 'node:fs/promises';
import * as path from 'node:path';
import * as os from 'node:os';

export interface ResumableSession {
  // Claude's session uuid — also the JSONL filename. Pass to
  // `claude --resume <id>` to continue this session.
  sessionId: string;
  // Decoded cwd (e.g. "/Users/example/project"). Used to group
  // sessions by project in the picker.
  cwd: string;
  // mtime of the .jsonl. Used to sort newest-first.
  modifiedAt: number;
  // First user message text — gives the user a recognizable preview
  // ("ok lets ship the thing", "fix the auth issue", etc.). Empty if
  // the session has no user input yet.
  firstUserMessage: string;
  // Size of the .jsonl file in bytes. Rough proxy for conversation
  // length when the user is scanning the list.
  sizeBytes: number;
}

const PROJECTS_DIR = path.join(os.homedir(), '.claude', 'projects');

// Reverse Claude's cwd encoding: "-Users-example-Projects-rail-me" →
// "/Users/example/Projects/rail-me". Each '-' is restored to '/'. This
// is lossy in theory (a cwd with '-' in a folder name encodes the
// same as '/' there) but in practice the encoded form is what Claude
// produced from the actual cwd, so reversing always recovers a
// reasonable display path.
function decodeCwd(encoded: string): string {
  // Claude encodes "/Users/example/foo" as "-Users-example-foo" — the
  // leading dash is the root '/'. Restoring: each '-' → '/'.
  return encoded.replace(/-/g, '/');
}

// Read just the first ~16 KB of a .jsonl to get its first event. JSONL
// is line-delimited, so we look for the first complete line and parse
// it as JSON. Returns undefined if the file is empty / unreadable /
// malformed.
async function firstUserMessageOf(filePath: string): Promise<string> {
  let fh: fsp.FileHandle | null = null;
  try {
    fh = await fsp.open(filePath, 'r');
    const buf = Buffer.alloc(16384);
    const { bytesRead } = await fh.read(buf, 0, buf.length, 0);
    if (bytesRead === 0) return '';
    const text = buf.subarray(0, bytesRead).toString('utf8');
    // Parse line by line until we find a user event with a real text
    // body (skip sidechain, tool_results, etc.).
    for (const line of text.split('\n')) {
      if (!line.trim()) continue;
      let ev: unknown;
      try { ev = JSON.parse(line); } catch { continue; }
      if (typeof ev !== 'object' || ev === null) continue;
      const e = ev as Record<string, unknown>;
      if (e.type !== 'user') continue;
      const msg = e.message as Record<string, unknown> | undefined;
      const content = msg?.content;
      if (typeof content === 'string') {
        return content.replace(/\s+/g, ' ').trim().slice(0, 200);
      }
      if (Array.isArray(content)) {
        // Skip tool_result-only events (system loop feedback, not
        // human input). Take the first text block we find.
        for (const block of content) {
          if (block && typeof block === 'object'
            && (block as Record<string, unknown>).type === 'text') {
            const t = (block as Record<string, unknown>).text;
            if (typeof t === 'string') {
              return t.replace(/\s+/g, ' ').trim().slice(0, 200);
            }
          }
        }
      }
    }
    return '';
  } catch {
    return '';
  } finally {
    try { await fh?.close(); } catch { /* ignore */ }
  }
}

// Walk every project dir and return one ResumableSession per .jsonl.
// Sorted newest-first by mtime. Concurrency-limited so we don't slam
// the FS with thousands of fhs at once.
export async function scanResumableSessions(): Promise<ResumableSession[]> {
  let projectEntries: fs.Dirent[];
  try {
    projectEntries = await fsp.readdir(PROJECTS_DIR, { withFileTypes: true });
  } catch {
    return [];
  }
  // Gather (file path, decoded cwd) pairs from every project dir.
  const tasks: Array<{ filePath: string; cwd: string; sessionId: string }> = [];
  for (const dir of projectEntries) {
    if (!dir.isDirectory()) continue;
    const cwd = decodeCwd(dir.name);
    const dirPath = path.join(PROJECTS_DIR, dir.name);
    let files: fs.Dirent[];
    try { files = await fsp.readdir(dirPath, { withFileTypes: true }); } catch { continue; }
    for (const f of files) {
      if (!f.isFile() || !f.name.endsWith('.jsonl')) continue;
      const sessionId = f.name.slice(0, -'.jsonl'.length);
      tasks.push({ filePath: path.join(dirPath, f.name), cwd, sessionId });
    }
  }

  // Fan out stat + first-message reads, bounded concurrency.
  const out: ResumableSession[] = [];
  const POOL = 16;
  for (let i = 0; i < tasks.length; i += POOL) {
    const batch = tasks.slice(i, i + POOL);
    const results = await Promise.all(batch.map(async (t) => {
      try {
        const [st, firstMsg] = await Promise.all([
          fsp.stat(t.filePath),
          firstUserMessageOf(t.filePath)
        ]);
        return {
          sessionId: t.sessionId,
          cwd: t.cwd,
          modifiedAt: st.mtimeMs,
          firstUserMessage: firstMsg,
          sizeBytes: st.size
        } satisfies ResumableSession;
      } catch {
        return null;
      }
    }));
    for (const r of results) if (r) out.push(r);
  }

  // Newest first.
  out.sort((a, b) => b.modifiedAt - a.modifiedAt);
  return out;
}
