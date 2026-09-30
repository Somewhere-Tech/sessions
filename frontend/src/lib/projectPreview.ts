import type { SessionInfo } from '../types';
import { classifySession, sessionNeedsYou } from './sessionStatus';
import { isPinned } from './workingSet';

export const PROJECT_PREVIEW_LIMIT = 3;

// Presentation only: preserve the existing order and membership. Attention
// and the selected conversation remain visible even when they exceed the cap.
export function projectPreview<T>(rows: T[], sessionOf: (row: T) => SessionInfo, selected: (row: T) => boolean): T[] {
  const important = (row: T): boolean => {
    const session = sessionOf(row);
    return selected(row) || isPinned(session) || sessionNeedsYou(session) || classifySession(session).state === 'working';
  };
  let remaining = Math.max(0, PROJECT_PREVIEW_LIMIT - rows.filter(important).length);
  return rows.filter((row) => important(row) || remaining-- > 0);
}
