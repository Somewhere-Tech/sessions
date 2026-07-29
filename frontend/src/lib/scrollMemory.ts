// Per-session scroll-position cache. SessionView remounts on session
// switch (key={activeId}), so internal scroll state is otherwise lost.
// Each pane writes its scrollTop here on every scroll event; on remount
// the pane reads the cached position and restores. If `atBottom` was
// true at save time we preserve that semantically — auto-stick still
// follows new content rather than freezing at a literal pixel offset.

export type Pane = 'sessions' | 'remote';

export interface ScrollPosition {
  scrollTop: number;
  atBottom: boolean;
}

const cache = new Map<string, Map<Pane, ScrollPosition>>();

export function saveScrollPosition(sessionId: string, pane: Pane, pos: ScrollPosition): void {
  let m = cache.get(sessionId);
  if (!m) {
    m = new Map();
    cache.set(sessionId, m);
  }
  m.set(pane, pos);
}

export function readScrollPosition(sessionId: string, pane: Pane): ScrollPosition | null {
  return cache.get(sessionId)?.get(pane) ?? null;
}
