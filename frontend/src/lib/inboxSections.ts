import type { SessionInfo } from '../types';
import { classifySession, sessionNeedsYou } from './sessionStatus';

// The inbox is organized by attention, then by the work a session belongs to.
// This module is the pure part: given the sessions in scope and the project
// each one resolves to, it produces the strip and the sections the navigator
// renders. Nothing here reads the network or the DOM, so the shape of the
// inbox is testable as data.

export interface ProjectRef {
  id: string;
  name: string;
  implicit: boolean;
  pinned?: boolean;
}

export interface InboxSection {
  id: string;
  name: string;
  implicit: boolean;
  // Live, reachable sessions the person talks to, engagement order.
  live: SessionInfo[];
  // Live sessions whose runner cannot be reached right now. They are not
  // ended, but they are not something to type into either.
  notConnected: SessionInfo[];
  // Recently ended sessions in this project, newest first, capped.
  finished: SessionInfo[];
  needsYou: number;
  updatedAt: number;
}

export interface InboxLayout {
  // The strip at the top: sessions waiting on the person, across every
  // project, capped so six never becomes a wall. `moreNeedsYou` is how many
  // were folded.
  needsYou: SessionInfo[];
  moreNeedsYou: number;
  sections: InboxSection[];
  // Sessions that resolved to no named project fold into this one section.
  other: InboxSection | null;
}

export const NEEDS_YOU_STRIP_LIMIT = 3;
export const FINISHED_PER_PROJECT_LIMIT = 5;
export const OTHER_PROJECTS_ID = 'other-projects';

function isNotConnected(session: SessionInfo): boolean {
  const state = classifySession(session).state;
  return state === 'unavailable' || state === 'reconnecting' || state === 'needs-recovery';
}

// A one-line reason for the not-connected fold, in the person's words.
export function notConnectedReason(session: SessionInfo): string {
  if (session.unreachableReason === 'restart-restore-pending') return 'not restored after restart · Resume to continue';
  if (session.pid && session.pid > 0) return 'reconnecting to its runner';
  return 'its machine or runner is offline';
}

export function buildInboxLayout(options: {
  live: SessionInfo[];
  ended: SessionInfo[];
  // Every live session that may need the person, including lanes that the
  // list itself folds away. A blocked lane must reach the strip and its
  // project's count even while its row sits under its manager.
  attention?: SessionInfo[];
  lastActivity: (session: SessionInfo) => number;
  projectFor: (session: SessionInfo) => ProjectRef | null;
}): InboxLayout {
  const { live, ended, lastActivity, projectFor } = options;
  const attention = options.attention ?? live;
  const waiting = attention.filter(sessionNeedsYou).sort((a, b) => lastActivity(b) - lastActivity(a));

  const sections = new Map<string, InboxSection>();
  const sectionFor = (session: SessionInfo): InboxSection => {
    const ref = projectFor(session);
    const key = ref && !ref.implicit ? ref.id : OTHER_PROJECTS_ID;
    let section = sections.get(key);
    if (!section) {
      section = {
        id: key,
        name: ref && !ref.implicit ? ref.name : 'Other projects',
        implicit: !ref || ref.implicit,
        live: [], notConnected: [], finished: [], needsYou: 0, updatedAt: 0
      };
      sections.set(key, section);
    }
    return section;
  };

  for (const session of live) {
    const section = sectionFor(session);
    if (isNotConnected(session)) section.notConnected.push(session); else section.live.push(session);
    section.updatedAt = Math.max(section.updatedAt, lastActivity(session));
  }
  for (const session of waiting) {
    sectionFor(session).needsYou += 1;
  }
  for (const session of [...ended].sort((a, b) => lastActivity(b) - lastActivity(a))) {
    const section = sectionFor(session);
    if (section.finished.length < FINISHED_PER_PROJECT_LIMIT) section.finished.push(session);
    section.updatedAt = Math.max(section.updatedAt, lastActivity(session));
  }

  const other = sections.get(OTHER_PROJECTS_ID) ?? null;
  const named = [...sections.values()]
    .filter((section) => section.id !== OTHER_PROJECTS_ID)
    .sort((a, b) => b.updatedAt - a.updatedAt);

  return {
    needsYou: waiting.slice(0, NEEDS_YOU_STRIP_LIMIT),
    moreNeedsYou: Math.max(0, waiting.length - NEEDS_YOU_STRIP_LIMIT),
    sections: named,
    other
  };
}
