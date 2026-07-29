import type { SessionInfo } from '../types';

export interface WorkingSetGroups {
  runningRoots: SessionInfo[];
  setAsideRoots: SessionInfo[];
  ended: SessionInfo[];
  runningIds: Set<string>;
  setAsideIds: Set<string>;
}

export function isSetAside(session: SessionInfo): boolean {
  return !session.exited && session.setAsideAt != null;
}

export function effectiveParentId(session: SessionInfo): string | undefined {
  return session.displayParentSessionId !== undefined
    ? session.displayParentSessionId || undefined
    : session.parentSessionId;
}

// Set-aside is working-set organization, not lifecycle. This reducer keeps
// live non-aside descendants reachable by promoting them across a set-aside
// ancestor, while retaining ended ancestors only when they are the path to
// live work.
export function groupWorkingSet(
  sessions: SessionInfo[],
  openSessionIds: string[],
  pins: string[]
): WorkingSetGroups {
  const byId = new Map(sessions.map((session) => [session.id, session]));
  const children = new Map<string, SessionInfo[]>();
  for (const session of sessions) {
    const parentId = effectiveParentId(session);
    if (!parentId || !byId.has(parentId)) continue;
    const nested = children.get(parentId) ?? [];
    nested.push(session);
    children.set(parentId, nested);
  }
  const open = new Set(openSessionIds);
  const pinned = new Set(pins);
  const runningMemo = new Map<string, boolean>();
  const visiting = new Set<string>();
  const belongsInRunning = (session: SessionInfo): boolean => {
    const cached = runningMemo.get(session.id);
    if (cached !== undefined) return cached;
    if (visiting.has(session.id)) return false;
    visiting.add(session.id);
    let result: boolean;
    if (!session.exited) {
      result = open.has(session.id) || pinned.has(session.id) || !isSetAside(session);
    } else {
      result = (children.get(session.id) ?? []).some(belongsInRunning);
    }
    visiting.delete(session.id);
    runningMemo.set(session.id, result);
    return result;
  };

  const runningIds = new Set(sessions.filter(belongsInRunning).map((session) => session.id));
  const setAsideIds = new Set(sessions
    .filter((session) => isSetAside(session) && !open.has(session.id) && !pinned.has(session.id))
    .map((session) => session.id));
  const rootsFor = (ids: Set<string>): SessionInfo[] => sessions.filter((session) => {
    if (!ids.has(session.id)) return false;
    const parentId = effectiveParentId(session);
    return !parentId || !ids.has(parentId);
  });

  return {
    runningRoots: rootsFor(runningIds),
    setAsideRoots: rootsFor(setAsideIds),
    ended: sessions.filter((session) => session.exited && !runningIds.has(session.id)),
    runningIds,
    setAsideIds
  };
}
