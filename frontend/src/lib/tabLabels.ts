// User-chosen labels for session tabs. Two layers:
//
//   labelById[sessionId] — explicit rename for a specific sessionsd
//     session. Overrides everything else.
//   labelByCwd[cwd]      — "friendly name for this project." Set
//     automatically every time the user renames a tab (we record
//     the cwd alongside the session id), so a fresh session or a
//     resume in the same folder inherits the same friendly name
//     without the user having to rename again. Also makes the
//     resume picker show your real project names instead of bare
//     folder basenames.
//
// Empty / missing override = fall back to the cwd-derived basename.

import { useSyncExternalStore } from 'react';
import type { SessionInfo } from '../types';

const STORAGE_KEY = 'sessions:tab-labels:v2';

interface Stored {
  byId: Record<string, string>;
  byCwd: Record<string, string>;
}

function read(): Stored {
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) {
      // Migrate v1 single-map shape, if present.
      const legacy = window.localStorage.getItem('sessions:tab-labels:v1');
      if (legacy) {
        const parsed = JSON.parse(legacy);
        if (parsed && typeof parsed === 'object') {
          const byId: Record<string, string> = {};
          for (const [k, v] of Object.entries(parsed)) {
            if (typeof k === 'string' && typeof v === 'string') byId[k] = v;
          }
          return { byId, byCwd: {} };
        }
      }
      return { byId: {}, byCwd: {} };
    }
    const parsed = JSON.parse(raw);
    if (parsed && typeof parsed === 'object') {
      return {
        byId: (parsed.byId && typeof parsed.byId === 'object') ? parsed.byId : {},
        byCwd: (parsed.byCwd && typeof parsed.byCwd === 'object') ? parsed.byCwd : {}
      };
    }
  } catch { /* ignore */ }
  return { byId: {}, byCwd: {} };
}

function write(state: Stored): void {
  try { window.localStorage.setItem(STORAGE_KEY, JSON.stringify(state)); }
  catch { /* quota / private mode — non-fatal */ }
}

let cache: Stored = read();
const subs = new Set<() => void>();
function notify(): void { for (const cb of subs) cb(); }

export function getTabLabel(sessionId: string): string | null {
  return cache.byId[sessionId] ?? null;
}

// Resolve label for a session, considering both id-specific AND cwd
// inheritance. The id override always wins; cwd is the fallback that
// makes new/resumed sessions feel like the same project.
export function resolveLabel(sessionId: string | null, cwd: string | null): string | null {
  if (sessionId && cache.byId[sessionId]) return cache.byId[sessionId];
  if (cwd && cache.byCwd[cwd]) return cache.byCwd[cwd];
  return null;
}

// Look up purely by cwd — used by the resume picker, where the
// "previous session" had a different id than what we'll spawn for the
// resume, but the cwd is the same.
export function getCwdLabel(cwd: string): string | null {
  return cache.byCwd[cwd] ?? null;
}

// Set the label for a specific sessionsd session. Also records the cwd
// → label mapping so future sessions in the same folder inherit it.
export function setTabLabel(sessionId: string, label: string, cwd?: string): void {
  const trimmed = label.trim();
  const byId = { ...cache.byId };
  const byCwd = { ...cache.byCwd };
  if (trimmed.length === 0) {
    delete byId[sessionId];
  } else {
    byId[sessionId] = trimmed;
    if (cwd) byCwd[cwd] = trimmed;
  }
  cache = { byId, byCwd };
  write(cache);
  notify();
}

// React hook — live label that updates the instant any other component
// renames. Pass cwd so we get the cwd-inherited fallback too.
export function useTabLabel(sessionId: string, cwd?: string): string | null {
  return useSyncExternalStore(
    (cb) => { subs.add(cb); return () => subs.delete(cb); },
    () => cache.byId[sessionId] ?? (cwd ? cache.byCwd[cwd] ?? null : null),
    () => null
  );
}

// Canonical label for a session from its metadata (no user overrides).
// Resolution order mirrors the tab strip's derivedLabel so every consumer
// (SessionTabs, GridView, MobileNav, pop-out title, …) shows the same
// name for the same session.
//   1. Claude's /rename title (user-facing, cross-client authoritative)
//   2. Sessions' explicit durable name (for example the first launcher task)
//   3. Claude's ai-title (auto-generated first-prompt summary)
//   4. explicit durable description
//   5. cwd basename — the project folder name, our traditional default
//   6. cmd basename or short id as last resort
//
// User's sessions tab rename is layered ABOVE this by callers (via
// getTabLabel / useTabLabel) so it always wins without being part of
// this function.
export function sessionLabel(session: SessionInfo): string {
  if (session.claudeCustomTitle && session.claudeCustomTitle.length > 0) return session.claudeCustomTitle;
  if (session.name && session.name.length > 0) return session.name;
  if (session.claudeAiTitle && session.claudeAiTitle.length > 0) return session.claudeAiTitle;
  if (session.description && session.description.length > 0) return session.description;
  if (session.cwd && session.cwd.length > 0) {
    const parts = session.cwd.split('/').filter(Boolean);
    const last = parts[parts.length - 1];
    if (last) return last;
  }
  return session.cmd || session.id.slice(0, 6);
}
