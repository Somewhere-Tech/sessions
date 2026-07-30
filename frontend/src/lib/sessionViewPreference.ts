export type SessionViewMode = 'terminal' | 'remote';

const GLOBAL_VIEW_KEY = 'sessions:viewMode';
const NEXT_VIEW_PREFIX = 'sessions:next-view:';

export function readInitialSessionView(sessionId: string): SessionViewMode {
  try {
    const oneShotKey = `${NEXT_VIEW_PREFIX}${sessionId}`;
    const oneShot = window.sessionStorage.getItem(oneShotKey);
    if (oneShot === 'terminal' || oneShot === 'remote') {
      window.sessionStorage.removeItem(oneShotKey);
      return oneShot;
    }
    const saved = window.localStorage.getItem(GLOBAL_VIEW_KEY);
    // Conversation is the durable default. Terminal is an escape hatch and
    // should not make every subsequently opened provider session start raw.
    if (saved === 'terminal' || saved === 'remote') return 'remote';
    if (saved === 'details' || saved === 'reflowed' || saved === 'sessions' || saved === 'split') {
      return 'remote';
    }
  } catch {
    // Storage is optional; the readable conversation view remains the default.
  }
  return 'remote';
}

export function writeSessionView(mode: SessionViewMode): void {
  try {
    window.localStorage.setItem(GLOBAL_VIEW_KEY, mode === 'terminal' ? 'remote' : mode);
  } catch {
    // Storage is optional.
  }
}

export function preferNextSessionView(sessionId: string, mode: SessionViewMode): void {
  try {
    window.sessionStorage.setItem(`${NEXT_VIEW_PREFIX}${sessionId}`, mode);
  } catch {
    // The normal saved preference is a safe fallback.
  }
}
