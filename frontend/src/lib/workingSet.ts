import type { SessionInfo } from '../types';
import { providerConversationId } from './sessionStatus';

export interface WorkingSetGroups {
  // The user's own group. A pinned session is here and nowhere else: it is
  // removed from Live, from Quiet, and from Ended, so the navigator can render
  // one labelled "Pinned" section and every session still appears exactly once.
  pinnedRoots: SessionInfo[];
  runningRoots: SessionInfo[];
  setAsideRoots: SessionInfo[];
  ended: SessionInfo[];
  pinnedIds: Set<string>;
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

function providerConversationKey(session: SessionInfo): string | null {
  const id = providerConversationId(session);
  if (!id) return null;
  const provider = session.tool === 'claude-code' || session.tool === 'codex'
    ? session.tool
    : null;
  return provider ? `${provider}:${id}` : null;
}

// Runtime records remain intact in the daemon and Details, but the navigator
// represents one provider conversation once. A live runtime wins over an
// ended predecessor; otherwise the newest runtime wins. Children attached to
// an older runtime are rewired only in this presentation copy so its manager
// tree remains whole without changing trusted creator provenance.
export function collapseConversationRuntimes(sessions: SessionInfo[]): SessionInfo[] {
  const groups = new Map<string, SessionInfo[]>();
  for (const session of sessions) {
    const key = providerConversationKey(session);
    if (!key) continue;
    const group = groups.get(key) ?? [];
    group.push(session);
    groups.set(key, group);
  }

  const replacement = new Map<string, string>();
  const hidden = new Set<string>();
  for (const group of groups.values()) {
    if (group.length < 2) continue;
    const representative = [...group].sort((left, right) => {
      if (left.exited !== right.exited) return left.exited ? 1 : -1;
      return right.createdAt - left.createdAt;
    })[0];
    for (const session of group) {
      if (session.id === representative.id) continue;
      replacement.set(session.id, representative.id);
      hidden.add(session.id);
    }
  }
  if (hidden.size === 0) return sessions;

  const byId = new Map(sessions.map((session) => [session.id, session]));
  const parentOf = (session: SessionInfo): string | undefined => (
    session.displayParentSessionId !== undefined
      ? session.displayParentSessionId || undefined
      : session.parentSessionId
  );
  const resolvedParent = (session: SessionInfo): string | undefined => {
    let parent = parentOf(session);
    const visited = new Set<string>();
    while (parent && replacement.has(parent) && !visited.has(parent)) {
      visited.add(parent);
      const next = replacement.get(parent);
      if (next && next !== session.id) return next;
      parent = byId.has(parent) ? parentOf(byId.get(parent)!) : undefined;
    }
    return parent === session.id ? undefined : parent;
  };

  return sessions.filter((session) => !hidden.has(session.id)).map((session) => {
    const before = parentOf(session);
    const after = resolvedParent(session);
    if (before === after) return session;
    return session.displayParentSessionId !== undefined
      ? { ...session, displayParentSessionId: after ?? '' }
      : { ...session, parentSessionId: after };
  });
}

// The daemon's own reason for refusing a pin on an ended conversation, in the
// words it answers 409 with (`runtime/internal/session/manager.go`
// `UpdatePinned`). Every surface that offers the pin says this one sentence, so
// the row menu and the details panel cannot grow two different excuses.
export const PIN_UNAVAILABLE_WHEN_ENDED = 'A pin exempts a live session from '
  + 'automatic cleanup and cannot protect one that already ended. Archive it '
  + 'instead.';

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
//
// Pinning outranks all of it. `docs/PHILOSOPHY.md`: a pin is the user marking a
// workbench, and the machinery keeps its hands off it. The pinned sessions
// therefore do not merely sort first inside whichever group the machinery chose
// for them — they LEAVE that group and form their own, because a group the user
// composed by hand is a different kind of thing from a group a classifier
// composed, and burying one inside the other is what made the mark invisible.
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

  // ── The Pinned group ──────────────────────────────────────────────────────
  // Built last, out of the groups above, so nothing about how the machinery
  // classifies a session had to change: a pin only decides which section the
  // session is finally filed under.
  //
  // A pinned session brings its children with it. The alternative — pin the
  // parent, leave the children under Live — would show the same manager in two
  // places at once and strand its delegates in a group their manager left, and
  // "sessions with the same manager appear together" is the nesting rule the
  // rest of this file already enforces (see the finished-descendant rollup
  // above). The one exception is the one that already exists: a child the user
  // explicitly moved to Later stays in Quiet, because that is also a mark the
  // user made by hand and a pin on the parent does not overrule it.
  const pinnedIds = new Set(sessions
    .filter((session) => pinned.has(session.id))
    .map((session) => session.id));
  for (const session of sessions.filter((candidate) => pinnedIds.has(candidate.id))) {
    includeDescendants(session, pinnedIds, (child) => (
      isSetAside(child) && !open.has(child.id) && !pinned.has(child.id)
    ));
  }
  // Exactly once in the tree: whatever the Pinned group holds, no other group
  // may also hold.
  for (const id of pinnedIds) {
    runningIds.delete(id);
    setAsideIds.delete(id);
  }

  const rootsFor = (ids: Set<string>): SessionInfo[] => sessions.filter((session) => {
    if (!ids.has(session.id)) return false;
    const parentId = effectiveParentId(session);
    return !parentId || !ids.has(parentId);
  });

  return {
    pinnedRoots: rootsFor(pinnedIds),
    runningRoots: rootsFor(runningIds),
    setAsideRoots: rootsFor(setAsideIds),
    // An ended conversation the user pinned is still the user's; it belongs in
    // Pinned rather than in the ended pile, which is the behaviour the pin
    // already had when it kept such a record in focus.
    ended: sessions.filter((session) => (
      session.exited && !runningIds.has(session.id) && !pinnedIds.has(session.id)
    )),
    pinnedIds,
    runningIds,
    setAsideIds
  };
}
