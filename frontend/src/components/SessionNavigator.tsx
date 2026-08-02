import { useEffect, useMemo, useState, type DragEvent } from 'react';
import type { SessionInfo } from '../types';
import { getTabLabel, sessionLabel } from '../lib/tabLabels';
import { ProviderMark, normalizeProvider } from './ProviderBadge';
import {
  canContinueSession,
  endedAtLabel,
  endedSummary,
  isCrashedSession,
  isDegradedSession
} from '../lib/sessionStatus';
import {
  effectiveParentId,
  groupWorkingSet,
  humanEngagementAt,
  isAgentLedChild,
  isSetAside
} from '../lib/workingSet';
import { useSessions } from '../store/sessions';
import { MachineMark } from './MachineMark';
import { ContinueElsewhereButton } from './ContinueElsewhereButton';
import { serverDisplayName, useServers } from '../lib/servers';

type PrimaryFilter = 'all' | 'needs' | 'working' | 'later' | 'quiet';
type ProviderFilter = 'all' | 'claude' | 'codex' | 'shell';
type DateFilter = 'all' | 'today' | 'week';

const PIN_KEY = 'sessions:pinned-managers:v1';
const RECENTLY_ENDED_DAYS = 7;
const RECENTLY_ENDED_LIMIT = 20;

function readPins(): string[] {
  try {
    const value = JSON.parse(window.localStorage.getItem(PIN_KEY) ?? '[]');
    return Array.isArray(value) ? value.filter((id): id is string => typeof id === 'string').slice(0, 5) : [];
  } catch { return []; }
}

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

function isFinished(session: SessionInfo): boolean {
  return (session.exited && !isCrashedSession(session))
    || (!session.exited && !session.working && session.idleReason === 'completed');
}
function needsYou(session: SessionInfo): boolean {
  if (session.exited || session.working) return false;
  if (isDegradedSession(session)) return true;
  if (session.idleReason) return session.idleReason === 'needs-input';
  // Unknown idle state is not enough evidence to alarm the user. Providers
  // that can identify a question or approval record needs-input explicitly.
  return false;
}

interface Props {
  sessions: SessionInfo[];
  activeId: string | null;
  machine: string;
  onOpen: (id: string) => void;
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
  const updateSetAside = useSessions((state) => state.updateSetAside);
  const configuredMachines = useServers((state) => state.servers);
  const activeMachineId = useServers((state) => state.activeId);
  const selectMachine = useServers((state) => state.setActive);
  const [primary, setPrimary] = useState<PrimaryFilter>('all');
  const [provider, setProvider] = useState<ProviderFilter>('all');
  const [project, setProject] = useState('all');
  const [date, setDate] = useState<DateFilter>('all');
  const [query, setQuery] = useState('');
  const [pins, setPins] = useState<string[]>(readPins);
  const [expandedCompleted, setExpandedCompleted] = useState<Set<string>>(new Set());
  const [expandedHelpers, setExpandedHelpers] = useState<Set<string>>(new Set());
  const [draggingId, setDraggingId] = useState<string | null>(null);
  const [dropTargetId, setDropTargetId] = useState<string | null>(null);
  const [movingId, setMovingId] = useState<string | null>(null);
  const [moveError, setMoveError] = useState<string | null>(null);
  const [runningOpen, setRunningOpen] = useState(true);
  const [setAsideOpen, setSetAsideOpen] = useState(false);
  const [endedOpen, setEndedOpen] = useState(false);
  const [showAllEnded, setShowAllEnded] = useState(false);
  const [selectingEnded, setSelectingEnded] = useState(false);
  const [selectedEnded, setSelectedEnded] = useState<Set<string>>(new Set());
  const [archiveBusy, setArchiveBusy] = useState(false);
  const [archiveError, setArchiveError] = useState<string | null>(null);
  const [collapsedTrees, setCollapsedTrees] = useState<Set<string>>(new Set());
  const [movePickerId, setMovePickerId] = useState<string | null>(null);
  const [actionMenuId, setActionMenuId] = useState<string | null>(null);
  const [endingId, setEndingId] = useState<string | null>(null);
  const [copyingId, setCopyingId] = useState<string | null>(null);

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

  const sessionIds = useMemo(() => new Set(sessions.map((session) => session.id)), [sessions]);
  const endedSessionIds = useMemo(() => new Set(sessions.filter((session) => session.exited).map((session) => session.id)), [sessions]);
  useEffect(() => {
    setPins((current) => {
      const next = current.filter((id) => sessionIds.has(id)).slice(0, 5);
      if (next.length === current.length && next.every((id, index) => id === current[index])) return current;
      try { window.localStorage.setItem(PIN_KEY, JSON.stringify(next)); } catch { /* non-fatal */ }
      return next;
    });
  }, [sessionIds]);
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
      setSetAsideOpen(true);
    }
    if (searching || primary === 'quiet') setEndedOpen(true);
    if (primary === 'later') setSetAsideOpen(true);
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
      if (element?.closest(`[data-session-actions="${actionMenuId}"]`)) return;
      setActionMenuId(null);
    };
    const dismissOnEscape = (event: KeyboardEvent): void => {
      if (event.key !== 'Escape' || event.defaultPrevented) return;
      const summary = document.querySelector<HTMLElement>(`[data-session-actions="${actionMenuId}"] > summary`);
      setActionMenuId(null);
      summary?.focus();
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
    for (const session of sessions) {
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
  }, [sessions, sessionIds]);

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
    for (const session of sessions) visit(session);
    return values;
  }, [children, sessions]);

  const grouped = useMemo(
    () => groupWorkingSet(sessions, openSessionIds, pins),
    [openSessionIds, pins, sessions]
  );
  const sortRoots = (items: SessionInfo[], honorPins: boolean): SessionInfo[] => [...items].sort((a, b) => {
    if (honorPins) {
      const ap = pins.indexOf(a.id); const bp = pins.indexOf(b.id);
      if (ap >= 0 || bp >= 0) return ap < 0 ? 1 : bp < 0 ? -1 : ap - bp;
    }
    return (subtreeHumanEngagement.get(b.id) ?? humanEngagementAt(b))
      - (subtreeHumanEngagement.get(a.id) ?? humanEngagementAt(a));
  });
  const runningRoots = sortRoots(grouped.runningRoots, true);
  const setAsideRoots = sortRoots(grouped.setAsideRoots, false);

  const projects = useMemo(() => [...new Set(sessions.map(projectName).filter(Boolean))].sort(), [sessions]);
  const counts = useMemo(() => ({
    needs: sessions.filter(needsYou).length,
    running: sessions.filter((session) => grouped.runningIds.has(session.id)).length,
    aside: sessions.filter((session) => grouped.setAsideIds.has(session.id)).length,
    working: sessions.filter((session) => session.working && !session.exited).length
  }), [grouped.runningIds, grouped.setAsideIds, sessions]);
  const setAsideAttention = useMemo(() => {
    const setAside = sessions.filter((session) => grouped.setAsideIds.has(session.id));
    return {
      needs: setAside.filter(needsYou).length,
      limited: setAside.filter(isDegradedSession).length
    };
  }, [grouped.setAsideIds, sessions]);

  const matches = (session: SessionInfo): boolean => {
    if (primary === 'needs' && !needsYou(session)) return false;
    if (primary === 'working' && (!session.working || session.exited)) return false;
    if (primary === 'later' && !grouped.setAsideIds.has(session.id)) return false;
    if (primary === 'quiet' && !session.exited) return false;
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
  const filteredRunningRoots = runningRoots.filter((root) => treeMatches(root, grouped.runningIds));
  const filteredSetAsideRoots = setAsideRoots.filter((root) => treeMatches(root, grouped.setAsideIds));
  const filteredEnded = grouped.ended
    .filter(matches)
    .sort((left, right) => lastActivity(right) - lastActivity(left));
  const recentCutoff = Date.now() - RECENTLY_ENDED_DAYS * 86_400_000;
  const recentEnded = filteredEnded
    .filter((session) => lastActivity(session) >= recentCutoff)
    .slice(0, RECENTLY_ENDED_LIMIT);
  const browsingAllEnded = showAllEnded || primary === 'quiet' || query.trim() !== '';
  const visibleEnded = browsingAllEnded ? filteredEnded : recentEnded;
  const hasOlderEnded = filteredEnded.length > recentEnded.length;
  useEffect(() => {
    if (counts.running === 0 && filteredEnded.length > 0) setEndedOpen(true);
  }, [counts.running, filteredEnded.length]);
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
  const changeSetAside = async (session: SessionInfo, setAside: boolean): Promise<void> => {
    if (session.exited) return;
    setMoveError(null);
    setActionMenuId(null);
    try {
      await updateSetAside(session.id, setAside);
      if (setAside) {
        if (pins.includes(session.id)) togglePin(session.id, true);
        if (openSessionIds.includes(session.id)) onCloseView(session.id);
      }
    } catch (error) {
      setMoveError(error instanceof Error ? error.message : 'Could not update the working set.');
    }
  };
  const requestEndSession = async (session: SessionInfo): Promise<void> => {
    if (session.exited || endingId) return;
    const label = getTabLabel(session.id) ?? sessionLabel(session);
    if (!window.confirm(`End this session?\n\n“${label}” will stop running. Its conversation is kept, and you can resume it later from Quiet.`)) return;
    setEndingId(session.id);
    setMoveError(null);
    setActionMenuId(null);
    try {
      await endSession(session.id);
    } catch (error) {
      setMoveError(error instanceof Error ? error.message : 'Could not end the session.');
    } finally {
      setEndingId(null);
    }
  };
  const togglePin = (id: string, forceRemove = false): void => {
    const bringBack = !forceRemove && sessions.find((session) => session.id === id)?.setAsideAt != null;
    setPins((current) => {
      if (!current.includes(id) && current.length >= 5) return current;
      const next = current.includes(id) || forceRemove ? current.filter((item) => item !== id) : [...current, id];
      try { window.localStorage.setItem(PIN_KEY, JSON.stringify(next)); } catch { /* non-fatal */ }
      return next;
    });
    if (bringBack) {
      void updateSetAside(id, false).catch((error) => {
        setMoveError(error instanceof Error ? error.message : 'Could not bring the session back.');
      });
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
      needs: descendants.filter(needsYou).length,
      finished: descendants.filter((child) => child.exited || isFinished(child)).length
    };
  };

  const renderNode = (
    session: SessionInfo,
    depth: number,
    endedFlat = false,
    groupIds: Set<string> = grouped.runningIds
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
      || needsYou(child)
      || isDegradedSession(child)
      || isCrashedSession(child)
    ));
    const visibleAgentIDs = new Set((helpersExpanded ? agentChildren : urgentAgentChildren).map((child) => child.id));
    const completed = userChildren.filter((child) => isFinished(child) && !hasLiveDescendant(child, groupIds));
    const completedIds = new Set(completed.map((child) => child.id));
    const visible = nested.filter((child) => (
      isAgentLedChild(child)
        ? visibleAgentIDs.has(child.id)
        : !completedIds.has(child.id) || expandedCompleted.has(session.id)
    ));
    const helperRollup = {
      working: agentChildren.filter((child) => !child.exited && child.working).length,
      needs: agentChildren.filter(needsYou).length,
      finished: agentChildren.filter((child) => child.exited || isFinished(child)).length
    };
    const providerName = normalizeProvider(session.tool);
    const degraded = isDegradedSession(session);
    const status = session.exited ? 'finished' : degraded ? 'limited' : session.working ? 'running' : needsYou(session) ? 'needs' : 'idle';
    const end = session.exited ? endedSummary(session, sessions) : null;
    const currentParentID = effectiveParentId(session);
    const otherProvider = session.tool === 'claude-code'
      ? 'codex'
      : session.tool === 'codex'
        ? 'claude'
        : null;
    const otherProviderLabel = otherProvider === 'codex' ? 'Codex' : 'Claude';
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
          className={`session-nav-row is-${status}${session.id === activeId ? ' is-active' : ''}${draggingId === session.id ? ' is-dragging' : ''}${dropTargetId === session.id ? ' is-drop-target' : ''}`}
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
          <span className={`session-nav-status is-${status}`} aria-hidden />
          <span className="session-nav-copy">
            <span className="session-nav-title">{label}</span>
            {end ? <span className={`session-nav-ended is-${end.tone}`}>{end.label}</span> : null}
            {resumedFrom ? <span className="session-nav-parent">Resumed from {getTabLabel(resumedFrom.id) ?? sessionLabel(resumedFrom)}</span> : null}
            {endedFlat && parent ? <span className="session-nav-parent">Under {getTabLabel(parent.id) ?? sessionLabel(parent)}</span> : null}
            {!endedFlat && depth === 0 && parent && isSetAside(parent) ? <span className="session-nav-parent">Under {getTabLabel(parent.id) ?? sessionLabel(parent)} (Later)</span> : null}
            {session.setAsideAt != null ? <span className="session-nav-aside-chip">{session.exited ? 'Was in Later' : 'Later'}</span> : null}
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
            <details
              className="session-row-actions"
              data-session-actions={session.id}
              open={actionMenuId === session.id}
              onToggle={(event) => {
                if (event.currentTarget.open) setActionMenuId(session.id);
                else setActionMenuId((current) => current === session.id ? null : current);
              }}
              onClick={(event) => event.stopPropagation()}
            >
              <summary aria-label={`Actions for ${label}`} title="Session actions">•••</summary>
              <div className={`session-row-action-menu${session.exited ? ' opens-up' : ''}`} role="menu">
                <button type="button" role="menuitem" onClick={() => { setActionMenuId(null); onOpen(session.id); }}>{session.exited ? 'View history' : 'Open in tab'}</button>
                {openSessionIds.includes(session.id) ? <button type="button" role="menuitem" onClick={() => { setActionMenuId(null); onCloseView(session.id); }}>Close tab <small>keeps running in In focus</small></button> : null}
                {end && canContinueSession(session) ? <button type="button" role="menuitem" onClick={() => { setActionMenuId(null); onResumeSession(session); }}>Continue conversation…</button> : null}
                {end && canContinueSession(session) && otherProvider ? (
                  <button type="button" role="menuitem" onClick={() => {
                    setActionMenuId(null);
                    onResumeSession(session, otherProvider);
                  }}>Continue with {otherProviderLabel}…</button>
                ) : null}
                {end && canContinueSession(session) ? (
                  <ContinueElsewhereButton
                    sessionId={session.id}
                    label={label}
                    appearance="menuitem"
                    onOpen={() => setActionMenuId(null)}
                  />
                ) : null}
                {!session.exited && otherProvider && providerName ? (
                  <>
                    <button
                      type="button"
                      role="menuitem"
                      disabled={copyingId !== null}
                      onClick={() => void copyConversation(session, otherProvider)}
                    >
                      {copyingId === session.id ? 'Copying conversation…' : `Open a copy in ${otherProviderLabel}…`}
                      <small>original keeps running</small>
                    </button>
                    <button
                      type="button"
                      role="menuitem"
                      disabled={copyingId !== null}
                      onClick={() => void copyConversation(session, providerName)}
                    >
                      Fork a copy in {providerName === 'claude' ? 'Claude' : 'Codex'}…
                      <small>original keeps running</small>
                    </button>
                    <button type="button" role="menuitem" aria-disabled="true">Continue on another machine… <small>end first</small></button>
                  </>
                ) : null}
                {session.reopenedAs ? <button type="button" role="menuitem" onClick={() => { setActionMenuId(null); onOpen(session.reopenedAs!); }}>Open resumed runtime</button> : null}
                {session.resumedFrom ? <button type="button" role="menuitem" onClick={() => { setActionMenuId(null); onOpen(session.resumedFrom!); }}>View previous runtime</button> : null}
                <button type="button" role="menuitem" onClick={() => { setActionMenuId(null); onStartLinked(session.id); }}>Start child session…</button>
                <button type="button" role="menuitem" onClick={() => { setActionMenuId(null); setMovePickerId(session.id); }}>Group under…</button>
                {currentParentID ? <button type="button" role="menuitem" onClick={() => void moveSession(session.id, null)}>Make top-level</button> : null}
                {!currentParentID ? <button type="button" role="menuitem" disabled={!pins.includes(session.id) && pins.length >= 5} onClick={() => { togglePin(session.id); setActionMenuId(null); }}>{pins.includes(session.id) ? 'Unpin manager' : 'Pin manager'}</button> : null}
                {session.exited ? (
                  <>
                    <button type="button" role="menuitem" onClick={() => { setSelectingEnded(true); setSelectedEnded(new Set([session.id])); setEndedOpen(true); setActionMenuId(null); }}>Select</button>
                    <button type="button" role="menuitem" onClick={() => { setActionMenuId(null); void archiveEnded([session.id]); }}>Archive from list</button>
                  </>
                ) : (
                  <>
                    {isSetAside(session)
                      ? <button type="button" role="menuitem" onClick={() => void changeSetAside(session, false)}>Bring into focus</button>
                      : <button type="button" role="menuitem" onClick={() => void changeSetAside(session, true)}>Move to Later <small>keeps running</small></button>}
                    <button type="button" role="menuitem" disabled={endingId !== null} onClick={() => void requestEndSession(session)}>{endingId === session.id ? 'Ending…' : 'End session…'}</button>
                  </>
                )}
              </div>
            </details>
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
  const movePickerSession = movePickerId ? sessions.find((session) => session.id === movePickerId) ?? null : null;
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
          <button type="button" className="session-continue-action" onClick={onContinue}>Continue</button>
          <button type="button" className="session-new-action" onClick={onNew} aria-label="New session">＋</button>
        </div>
      </header>
      <div className="session-machine-filter" role="toolbar" aria-label="Connected computers">
        {configuredMachines.map((configured) => (
          <button
            type="button"
            key={configured.id}
            className={configured.id === activeMachineId ? 'is-active' : undefined}
            aria-pressed={configured.id === activeMachineId}
            title={`Show sessions on ${serverDisplayName(configured, true)}`}
            onClick={() => selectMachine(configured.id)}
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
        <FilterButton label="Later" active={primary === 'later'} onClick={() => setPrimary('later')} />
        <FilterButton label="Quiet" active={primary === 'quiet'} onClick={() => setPrimary('quiet')} />
        <details className="session-more-filters">
          <summary aria-label="More filters">⋯</summary>
          <div className="session-filter-popover">
            <label>Provider<select value={provider} onChange={(event) => setProvider(event.currentTarget.value as ProviderFilter)}><option value="all">All providers</option><option value="claude">Claude</option><option value="codex">Codex</option><option value="shell">Shell</option></select></label>
            <label>Computer<select value={activeMachineId ?? ''} onChange={(event) => selectMachine(event.currentTarget.value)}>{configuredMachines.map((configured) => <option key={configured.id} value={configured.id}>{serverDisplayName(configured, true)}</option>)}</select></label>
            <label>Project<select value={project} onChange={(event) => setProject(event.currentTarget.value)}><option value="all">All projects</option>{projects.map((item) => <option key={item}>{item}</option>)}</select></label>
            <label>Date<select value={date} onChange={(event) => setDate(event.currentTarget.value as DateFilter)}><option value="all">Any time</option><option value="today">Today</option><option value="week">Past 7 days</option></select></label>
          </div>
        </details>
      </div>
      <div className="session-tree" role="tree">
        {moveError ? <div className="session-move-error" role="alert">{moveError}</div> : null}
        {archiveError ? <div className="session-move-error" role="alert">{archiveError}</div> : null}
        {selectingEnded ? (
          <div className="session-bulk-actions">
            <strong>{selectedEnded.size} selected</strong>
            <button type="button" disabled={archiveBusy || selectedEnded.size === 0} onClick={() => void archiveSelected()}>{archiveBusy ? 'Archiving…' : 'Archive'}</button>
            <button type="button" onClick={() => { setSelectingEnded(false); setSelectedEnded(new Set()); }}>Cancel</button>
          </div>
        ) : null}
        {draggingId ? (
          <div
            className={`session-root-drop${dropTargetId === '__root__' ? ' is-drop-target' : ''}`}
            onDragOver={(event) => dragOverTarget(event, null)}
            onDragLeave={() => setDropTargetId((current) => current === '__root__' ? null : current)}
            onDrop={(event) => dropOnTarget(event, null)}
          >
            Drop here to make this a manager session
          </div>
        ) : null}
        {primary !== 'quiet' && primary !== 'later' ? <div className="session-tree-group">
          <button type="button" className="session-tree-group-head" onClick={() => setRunningOpen((current) => !current)}>
            <span className="session-group-disclosure"><DisclosureChevron open={runningOpen} /> In focus</span><strong>{counts.running}</strong>
          </button>
          {runningOpen ? (
            <>
              {pins.some((id) => filteredRunningRoots.some((root) => root.id === id)) ? <div className="session-tree-label">Pinned managers <span>{Math.min(pins.length, 5)}/5</span></div> : null}
              {filteredRunningRoots.map((root) => renderNode(root, 0, false, grouped.runningIds))}
              {filteredRunningRoots.length === 0 ? <div className="session-tree-empty is-compact">No matching work in focus.</div> : null}
            </>
          ) : null}
        </div> : null}
        {primary !== 'quiet' && (counts.aside > 0 || primary === 'later') ? <div className="session-tree-group is-set-aside">
          <button type="button" className="session-tree-group-head" onClick={() => setSetAsideOpen((current) => !current)}>
            <span className="session-group-disclosure"><DisclosureChevron open={setAsideOpen} /> Later</span>
            <span className={(setAsideAttention.needs || setAsideAttention.limited) ? 'session-group-attention' : undefined}>
              {counts.aside}
              {setAsideAttention.needs ? ` · ${setAsideAttention.needs} needs you` : ''}
              {setAsideAttention.limited ? ` · ${setAsideAttention.limited} limited` : ''}
            </span>
          </button>
          {setAsideOpen ? (
            <>
              {filteredSetAsideRoots.map((root) => renderNode(root, 0, false, grouped.setAsideIds))}
              {filteredSetAsideRoots.length === 0 ? <div className="session-tree-empty is-compact">Nothing is waiting for later.</div> : null}
            </>
          ) : null}
        </div> : null}
        {primary !== 'working' && primary !== 'needs' && primary !== 'later' ? <div className="session-tree-group">
          <div className="session-tree-group-head">
            <button type="button" onClick={() => setEndedOpen((current) => !current)}><span className="session-group-disclosure"><DisclosureChevron open={endedOpen} /> Quiet</span><strong>{visibleEnded.length}</strong></button>
            {filteredEnded.length > 0 ? <button type="button" className="session-select-ended" onClick={() => { setSelectingEnded((current) => !current); setEndedOpen(true); }}>Select</button> : null}
          </div>
          {endedOpen ? (
            <>
              {visibleEnded.map((session) => renderNode(session, 0, true))}
              {!browsingAllEnded && hasOlderEnded ? <button type="button" className="session-all-ended" onClick={() => setShowAllEnded(true)}>All quiet sessions →</button> : null}
              {showAllEnded && primary !== 'quiet' && query.trim() === '' ? <button type="button" className="session-all-ended" onClick={() => setShowAllEnded(false)}>Show recent only</button> : null}
              {visibleEnded.length === 0 ? <div className="session-tree-empty is-compact">No quiet sessions yet.</div> : null}
            </>
          ) : null}
        </div> : null}
        {sessions.length === 0 ? <div className="session-tree-empty">No sessions on {machine}.</div> : null}
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
                  <small>{candidate.exited ? 'Quiet manager' : candidate.working ? 'Working' : 'In focus · idle'} · {projectName(candidate)}</small>
                </button>
              ))}
            </div>
            {moveCandidates.length === 0 ? <div className="session-tree-empty">No eligible manager sessions.</div> : null}
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
