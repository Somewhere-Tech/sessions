// Legacy browser-local labels for session tabs. Two layers:
//
//   labelById[sessionId] — explicit rename for a specific sessionsd
//     session. Durable session names returned by sessionsd replace this
//     compatibility value during every refresh.
//   labelByCwd[cwd]      — "friendly name for this project." Set
//     by older versions when a user renamed a tab. It remains a fallback
//     for unnamed legacy sessions and resume-picker project hints.
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

// Set a browser-local compatibility label. New durable renames omit cwd so
// one conversation title does not silently become every future chat's title.
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

// sessionsd is the canonical title store. Reconcile its durable names into
// the compatibility cache before React renders a refreshed session list so
// stale browser-local labels can never hide CLI, Fleet, or remote renames.
export function reconcileDurableTabLabels(
  sessions: Array<Pick<SessionInfo, 'id' | 'name'>>
): void {
  let changed = false;
  const byId = { ...cache.byId };
  for (const session of sessions) {
    const durable = session.name?.trim();
    if (!durable || byId[session.id] === durable) continue;
    byId[session.id] = durable;
    changed = true;
  }
  if (!changed) return;
  cache = { ...cache, byId };
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

function boundedTitle(value: string, maxRunes = 56): string {
  const compact = value.replace(/\s+/g, ' ').trim();
  const runes = Array.from(compact);
  if (runes.length <= maxRunes) return compact;
  const clipped = runes.slice(0, maxRunes - 1).join('');
  const wordBoundary = clipped.lastIndexOf(' ');
  const safe = wordBoundary >= Math.floor(maxRunes * .62) ? clipped.slice(0, wordBoundary) : clipped;
  return `${safe.replace(/[,:;\-–—]+$/g, '').trim()}…`;
}

/** Turn an initial request into a calm list label without changing its meaning. */
export function sessionTitleFromPrompt(value: string): string {
  let title = value.split('\n')[0]?.replace(/\s+/g, ' ').trim() ?? '';
  title = title
    .replace(/^(?:hey|hello|hi|okay|ok|so)\b[\s,.:;!-]*/i, '')
    .replace(/^(?:please\s+)?(?:can|could|would|will)\s+you\s+/i, '')
    .replace(/^i\s+(?:really\s+)?(?:want|need|would\s+like)(?:\s+you)?\s+to\s+/i, '')
    .replace(/^this\s+(?:chat|conversation|session)\s+is\s+about\s+/i, '')
    .replace(/^i\s+(?:have|had)\s+(?:(?:some|a\s+few)\s+)?questions?\s+(?:about|on)\s+/i, 'Questions about ')
    .replace(/^look\s+at\s+this\s+repo\s+and\s+tell\s+me\s+whether\s+it\s+is\s+/i, 'Check whether this repo is ')
    .replace(/^please\s+/i, '')
    .trim();
  if (!title) title = value.replace(/\s+/g, ' ').trim();
  if (title) title = `${title.charAt(0).toUpperCase()}${title.slice(1)}`;
  return boundedTitle(title);
}

// Canonical label for a session from its metadata (no user overrides).
// Resolution order mirrors the tab strip's derivedLabel so every consumer
// (SessionTabs, GridView, MobileNav, pop-out title, …) shows the same
// name for the same session.
//   1. Sessions' explicit durable name
//   2. Claude's /rename title (imported when Sessions has no title)
//   3. Claude's ai-title (auto-generated first-prompt summary)
//   4. explicit durable description
//   5. cwd basename — the project folder name, our traditional default
//   6. cmd basename or short id as last resort
//
// Browser-local aliases are retained only for pre-durable-name compatibility;
// refresh reconciliation makes a durable name win in every consumer.
export function sessionLabel(session: SessionInfo): string {
  const name = session.name?.trim() ?? '';
  const description = session.description?.trim() ?? '';
  if (session.name_source === 'explicit' && name) return boundedTitle(name);
  if (session.claudeCustomTitle?.trim()) return boundedTitle(session.claudeCustomTitle);
  if (session.claudeAiTitle?.trim()) return boundedTitle(session.claudeAiTitle);
  if (session.name_source === 'provider' && name) return boundedTitle(name);
  if (session.name_source === 'launch') {
    if (description) return sessionTitleFromPrompt(description);
    if (name) return sessionTitleFromPrompt(name);
  }
  // Sessions created before name provenance was recorded often already have a
  // useful human/provider title. Preserve it instead of replacing it with an
  // implementation-oriented description or first command.
  if (name && !/^(?:claude|codex|shell)\s+[-—]/i.test(name)) {
    const conversational = /^(?:hey\b|hello\b|hi\b|okay\b|ok\b|so\b|i\s+(?:want|need|would\s+like|have|had)\b|this\s+(?:chat|conversation|session)\b|(?:can|could|would|will)\s+you\b|please\b)/i;
    return conversational.test(name) ? sessionTitleFromPrompt(name) : boundedTitle(name);
  }
  if (description) return sessionTitleFromPrompt(description);
  if (session.cwd && session.cwd.length > 0) {
    const parts = session.cwd.split('/').filter(Boolean);
    const last = parts[parts.length - 1];
    if (last) return last;
  }
  return session.cmd || session.id.slice(0, 6);
}

/** Resolve old browser labels while still shaping daemon-owned launch names. */
export function resolvedSessionLabel(session: SessionInfo): string {
  const stored = getTabLabel(session.id);
  if (stored && stored !== session.name) return boundedTitle(stored);
  return sessionLabel(session);
}
