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

// The pin is daemon-owned: the user marked this session as a workbench, and
// the mark survives daemon restarts. It is read here rather than from local
// client state so the app, the CLI, and any other client agree about which
// sessions are pinned.
export function isPinned(session: SessionInfo): boolean {
  return session.pinned === true;
}

export function pinnedSessionIds(sessions: SessionInfo[]): string[] {
  return sessions.filter(isPinned).map((session) => session.id);
}

// pinnedFirst floats the pinned sessions to the top of a group without
// disturbing anything else about its order. It is a stable partition rather
// than a comparator over a "pinned" key so that the ordering the list already
// had — engagement, activity, whatever a caller sorted by — is exactly
// preserved inside each of the two halves.
export function pinnedFirst(sessions: SessionInfo[]): SessionInfo[] {
  return [...sessions.filter(isPinned), ...sessions.filter((session) => !isPinned(session))];
}

export function effectiveParentId(session: SessionInfo): string | undefined {
  return session.displayParentSessionId !== undefined
    ? session.displayParentSessionId || undefined
    : session.parentSessionId;
}

// User attention, not provider chatter, decides where work appears. This keeps
// a manager stable while its agents stream tools and output, but lets a thread
// the user actually returns to rise naturally without requiring a pin.
export function humanEngagementAt(session: SessionInfo): number {
  return Math.max(session.lastUserMessageAt ?? 0, session.createdAt || 0);
}

// New clients record how a child was requested. Older provider lanes already
// carry KindLane, so they can receive the same compact helper treatment while
// legacy child sessions of unknown provenance remain fully visible.
export function isAgentLedChild(session: SessionInfo): boolean {
  return session.delegationKind === 'agent' || session.kind === 'lane';
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
      // An ended conversation can still be work the user is actively reading.
      // Keep an open or pinned record in focus; closing its view returns it to
      // Quiet without changing or deleting provider history.
      result = open.has(session.id) || pinned.has(session.id)
        || (children.get(session.id) ?? []).some(belongsInRunning);
    }
    visiting.delete(session.id);
    runningMemo.set(session.id, result);
    return result;
  };

  const runningIds = new Set(sessions.filter(belongsInRunning).map((session) => session.id));
  const setAsideIds = new Set(sessions
    .filter((session) => isSetAside(session) && !open.has(session.id) && !pinned.has(session.id))
    .map((session) => session.id));
  const includeDescendants = (
    session: SessionInfo,
    target: Set<string>,
    shouldSkip: (child: SessionInfo) => boolean,
    seen = new Set<string>()
  ): void => {
    if (seen.has(session.id)) return;
    seen.add(session.id);
    for (const child of children.get(session.id) ?? []) {
      if (shouldSkip(child)) continue;
      target.add(child.id);
      includeDescendants(child, target, shouldSkip, seen);
    }
  };
  // Finished descendants remain part of their manager's story so the
  // navigator can collapse them into a useful completion rollup. A child that
  // the user explicitly moved to Later forms a separate working-set root.
  for (const session of sessions.filter((candidate) => runningIds.has(candidate.id))) {
    includeDescendants(session, runningIds, (child) => (
      isSetAside(child) && !open.has(child.id) && !pinned.has(child.id)
    ));
  }
  for (const session of sessions.filter((candidate) => setAsideIds.has(candidate.id))) {
    includeDescendants(session, setAsideIds, (child) => (
      open.has(child.id) || pinned.has(child.id) || runningIds.has(child.id)
    ));
  }
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
