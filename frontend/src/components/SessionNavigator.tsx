import { useEffect, useMemo, useState, type DragEvent } from 'react';
import { createPortal } from 'react-dom';
import type { SessionInfo } from '../types';
import { getTabLabel, sessionLabel } from '../lib/tabLabels';
import { ProviderMark, normalizeProvider } from './ProviderBadge';
import {
  canContinueSession,
  classifySession,
  endedAtLabel,
  endedSummary,
  sessionIsFinished,
  sessionNeedsYou,
  sessionWantsAttention
} from '../lib/sessionStatus';
import {
  effectiveParentId,
  collapseConversationRuntimes,
  groupWorkingSet,
  humanEngagementAt,
  isAgentLedChild,
  isPinned,
  pinnedFirst,
  pinnedSessionIds,
  PIN_UNAVAILABLE_WHEN_ENDED
} from '../lib/workingSet';
import { useSessions } from '../store/sessions';
import { MachineMark } from './MachineMark';
import { ContinueElsewhereButton } from './ContinueElsewhereButton';
import { serverDisplayName, useServers } from '../lib/servers';
import { useFleetSessions, type FleetSessionSnapshot } from '../hooks/useFleetSessions';

type PrimaryFilter = 'all' | 'needs' | 'working' | 'ended';
type ProviderFilter = 'all' | 'claude' | 'codex' | 'shell';
type DateFilter = 'all' | 'today' | 'week';

const RECENTLY_ENDED_DAYS = 7;
const RECENTLY_ENDED_LIMIT = 20;
const MACHINE_SCOPE_KEY = 'sessions:navigator-machine-scope';
const ALL_MACHINES_SCOPE = 'all-machines';

type MachineScope = typeof ALL_MACHINES_SCOPE | string;

function lastActivity(session: SessionInfo): number {
  return Math.max(session.lastDataAt || 0, session.exitedAt ?? 0, session.createdAt || 0);
}

function relativeTime(at: number): string {
  const delta = Math.max(0, Date.now() - at);
  if (delta < 60_000) return 'now';
  if (delta < 3_600_000) return `${Math.floor(delta / 60_000)}m`;
  if (delta < 86_400_000) return `${Math.floor(delta / 3_600_000)}h`;
  if (delta < 604_800_000) return `${Math.floor(delta / 86_400_000)}d`;
  return new Date(at).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
}

function projectName(session: SessionInfo): string {
  const tagged = session.tags?.project ?? session.tags?.product;
  if (tagged) return tagged;
  const path = session.sourceRepo || session.cwd;
  return path.split('/').filter(Boolean).pop() ?? path;
}

// Status classification lives in lib/sessionStatus.ts. This file used to
// answer "is it finished / does it need me?" twice, in two different ways,
// and disagreed with Fleet and Home. It now only asks.

function readMachineScope(activeMachineId: string | null): MachineScope {
  try {
    return window.localStorage.getItem(MACHINE_SCOPE_KEY) || activeMachineId || ALL_MACHINES_SCOPE;
  } catch {
    return activeMachineId || ALL_MACHINES_SCOPE;
  }
}

function writeMachineScope(scope: MachineScope): void {
  try { window.localStorage.setItem(MACHINE_SCOPE_KEY, scope); } catch { /* preference only */ }
}

function orderedMachineRows(sessions: SessionInfo[]): Array<{ session: SessionInfo; depth: number }> {
  const ids = new Set(sessions.map((session) => session.id));
  const children = new Map<string, SessionInfo[]>();
  const roots: SessionInfo[] = [];
  const sort = (items: SessionInfo[]): SessionInfo[] => items.sort((left, right) => lastActivity(right) - lastActivity(left));

  for (const session of sessions) {
    const parentId = effectiveParentId(session);
    if (!parentId || !ids.has(parentId)) {
      roots.push(session);
      continue;
    }
    const nested = children.get(parentId) ?? [];
    nested.push(session);
    children.set(parentId, nested);
  }
  children.forEach(sort);

  const rows: Array<{ session: SessionInfo; depth: number }> = [];
  const visited = new Set<string>();
  const append = (session: SessionInfo, depth: number): void => {
    if (visited.has(session.id)) return;
    visited.add(session.id);
    rows.push({ session, depth });
    for (const child of children.get(session.id) ?? []) append(child, depth + 1);
  };
  for (const root of sort(roots)) append(root, 0);
  // Corrupt or cyclic legacy parentage must remain discoverable rather than
  // disappearing from the aggregate inbox.
  for (const session of sort([...sessions])) append(session, 0);
  return rows;
}

interface Props {
  sessions: SessionInfo[];
  activeId: string | null;
  machine: string;
  onOpen: (id: string) => void;
  onOpenMachineSession: (serverId: string, sessionId: string) => void;
  onNew: () => void;
  onContinue: () => void;
  onResumeSession: (session: SessionInfo, destinationProvider?: 'claude' | 'codex') => void;
  onForkSession: (session: SessionInfo, destinationProvider: 'claude' | 'codex') => Promise<void>;
  onStartLinked: (sessionId: string) => void;
  openSessionIds: string[];
  onCloseView: (id: string) => void;
  onReparent: (id: string, parentId: string | null) => Promise<void>;
}

export function SessionNavigator({
  sessions,
  activeId,
  machine,
  onOpen,
  onOpenMachineSession,
  onNew,
  onContinue,
  onResumeSession,
  onForkSession,
  onStartLinked,
  openSessionIds,
  onCloseView,
  onReparent
}: Props): JSX.Element {
  const archiveSessions = useSessions((state) => state.archive);
  const endSession = useSessions((state) => state.kill);
  // The same store action the details panel's toggle calls. One write path, so
  // pinning from the row and pinning from the panel cannot drift.
  const updatePinned = useSessions((state) => state.updatePinned);
  const configuredMachines = useServers((state) => state.servers);
  const activeMachineId = useServers((state) => state.activeId);
  const selectMachine = useServers((state) => state.setActive);
  const [machineScope, setMachineScopeState] = useState<MachineScope>(() => readMachineScope(activeMachineId));
  const showingAllMachines = machineScope === ALL_MACHINES_SCOPE;
  const fleetSnapshots = useFleetSessions(configuredMachines, showingAllMachines);
  const [primary, setPrimary] = useState<PrimaryFilter>('all');
  const [provider, setProvider] = useState<ProviderFilter>('all');
  const [project, setProject] = useState('all');
  const [date, setDate] = useState<DateFilter>('all');
  const [query, setQuery] = useState('');
  const [expandedCompleted, setExpandedCompleted] = useState<Set<string>>(new Set());
  const [expandedHelpers, setExpandedHelpers] = useState<Set<string>>(new Set());
  const [draggingId, setDraggingId] = useState<string | null>(null);
  const [dropTargetId, setDropTargetId] = useState<string | null>(null);
  const [movingId, setMovingId] = useState<string | null>(null);
  const [moveError, setMoveError] = useState<string | null>(null);
  const [pinnedOpen, setPinnedOpen] = useState(true);
  const [runningOpen, setRunningOpen] = useState(true);
  const [endedOpen, setEndedOpen] = useState(false);
  const [showAllEnded, setShowAllEnded] = useState(false);
  const [selectingEnded, setSelectingEnded] = useState(false);
  const [selectedEnded, setSelectedEnded] = useState<Set<string>>(new Set());
  const [archiveBusy, setArchiveBusy] = useState(false);
  const [archiveError, setArchiveError] = useState<string | null>(null);
  const [collapsedTrees, setCollapsedTrees] = useState<Set<string>>(new Set());
  const [movePickerId, setMovePickerId] = useState<string | null>(null);
  const [actionMenuId, setActionMenuId] = useState<string | null>(null);
  const [actionMenuPosition, setActionMenuPosition] = useState<{ top: number; left: number } | null>(null);
  const [endingId, setEndingId] = useState<string | null>(null);
  const [endConfirmId, setEndConfirmId] = useState<string | null>(null);
  // A failed end belongs to the confirmation the user is looking at. It used
  // to be reported through `moveError`, which renders inside the session tree
  // — underneath this dialog's fixed, opaque scrim. The request failed, the
  // dialog stayed open, and the only explanation was invisible.
  const [endError, setEndError] = useState<string | null>(null);
  const [copyingId, setCopyingId] = useState<string | null>(null);
  const [pinningId, setPinningId] = useState<string | null>(null);
  const navigatorSessions = useMemo(() => collapseConversationRuntimes(sessions), [sessions]);

  const selectMachineScope = (scope: MachineScope): void => {
    setMachineScopeState(scope);
    writeMachineScope(scope);
    if (scope !== ALL_MACHINES_SCOPE) selectMachine(scope);
  };

  useEffect(() => {
    if (machineScope === ALL_MACHINES_SCOPE) return;
    const stillConfigured = configuredMachines.some((server) => server.id === machineScope);
    const next = stillConfigured && activeMachineId
      ? activeMachineId
      : activeMachineId ?? configuredMachines[0]?.id ?? ALL_MACHINES_SCOPE;
    if (next === machineScope) return;
    setMachineScopeState(next);
    writeMachineScope(next);
  }, [activeMachineId, configuredMachines, machineScope]);

  const copyConversation = async (
    session: SessionInfo,
    destinationProvider: 'claude' | 'codex'
  ): Promise<void> => {
    setCopyingId(session.id);
    setMoveError(null);
    setActionMenuId(null);
    try {
      await onForkSession(session, destinationProvider);
    } catch (error) {
      setMoveError(error instanceof Error ? error.message : String(error));
    } finally {
      setCopyingId(null);
    }
  };

  // Same shape as copyConversation and moveSession above: name the row that is
  // in flight, clear the last error, close the menu, and put a failure where the
  // tree already reports failures. It is deliberately NOT an optimistic write.
  // A pin is a promise that the machinery will keep its hands off this session;
  // painting it before the daemon stored it shows a protection the session does
  // not have, which is the reason `store/sessions.ts` commits the daemon's
  // answer rather than the requested value and the reason the details panel
  // says the same thing. The user-visible outcome of a refusal is identical to
  // an optimistic write that rolled back — nothing moved, and the reason is on
  // screen — without the window in which the app lies about a safety property.
  const togglePin = async (session: SessionInfo): Promise<void> => {
    if (pinningId) return;
    setPinningId(session.id);
    setMoveError(null);
    setActionMenuId(null);
    try {
      await updatePinned(session.id, !isPinned(session));
    } catch (error) {
      setMoveError(error instanceof Error ? error.message : 'Could not change the pin.');
    } finally {
      setPinningId(null);
    }
  };

  const sessionIds = useMemo(() => new Set(navigatorSessions.map((session) => session.id)), [navigatorSessions]);
  const endedSessionIds = useMemo(() => new Set(navigatorSessions.filter((session) => session.exited).map((session) => session.id)), [navigatorSessions]);
  useEffect(() => {
    setSelectedEnded((current) => {
      const next = new Set([...current].filter((id) => endedSessionIds.has(id)));
      return next.size === current.size ? current : next;
    });
  }, [endedSessionIds]);
  useEffect(() => {
    const searching = query.trim() !== '';
    if (searching || primary === 'needs' || primary === 'working') {
      setRunningOpen(true);
      setPinnedOpen(true);
    }
    if (searching || primary === 'ended') setEndedOpen(true);
  }, [primary, query]);
  useEffect(() => {
    if (!actionMenuId) return;
    const dismiss = (event: MouseEvent): void => {
      const target = event.target;
      const element = target instanceof Element
        ? target
        : target instanceof Node
          ? target.parentElement
          : null;
      if (element?.closest(`[data-session-actions="${actionMenuId}"]`)
        || element?.closest(`[data-session-action-menu="${actionMenuId}"]`)) return;
      setActionMenuId(null);
      setActionMenuPosition(null);
    };
    const dismissOnEscape = (event: KeyboardEvent): void => {
      if (event.key !== 'Escape' || event.defaultPrevented) return;
      const trigger = document.querySelector<HTMLElement>(`[data-session-actions="${actionMenuId}"] .session-row-action-trigger`);
      setActionMenuId(null);
      setActionMenuPosition(null);
      trigger?.focus();
    };
    document.addEventListener('click', dismiss);
    document.addEventListener('keydown', dismissOnEscape);
    return () => {
      document.removeEventListener('click', dismiss);
      document.removeEventListener('keydown', dismissOnEscape);
    };
  }, [actionMenuId]);
  const children = useMemo(() => {
    const byParent = new Map<string, SessionInfo[]>();
    for (const session of navigatorSessions) {
      const parentID = session.displayParentSessionId !== undefined
        ? session.displayParentSessionId
        : session.parentSessionId;
      if (!parentID || !sessionIds.has(parentID)) continue;
      const list = byParent.get(parentID) ?? [];
      list.push(session);
      byParent.set(parentID, list);
    }
    for (const list of byParent.values()) list.sort((a, b) => humanEngagementAt(b) - humanEngagementAt(a));
    return byParent;
  }, [navigatorSessions, sessionIds]);

  const subtreeHumanEngagement = useMemo(() => {
    const values = new Map<string, number>();
    const visiting = new Set<string>();
    const visit = (session: SessionInfo): number => {
      const cached = values.get(session.id);
      if (cached !== undefined) return cached;
      if (visiting.has(session.id)) return humanEngagementAt(session);
      visiting.add(session.id);
      const value = Math.max(
        humanEngagementAt(session),
        ...(children.get(session.id) ?? []).map(visit)
      );
      visiting.delete(session.id);
      values.set(session.id, value);
      return value;
    };
    for (const session of navigatorSessions) visit(session);
    return values;
  }, [children, navigatorSessions]);

  // The pins are the daemon's, not this client's: `sessions pin` and the
  // toggle in the details panel are the same fact, so a session pinned from the
  // CLI stays in focus here even when it has been set aside.
  const pins = useMemo(() => pinnedSessionIds(navigatorSessions), [navigatorSessions]);
  const grouped = useMemo(
    () => groupWorkingSet(navigatorSessions, openSessionIds, pins),
    [navigatorSessions, openSessionIds, pins]
  );
  // Engagement order, then pinned first. The pins now have their own section,
  // so `pinnedFirst` is a no-op over the live roots and a cheap guard: if a pin
  // ever failed to be lifted out, it still surfaces at the top rather than
  // hiding among a hundred and eighty rows.
  const sortRoots = (items: SessionInfo[]): SessionInfo[] => pinnedFirst([...items].sort((a, b) => {
    return (subtreeHumanEngagement.get(b.id) ?? humanEngagementAt(b))
      - (subtreeHumanEngagement.get(a.id) ?? humanEngagementAt(a));
  }));
  const liveIds = useMemo(
    () => new Set([...grouped.runningIds, ...grouped.setAsideIds]),
    [grouped.runningIds, grouped.setAsideIds]
  );
  const liveRoots = sortRoots([...grouped.runningRoots, ...grouped.setAsideRoots]
    .filter((session, index, items) => items.findIndex((item) => item.id === session.id) === index));
  const pinnedIds = grouped.pinnedIds;
  const pinnedRoots = sortRoots(grouped.pinnedRoots);

  const fleetSessions = useMemo(
    () => fleetSnapshots.flatMap((snapshot) => snapshot.sessions),
    [fleetSnapshots]
  );
  const scopedSessions = showingAllMachines ? fleetSessions : navigatorSessions;
  const projects = useMemo(() => [...new Set(scopedSessions.map(projectName).filter(Boolean))].sort(), [scopedSessions]);
  const counts = useMemo(() => ({
    needs: scopedSessions.filter(sessionNeedsYou).length,
    // Everything the working set is holding, pinned or not. This is the number
    // that answers "does this person have anything going on?", so it must not
    // drop when a session moves into the Pinned group: pinning your only live
    // session must not make the app behave as though that session ended.
    live: showingAllMachines
      ? scopedSessions.filter((session) => !session.exited).length
      : navigatorSessions.filter((session) => liveIds.has(session.id) || pinnedIds.has(session.id)).length,
    // The Live group's own rows. The pinned ones are counted under Pinned.
    liveGroup: navigatorSessions.filter((session) => liveIds.has(session.id)).length,
    working: scopedSessions.filter((session) => session.working && !session.exited).length
  }), [liveIds, navigatorSessions, pinnedIds, scopedSessions, showingAllMachines]);

  const matches = (session: SessionInfo): boolean => {
    if (primary === 'needs' && !sessionNeedsYou(session)) return false;
    if (primary === 'working' && (!session.working || session.exited)) return false;
    if (primary === 'ended' && !session.exited) return false;
    const normalized = normalizeProvider(session.tool);
    if (provider !== 'all' && (provider === 'shell' ? session.tool !== 'terminal' : normalized !== provider)) return false;
    if (project !== 'all' && projectName(session) !== project) return false;
    const age = Date.now() - lastActivity(session);
    if (date === 'today' && age > 86_400_000) return false;
    if (date === 'week' && age > 7 * 86_400_000) return false;
    const needle = query.trim().toLowerCase();
    if (needle) {
      const haystack = `${getTabLabel(session.id) ?? sessionLabel(session)} ${session.cwd} ${session.lastSummary ?? ''} ${Object.values(session.tags ?? {}).join(' ')}`.toLowerCase();
      if (!haystack.includes(needle)) return false;
    }
    return true;
  };

  const treeMatches = (session: SessionInfo, groupIds: Set<string>): boolean => matches(session)
    || (children.get(session.id) ?? []).filter((child) => groupIds.has(child.id)).some((child) => treeMatches(child, groupIds));
  const hasLiveDescendant = (session: SessionInfo, groupIds: Set<string>): boolean => (children.get(session.id) ?? [])
    .filter((child) => groupIds.has(child.id))
    .some((child) => !child.exited || hasLiveDescendant(child, groupIds));
  const filteredLiveRoots = liveRoots.filter((root) => treeMatches(root, liveIds));
  const filteredPinnedRoots = pinnedRoots.filter((root) => treeMatches(root, pinnedIds));
  const filteredEnded = grouped.ended
    .filter(matches)
    .sort((left, right) => lastActivity(right) - lastActivity(left));
  const recentCutoff = Date.now() - RECENTLY_ENDED_DAYS * 86_400_000;
  const recentEnded = filteredEnded
    .filter((session) => lastActivity(session) >= recentCutoff)
    .slice(0, RECENTLY_ENDED_LIMIT);
  const browsingAllEnded = showAllEnded || primary === 'ended' || query.trim() !== '';
  const visibleEnded = browsingAllEnded ? filteredEnded : recentEnded;
  const hasOlderEnded = filteredEnded.length > recentEnded.length;
  const filteredFleetEnded = fleetSessions
    .filter((session) => session.exited && matches(session))
    .sort((left, right) => lastActivity(right) - lastActivity(left));
  const filteredFleetLiveCount = fleetSessions.filter((session) => !session.exited && matches(session)).length;
  const recentFleetEnded = filteredFleetEnded
    .filter((session) => lastActivity(session) >= recentCutoff)
    .slice(0, RECENTLY_ENDED_LIMIT);
  const visibleFleetEnded = browsingAllEnded ? filteredFleetEnded : recentFleetEnded;
  const visibleFleetEndedRows = new Set(visibleFleetEnded);
  const hasOlderFleetEnded = filteredFleetEnded.length > recentFleetEnded.length;
  const scopedEndedCount = showingAllMachines ? filteredFleetEnded.length : filteredEnded.length;
  useEffect(() => {
    if (counts.live === 0 && scopedEndedCount > 0) setEndedOpen(true);
  }, [counts.live, scopedEndedCount]);
  const toggleEndedSelection = (id: string): void => setSelectedEnded((current) => {
    const next = new Set(current);
    if (next.has(id)) next.delete(id); else next.add(id);
    return next;
  });
  const archiveEnded = async (ids: string[]): Promise<void> => {
    if (ids.length === 0) return;
    setArchiveBusy(true);
    setArchiveError(null);
    try {
      const result = await archiveSessions(ids);
      const skipped = result.items.filter((item) => item.status === 'skipped');
      if (skipped.length > 0) {
        setArchiveError(skipped.map((item) => `${item.id.slice(0, 8)}: ${item.reason ?? 'not archived'}`).join(' · '));
      }
      setSelectedEnded(new Set());
      setSelectingEnded(false);
    } catch (error) {
      setArchiveError(error instanceof Error ? error.message : 'Could not archive the selected sessions.');
    } finally {
      setArchiveBusy(false);
    }
  };
  const archiveSelected = async (): Promise<void> => archiveEnded([...selectedEnded]);
  const requestEndSession = (session: SessionInfo): void => {
    if (session.exited || endingId) return;
    setActionMenuId(null);
    setEndError(null);
    setEndConfirmId(session.id);
  };
  const dismissEndConfirm = (): void => {
    if (endingId) return;
    setEndConfirmId(null);
    setEndError(null);
  };
  const confirmEndSession = async (): Promise<void> => {
    if (!endConfirmId || endingId) return;
    setEndingId(endConfirmId);
    setMoveError(null);
    setEndError(null);
    try {
      await endSession(endConfirmId);
      setEndConfirmId(null);
    } catch (error) {
      // Keep the decision open and say why. docs/PRINCIPLES.md: cleanup must
      // never hide an unresolved decision — closing here would tell the user
      // a runtime had stopped when it is still running.
      setEndError(error instanceof Error ? error.message : 'Could not end the session.');
    } finally {
      setEndingId(null);
    }
  };
  const toggleCompleted = (id: string): void => setExpandedCompleted((current) => {
    const next = new Set(current);
    if (next.has(id)) next.delete(id); else next.add(id);
    return next;
  });
  const toggleHelpers = (id: string): void => setExpandedHelpers((current) => {
    const next = new Set(current);
    if (next.has(id)) next.delete(id); else next.add(id);
    return next;
  });
  const toggleTree = (id: string): void => setCollapsedTrees((current) => {
    const next = new Set(current);
    if (next.has(id)) next.delete(id); else next.add(id);
    return next;
  });
  const wouldCreateCycle = (id: string, parentID: string): boolean => {
    const seen = new Set<string>();
    let current = parentID;
    while (current) {
      if (current === id) return true;
      if (seen.has(current)) return true;
      seen.add(current);
      const session = sessions.find((candidate) => candidate.id === current);
      if (!session) return false;
      current = session.displayParentSessionId !== undefined
        ? session.displayParentSessionId
        : (session.parentSessionId ?? '');
    }
    return false;
  };
  const canMove = (id: string | null, parentID: string | null): id is string => (
    !!id && id !== parentID && (parentID === null || !wouldCreateCycle(id, parentID))
  );
  const moveSession = async (id: string, parentID: string | null): Promise<void> => {
    if (!canMove(id, parentID)) return;
    setMovingId(id);
    setMoveError(null);
    try {
      await onReparent(id, parentID);
    } catch (error) {
      setMoveError(error instanceof Error ? error.message : 'Could not move the session.');
    } finally {
      setMovingId(null);
      setMovePickerId(null);
      setActionMenuId(null);
      setDraggingId(null);
      setDropTargetId(null);
    }
  };
  const startDragging = (event: DragEvent<HTMLElement>, id: string): void => {
    event.stopPropagation();
    event.dataTransfer.effectAllowed = 'move';
    event.dataTransfer.setData('text/x-sessions-session-id', id);
    event.dataTransfer.setData('text/plain', id);
    setDraggingId(id);
    setMoveError(null);
  };
  const dragOverTarget = (event: DragEvent<HTMLElement>, parentID: string | null): void => {
    if (!canMove(draggingId, parentID)) return;
    event.preventDefault();
    event.dataTransfer.dropEffect = 'move';
    setDropTargetId(parentID ?? '__root__');
  };
  const dropOnTarget = (event: DragEvent<HTMLElement>, parentID: string | null): void => {
    event.preventDefault();
    event.stopPropagation();
    const id = draggingId
      || event.dataTransfer.getData('text/x-sessions-session-id')
      || event.dataTransfer.getData('text/plain');
    if (canMove(id, parentID)) void moveSession(id, parentID);
  };

  const descendantRollup = (session: SessionInfo, groupIds: Set<string>): {
    working: number;
    needs: number;
    finished: number;
  } => {
    const seen = new Set<string>();
    const descendants: SessionInfo[] = [];
    const collect = (parent: SessionInfo): void => {
      for (const child of children.get(parent.id) ?? []) {
        if (!groupIds.has(child.id) || seen.has(child.id)) continue;
        seen.add(child.id);
        descendants.push(child);
        collect(child);
      }
    };
    collect(session);
    return {
      working: descendants.filter((child) => !child.exited && child.working).length,
      needs: descendants.filter(sessionNeedsYou).length,
      finished: descendants.filter(sessionIsFinished).length
    };
  };

  const renderNode = (
    session: SessionInfo,
    depth: number,
    endedFlat = false,
    groupIds: Set<string> = liveIds
  ): JSX.Element | null => {
    if (endedFlat ? !matches(session) : !treeMatches(session, groupIds)) return null;
    const nested = endedFlat ? [] : (children.get(session.id) ?? []).filter((child) => groupIds.has(child.id));
    // Never collapse the only path to live work. A finished intermediary can
    // still own a running grandchild, so it remains visible until its entire
    // subtree is finished.
    const agentChildren = nested.filter(isAgentLedChild);
    const userChildren = nested.filter((child) => !isAgentLedChild(child));
    const helpersExpanded = expandedHelpers.has(session.id);
    const urgentAgentChildren = agentChildren.filter((child) => (
      child.id === activeId
      || sessionWantsAttention(child)
    ));
    const visibleAgentIDs = new Set((helpersExpanded ? agentChildren : urgentAgentChildren).map((child) => child.id));
    const completed = userChildren.filter((child) => sessionIsFinished(child) && !hasLiveDescendant(child, groupIds));
    const completedIds = new Set(completed.map((child) => child.id));
    const visible = nested.filter((child) => (
      isAgentLedChild(child)
        ? visibleAgentIDs.has(child.id)
        : !completedIds.has(child.id) || expandedCompleted.has(session.id)
    ));
    const helperRollup = {
      working: agentChildren.filter((child) => !child.exited && child.working).length,
      needs: agentChildren.filter(sessionNeedsYou).length,
      finished: agentChildren.filter(sessionIsFinished).length
    };
    const providerName = normalizeProvider(session.tool);
    const status = classifySession(session);
    const end = session.exited ? endedSummary(session, sessions) : null;
    const currentParentID = effectiveParentId(session);
    const parent = currentParentID ? sessions.find((candidate) => candidate.id === currentParentID) : null;
    const resumedFrom = session.resumedFrom ? sessions.find((candidate) => candidate.id === session.resumedFrom) : null;
    const hasChildren = visible.length > 0 || completed.length > 0 || agentChildren.length > 0;
    const collapsed = collapsedTrees.has(session.id);
    const label = getTabLabel(session.id) ?? sessionLabel(session);
    const rollup = depth === 0 && !endedFlat ? descendantRollup(session, groupIds) : null;
    const rollupParts = rollup
      ? [
        rollup.working ? `${rollup.working} working` : '',
        rollup.needs ? `${rollup.needs} need you` : '',
        rollup.finished ? `${rollup.finished} finished` : ''
      ].filter(Boolean)
      : [];
    const helperParts = [
      helperRollup.working ? `${helperRollup.working} working` : '',
      helperRollup.needs ? `${helperRollup.needs} need you` : '',
      helperRollup.finished ? `${helperRollup.finished} finished` : ''
    ].filter(Boolean);
    return (
      <div className="session-tree-node" key={session.id}>
        <div
          role="treeitem"
          tabIndex={0}
          aria-expanded={hasChildren ? !collapsed : undefined}
          className={`session-nav-row ${status.className}${session.id === activeId ? ' is-active' : ''}${draggingId === session.id ? ' is-dragging' : ''}${dropTargetId === session.id ? ' is-drop-target' : ''}`}
          data-session-id={session.id}
          style={{ '--tree-depth': depth } as React.CSSProperties}
          onClick={() => selectingEnded && session.exited ? toggleEndedSelection(session.id) : onOpen(session.id)}
          onKeyDown={(event) => {
            if (event.target !== event.currentTarget || (event.key !== 'Enter' && event.key !== ' ')) return;
            event.preventDefault();
            if (selectingEnded && session.exited) toggleEndedSelection(session.id); else onOpen(session.id);
          }}
          onDragOver={(event) => dragOverTarget(event, session.id)}
          onDragLeave={() => setDropTargetId((current) => current === session.id ? null : current)}
          onDrop={(event) => dropOnTarget(event, session.id)}
          title={selectingEnded && session.exited ? 'Select this ended session' : 'Open session'}
        >
          <button
            type="button"
            className="session-drag-handle"
            draggable={movingId !== session.id}
            aria-label={`Move ${label}`}
            aria-grabbed={draggingId === session.id}
            title="Drag to place under another session"
            onClick={(event) => event.stopPropagation()}
            onDragStart={(event) => startDragging(event, session.id)}
            onDragEnd={() => { setDraggingId(null); setDropTargetId(null); }}
          >⠿</button>
          {hasChildren ? (
            <button type="button" className="session-tree-toggle" aria-label={`${collapsed ? 'Expand' : 'Collapse'} ${label}`} onClick={(event) => { event.stopPropagation(); toggleTree(session.id); }}>
              <DisclosureChevron open={!collapsed} />
            </button>
          ) : <span className="session-tree-toggle-placeholder" aria-hidden />}
          <span className="session-nav-branch" aria-hidden>{depth > 0 ? '└' : ''}</span>
          <span className={`session-nav-status ${status.className}`} aria-hidden title={status.label} />
          {isPinned(session) ? <span className="manager-pin is-pinned" title="Pinned" aria-label="Pinned">📌</span> : null}
          <span className="session-nav-copy">
            <span className="session-nav-title">{label}</span>
            {end ? <span className={`session-nav-ended is-${end.tone}`}>{end.label}</span> : null}
            {resumedFrom ? <span className="session-nav-parent">Resumed from {getTabLabel(resumedFrom.id) ?? sessionLabel(resumedFrom)}</span> : null}
            {endedFlat && parent ? <span className="session-nav-parent">Under {getTabLabel(parent.id) ?? sessionLabel(parent)}</span> : null}
            {rollupParts.length > 0 ? <span className="session-nav-rollup">{rollupParts.join(' · ')}</span> : null}
            <span className="session-nav-meta">
              {providerName
                ? <span className="session-nav-provider" title={providerName === 'claude' ? 'Claude' : 'Codex'}><ProviderMark provider={providerName} size={20} /></span>
                : <span className="session-nav-provider is-shell" title="Shell">⌘</span>}
              <MachineMark machine={machine} size={17} />
              <span>{session.exited ? endedAtLabel(session) : relativeTime(lastActivity(session))}</span>
            </span>
          </span>
          {selectingEnded && session.exited ? <span className={`session-row-check${selectedEnded.has(session.id) ? ' is-selected' : ''}`} aria-hidden>{selectedEnded.has(session.id) ? '✓' : ''}</span> : null}
          {!selectingEnded && end && canContinueSession(session) ? (
            <button
              type="button"
              className="session-row-continue"
              aria-label={`Resume ${label} in a new runtime`}
              title="Resume this conversation in a new runtime"
              onClick={(event) => { event.stopPropagation(); onResumeSession(session); }}
            >Resume <span aria-hidden>→</span></button>
          ) : null}
          {!selectingEnded ? (
            <div
              className="session-row-actions"
              data-session-actions={session.id}
              onClick={(event) => event.stopPropagation()}
            >
              <button
                type="button"
                className="session-row-action-trigger"
                aria-label={`Actions for ${label}`}
                aria-haspopup="menu"
                aria-expanded={actionMenuId === session.id}
                title="Session actions"
                onClick={(event) => {
                  if (actionMenuId === session.id) {
                    setActionMenuId(null);
                    setActionMenuPosition(null);
                    return;
                  }
                  const rect = event.currentTarget.getBoundingClientRect();
                  const width = 224;
                  // Grew by one row when Pin/Unpin joined the menu; this only
                  // decides whether the popover is flipped above the trigger.
                  const estimatedHeight = 326;
                  setActionMenuPosition({
                    top: Math.max(8, Math.min(rect.bottom + 4, window.innerHeight - estimatedHeight - 8)),
                    left: Math.max(8, Math.min(rect.right - width, window.innerWidth - width - 8))
                  });
                  setActionMenuId(session.id);
                }}
              >•••</button>
              {actionMenuId === session.id && actionMenuPosition ? createPortal((
              <div
                className="session-row-action-menu"
                data-session-action-menu={session.id}
                role="menu"
                style={{
                  '--session-menu-top': `${actionMenuPosition.top}px`,
                  '--session-menu-left': `${actionMenuPosition.left}px`
                } as React.CSSProperties}
                onClick={(event) => event.stopPropagation()}
              >
                <button type="button" role="menuitem" onClick={() => { setActionMenuId(null); onOpen(session.id); }}>{session.exited ? 'View history' : 'Open in tab'}</button>
                {openSessionIds.includes(session.id) ? <button type="button" role="menuitem" onClick={() => { setActionMenuId(null); onCloseView(session.id); }}>Close tab <small>keeps running</small></button> : null}
                {/*
                  * Pin / Unpin — named for the state it moves to, like every
                  * other verb in this menu. It sits high because this is where
                  * a person reaches to organize a row, and being absent here is
                  * what made a shipped feature unreachable.
                  *
                  * On an ended session the item is shown DISABLED rather than
                  * hidden. The daemon refuses it with 409 in both directions
                  * (`UpdatePinned` checks `Exited` before reading the value),
                  * so hiding it would be honest about the API and dishonest
                  * about the product: an absent item reads as "this app cannot
                  * pin", which is exactly the report that started this. Disabled
                  * with the daemon's own reason names the verb that does work,
                  * and Archive is two items below. AGENTS.md #4: explain the
                  * failed operation and the safe next action.
                  */}
                <button
                  type="button"
                  role="menuitem"
                  className="session-action-pin"
                  disabled={session.exited || pinningId !== null}
                  title={session.exited ? PIN_UNAVAILABLE_WHEN_ENDED : undefined}
                  onClick={() => void togglePin(session)}
                >
                  {pinningId === session.id
                    ? 'Saving…'
                    : isPinned(session) ? 'Unpin' : 'Pin'}
                  {session.exited ? <small>ended · archive instead</small> : null}
                </button>
                {end && canContinueSession(session) ? <button type="button" role="menuitem" onClick={() => { setActionMenuId(null); onResumeSession(session); }}>Resume…</button> : null}
                {providerName ? (
                  <details className="session-action-submenu">
                    <summary>Fork <small>original stays here</small></summary>
                    <div>
                      <button type="button" role="menuitem" disabled={copyingId !== null} onClick={() => void copyConversation(session, 'claude')}>
                        {copyingId === session.id ? 'Creating copy…' : 'In Claude'}
                      </button>
                      <button type="button" role="menuitem" disabled={copyingId !== null} onClick={() => void copyConversation(session, 'codex')}>
                        {copyingId === session.id ? 'Creating copy…' : 'In Codex'}
                      </button>
                    </div>
                  </details>
                ) : null}
                {session.reopenedAs ? <button type="button" role="menuitem" onClick={() => { setActionMenuId(null); onOpen(session.reopenedAs!); }}>Open resumed runtime</button> : null}
                {session.resumedFrom ? <button type="button" role="menuitem" onClick={() => { setActionMenuId(null); onOpen(session.resumedFrom!); }}>View previous runtime</button> : null}
                <button type="button" role="menuitem" onClick={() => { setActionMenuId(null); onStartLinked(session.id); }}>Start linked session…</button>
                <details className="session-action-submenu">
                  <summary>Move</summary>
                  <div>
                    <button type="button" role="menuitem" onClick={() => { setActionMenuId(null); setMovePickerId(session.id); }}>Under another session…</button>
                    {currentParentID ? <button type="button" role="menuitem" onClick={() => void moveSession(session.id, null)}>Make top-level</button> : null}
                    <ContinueElsewhereButton sessionId={session.id} label={label} appearance="menuitem" onOpen={() => setActionMenuId(null)} />
                  </div>
                </details>
                {session.exited ? (
                  <>
                    <button type="button" role="menuitem" onClick={() => { setSelectingEnded(true); setSelectedEnded(new Set([session.id])); setEndedOpen(true); setActionMenuId(null); }}>Select</button>
                    <button type="button" role="menuitem" onClick={() => { setActionMenuId(null); void archiveEnded([session.id]); }}>Archive from list</button>
                  </>
                ) : (
                  <button type="button" role="menuitem" disabled={endingId !== null} onClick={() => requestEndSession(session)}>{endingId === session.id ? 'Ending…' : 'End session…'}</button>
                )}
              </div>
              ), document.body) : null}
            </div>
          ) : null}
        </div>
        {!collapsed ? visible.map((child) => renderNode(child, depth + 1, false, groupIds)) : null}
        {!collapsed && agentChildren.length > 0 ? (
          <button
            type="button"
            className="session-helper-summary"
            style={{ '--tree-depth': depth + 1 } as React.CSSProperties}
            onClick={() => toggleHelpers(session.id)}
          >
            <span>{helpersExpanded ? 'Hide helpers' : 'Helpers'}</span>
            <small>{helperParts.length > 0 ? helperParts.join(' · ') : `${agentChildren.length} quiet`}</small>
          </button>
        ) : null}
        {!collapsed && completed.length > 0 ? (
          <button type="button" className="completed-children" style={{ '--tree-depth': depth + 1 } as React.CSSProperties} onClick={() => toggleCompleted(session.id)}>
            {expandedCompleted.has(session.id) ? 'Hide completed' : `${completed.length} completed`}
          </button>
        ) : null}
      </div>
    );
  };

  const renderFleetMachineGroup = (
    snapshot: FleetSessionSnapshot,
    ended: boolean
  ): JSX.Element | null => {
    const eligible = snapshot.sessions.filter((session) => (
      session.exited === ended
      && matches(session)
      && (!ended || visibleFleetEndedRows.has(session))
    ));
    if (eligible.length === 0 && !snapshot.loading && !snapshot.error) return null;
    const machineName = serverDisplayName(snapshot.server, true);
    return (
      <section className={`session-fleet-machine${snapshot.error ? ' is-unreachable' : ''}`} key={`${ended ? 'ended' : 'live'}:${snapshot.server.id}`}>
        <header>
          <span><MachineMark machine={machineName} size={16} /><strong>{machineName}</strong></span>
          <small>{eligible.length}</small>
        </header>
        {orderedMachineRows(eligible).map(({ session, depth }) => {
          const status = classifySession(session);
          const providerName = normalizeProvider(session.tool);
          return (
            <button
              type="button"
              className="session-fleet-row"
              key={`${snapshot.server.id}:${session.id}`}
              style={{ '--tree-depth': Math.min(depth, 5) } as React.CSSProperties}
              onClick={() => onOpenMachineSession(snapshot.server.id, session.id)}
              title={`Open on ${machineName}`}
            >
              <span className="session-fleet-provider">
                {providerName ? <ProviderMark provider={providerName} size={18} /> : <span aria-label="Shell">⌘</span>}
              </span>
              <span className="session-fleet-copy">
                <strong>{getTabLabel(session.id) ?? sessionLabel(session)}</strong>
                <small>{projectName(session)}</small>
              </span>
              <span className={`session-fleet-state ${status.className}`}><i aria-hidden />{status.label}</span>
              <time>{relativeTime(lastActivity(session))}</time>
            </button>
          );
        })}
        {snapshot.loading ? <div className="session-fleet-machine-note">Checking sessions…</div> : null}
        {snapshot.error ? <div className="session-fleet-machine-note is-error">Can’t reach this computer right now.</div> : null}
      </section>
    );
  };
  const movePickerSession = movePickerId ? sessions.find((session) => session.id === movePickerId) ?? null : null;
  const endConfirmSession = endConfirmId ? sessions.find((session) => session.id === endConfirmId) ?? null : null;
  const movePickerParentID = movePickerSession ? effectiveParentId(movePickerSession) : null;
  const moveCandidates = movePickerSession
    ? sessions
      .filter((candidate) => canMove(movePickerSession.id, candidate.id))
      .sort((left, right) => Number(left.exited) - Number(right.exited) || lastActivity(right) - lastActivity(left))
    : [];

  return (
    <aside className="session-navigator">
      <header className="session-navigator-head">
        <div><span>Operations inbox</span><strong>Sessions</strong></div>
        <div className="session-navigator-actions">
          <button type="button" className="session-continue-action" onClick={onContinue}>Resume</button>
          <button type="button" className="session-new-action" onClick={onNew} aria-label="New session"><span aria-hidden>＋</span> New</button>
        </div>
      </header>
      <div className="session-machine-filter" role="toolbar" aria-label="Connected computers">
        <button
          type="button"
          className={showingAllMachines ? 'is-active' : undefined}
          aria-pressed={showingAllMachines}
          title="Show sessions from every connected computer"
          onClick={() => selectMachineScope(ALL_MACHINES_SCOPE)}
        >
          <span className="session-all-machines-mark" aria-hidden><i /><i /><i /></span>
          <span>All machines</span>
        </button>
        {configuredMachines.map((configured) => (
          <button
            type="button"
            key={configured.id}
            className={configured.id === machineScope ? 'is-active' : undefined}
            aria-pressed={configured.id === machineScope}
            title={`Show sessions on ${serverDisplayName(configured, true)}`}
            onClick={() => selectMachineScope(configured.id)}
          >
            <MachineMark machine={serverDisplayName(configured, true)} size={16} />
            <span>{serverDisplayName(configured, true)}</span>
          </button>
        ))}
      </div>
      <div className="session-nav-search"><span aria-hidden>⌕</span><input value={query} onChange={(event) => setQuery(event.currentTarget.value)} placeholder="Filter sessions" /></div>
      <div className="session-filter-row" role="toolbar" aria-label="Session status filters">
        <FilterButton label="All" active={primary === 'all'} onClick={() => setPrimary('all')} />
        <FilterButton label={`Needs you${counts.needs ? ` ${counts.needs}` : ''}`} active={primary === 'needs'} onClick={() => setPrimary('needs')} />
        <FilterButton label={`Working${counts.working ? ` ${counts.working}` : ''}`} active={primary === 'working'} onClick={() => setPrimary('working')} />
        <FilterButton label="Ended" active={primary === 'ended'} onClick={() => setPrimary('ended')} />
        <details className="session-more-filters">
          <summary aria-label="More filters">⋯</summary>
          <div className="session-filter-popover">
            <label>Provider<select value={provider} onChange={(event) => setProvider(event.currentTarget.value as ProviderFilter)}><option value="all">All providers</option><option value="claude">Claude</option><option value="codex">Codex</option><option value="shell">Shell</option></select></label>
            <label>Computer<select value={machineScope} onChange={(event) => selectMachineScope(event.currentTarget.value)}><option value={ALL_MACHINES_SCOPE}>All machines</option>{configuredMachines.map((configured) => <option key={configured.id} value={configured.id}>{serverDisplayName(configured, true)}</option>)}</select></label>
            <label>Project<select value={project} onChange={(event) => setProject(event.currentTarget.value)}><option value="all">All projects</option>{projects.map((item) => <option key={item}>{item}</option>)}</select></label>
            <label>Date<select value={date} onChange={(event) => setDate(event.currentTarget.value as DateFilter)}><option value="all">Any time</option><option value="today">Today</option><option value="week">Past 7 days</option></select></label>
          </div>
        </details>
      </div>
      <div className="session-tree" role="tree">
        {moveError ? <div className="session-move-error" role="alert">{moveError}</div> : null}
        {archiveError ? <div className="session-move-error" role="alert">{archiveError}</div> : null}
        {!showingAllMachines && selectingEnded ? (
          <div className="session-bulk-actions">
            <strong>{selectedEnded.size} selected</strong>
            <button type="button" disabled={archiveBusy || selectedEnded.size === 0} onClick={() => void archiveSelected()}>{archiveBusy ? 'Archiving…' : 'Archive'}</button>
            <button type="button" onClick={() => { setSelectingEnded(false); setSelectedEnded(new Set()); }}>Cancel</button>
          </div>
        ) : null}
        {!showingAllMachines && draggingId ? (
          <div
            className={`session-root-drop${dropTargetId === '__root__' ? ' is-drop-target' : ''}`}
            onDragOver={(event) => dragOverTarget(event, null)}
            onDragLeave={() => setDropTargetId((current) => current === '__root__' ? null : current)}
            onDrop={(event) => dropOnTarget(event, null)}
          >
            Drop here to make this a manager session
          </div>
        ) : null}
        {showingAllMachines && primary !== 'ended' ? <div className="session-tree-group session-fleet-scope-group">
          <button type="button" className="session-tree-group-head" onClick={() => setRunningOpen((current) => !current)}>
            <span className="session-group-disclosure"><DisclosureChevron open={runningOpen} /> Live across your fleet</span><strong>{counts.live}</strong>
          </button>
          {runningOpen ? (
            <>
              {fleetSnapshots.map((snapshot) => renderFleetMachineGroup(snapshot, false))}
              {filteredFleetLiveCount === 0 && fleetSnapshots.every((snapshot) => !snapshot.loading && !snapshot.error)
                ? <div className="session-tree-empty is-compact">No matching live sessions.</div>
                : null}
            </>
          ) : null}
        </div> : null}
        {showingAllMachines && primary !== 'working' && primary !== 'needs' ? <div className="session-tree-group session-fleet-scope-group">
          <button type="button" className="session-tree-group-head" onClick={() => setEndedOpen((current) => !current)}>
            <span className="session-group-disclosure"><DisclosureChevron open={endedOpen} /> Ended across your fleet</span><strong>{filteredFleetEnded.length}</strong>
          </button>
          {endedOpen ? (
            <>
              {fleetSnapshots.map((snapshot) => renderFleetMachineGroup(snapshot, true))}
              {!browsingAllEnded && hasOlderFleetEnded ? <button type="button" className="session-all-ended" onClick={() => setShowAllEnded(true)}>All ended sessions →</button> : null}
              {showAllEnded && primary !== 'ended' && query.trim() === '' ? <button type="button" className="session-all-ended" onClick={() => setShowAllEnded(false)}>Show recent only</button> : null}
              {filteredFleetEnded.length === 0 && fleetSnapshots.every((snapshot) => !snapshot.loading && !snapshot.error)
                ? <div className="session-tree-empty is-compact">No matching ended sessions.</div>
                : null}
            </>
          ) : null}
        </div> : null}
        {/*
          * The user's own section, above everything a classifier decided. It
          * appears only when there are pins: an empty "Pinned" header would be
          * a permanent piece of furniture teaching nothing, and the row menu is
          * where the feature is discovered.
          *
          * The count is the number of pins, not the number of rows — a pinned
          * manager brings its children into the section, and "Pinned 6" for one
          * marked session would be counting something the user did not mark.
          */}
        {!showingAllMachines && filteredPinnedRoots.length > 0 ? <div className="session-tree-group is-pinned">
          <button type="button" className="session-tree-group-head" onClick={() => setPinnedOpen((current) => !current)}>
            <span className="session-group-disclosure"><DisclosureChevron open={pinnedOpen} /> Pinned</span><strong>{pins.length}</strong>
          </button>
          {pinnedOpen ? filteredPinnedRoots.map((root) => renderNode(root, 0, false, pinnedIds)) : null}
        </div> : null}
        {!showingAllMachines && primary !== 'ended' ? <div className="session-tree-group">
          <button type="button" className="session-tree-group-head" onClick={() => setRunningOpen((current) => !current)}>
            <span className="session-group-disclosure"><DisclosureChevron open={runningOpen} /> Live</span><strong>{counts.liveGroup}</strong>
          </button>
          {runningOpen ? (
            <>
              {filteredLiveRoots.map((root) => renderNode(root, 0, false, liveIds))}
              {filteredLiveRoots.length === 0 ? <div className="session-tree-empty is-compact">No matching live sessions.</div> : null}
            </>
          ) : null}
        </div> : null}
        {!showingAllMachines && primary !== 'working' && primary !== 'needs' ? <div className="session-tree-group">
          <div className="session-tree-group-head">
            <button type="button" onClick={() => setEndedOpen((current) => !current)}><span className="session-group-disclosure"><DisclosureChevron open={endedOpen} /> Ended</span><strong>{visibleEnded.length}</strong></button>
            {filteredEnded.length > 0 ? <button type="button" className="session-select-ended" onClick={() => { setSelectingEnded((current) => !current); setEndedOpen(true); }}>Select</button> : null}
          </div>
          {endedOpen ? (
            <>
              {visibleEnded.map((session) => renderNode(session, 0, true))}
              {!browsingAllEnded && hasOlderEnded ? <button type="button" className="session-all-ended" onClick={() => setShowAllEnded(true)}>All ended sessions →</button> : null}
              {showAllEnded && primary !== 'ended' && query.trim() === '' ? <button type="button" className="session-all-ended" onClick={() => setShowAllEnded(false)}>Show recent only</button> : null}
              {visibleEnded.length === 0 ? <div className="session-tree-empty is-compact">No ended sessions yet.</div> : null}
            </>
          ) : null}
        </div> : null}
        {!showingAllMachines && sessions.length === 0 ? <div className="session-tree-empty">No sessions on {machine}.</div> : null}
        {showingAllMachines && configuredMachines.length === 0 ? <div className="session-tree-empty">Connect a computer to see its sessions here.</div> : null}
      </div>
      {movePickerSession ? (
        <div className="session-move-sheet" role="presentation" onMouseDown={(event) => { if (event.target === event.currentTarget) setMovePickerId(null); }}>
          <section role="dialog" aria-modal="true" aria-labelledby="move-session-title">
            <header><div><span>Organize session</span><h2 id="move-session-title">Move “{getTabLabel(movePickerSession.id) ?? sessionLabel(movePickerSession)}”</h2></div><button type="button" aria-label="Close move dialog" onClick={() => setMovePickerId(null)}>×</button></header>
            <p>Choose a manager. This changes visual grouping only; trusted creator history stays unchanged.</p>
            <div className="session-move-options">
              {movePickerParentID ? <button type="button" className="is-root" disabled={!canMove(movePickerSession.id, null)} onClick={() => void moveSession(movePickerSession.id, null)}><strong>Make top-level</strong><small>Show as its own manager session</small></button> : null}
              {moveCandidates.map((candidate) => (
                <button type="button" key={candidate.id} onClick={() => void moveSession(movePickerSession.id, candidate.id)}>
                  <strong>{getTabLabel(candidate.id) ?? sessionLabel(candidate)}</strong>
                  <small>{candidate.exited ? 'Ended session' : candidate.working ? 'Working' : 'Live · idle'} · {projectName(candidate)}</small>
                </button>
              ))}
            </div>
            {moveCandidates.length === 0 ? <div className="session-tree-empty">No eligible manager sessions.</div> : null}
          </section>
        </div>
      ) : null}
      {endConfirmSession ? (
        <div className="session-move-sheet session-end-sheet" role="presentation" onMouseDown={(event) => { if (event.target === event.currentTarget) dismissEndConfirm(); }}>
          <section role="dialog" aria-modal="true" aria-labelledby="end-session-title">
            <header>
              <div><span>End session</span><h2 id="end-session-title">Stop “{getTabLabel(endConfirmSession.id) ?? sessionLabel(endConfirmSession)}”?</h2></div>
              <button type="button" aria-label="Cancel ending session" disabled={endingId !== null} onClick={dismissEndConfirm}>×</button>
            </header>
            <p>This stops the agent on {machine}. Its conversation stays in Ended, where you can resume it later.</p>
            {endError ? (
              <div className="session-move-error session-end-error" role="alert">
                <strong>This session is still running.</strong> {endError}
              </div>
            ) : null}
            <div className="session-end-actions">
              <button type="button" disabled={endingId !== null} onClick={dismissEndConfirm}>Keep running</button>
              <button type="button" className="btn btn-primary" disabled={endingId !== null} onClick={() => void confirmEndSession()}>{endingId ? 'Ending…' : endError ? 'Try again' : 'End session'}</button>
            </div>
          </section>
        </div>
      ) : null}
    </aside>
  );
}

function FilterButton({ label, active, onClick }: { label: string; active: boolean; onClick: () => void }): JSX.Element {
  return <button type="button" className={active ? 'is-active' : ''} onClick={onClick}>{label}</button>;
}

function DisclosureChevron({ open }: { open: boolean }): JSX.Element {
  return (
    <svg className={`session-disclosure-chevron${open ? ' is-open' : ''}`} viewBox="0 0 20 20" aria-hidden>
      <path d="m7 4.75 5.25 5.25L7 15.25" />
    </svg>
  );
}
