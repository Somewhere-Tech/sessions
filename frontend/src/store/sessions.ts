import { create } from 'zustand';
import * as api from '../api/sessionsd';
import type { CreateSessionRequest, SessionInfo } from '../types';
import {
  filterSessionsForWindow,
  readWindowScope,
  sessionMatchesWindowScope
} from '../lib/windowScope';
import { reconcileDurableTabLabels } from '../lib/tabLabels';
import {
  cacheMachineSessions,
  lastCachedMachine,
  migrateLegacyCaches,
  readMachineCache,
  type CachedMachine
} from './sessionCache';

const windowScope = readWindowScope();

// Re-use the previous object for any session whose render-relevant fields
// are unchanged, so component selectors (`sessions.find(id)`) keep stable
// references across the 3s poll. Without this, every poll replaces all
// objects → every mounted SessionView re-renders on a timer (36 of them),
// a periodic main-thread hitch that shows up as laggy terminal input.
// lastDataAt is render-relevant now: Home and the operations navigator use it
// for activity timestamps and ordering. The API is polled every three seconds,
// so accepting one re-render per changed session per poll keeps that UI honest
// without reverting to per-output-byte React updates.
function reconcileSessions(prev: SessionInfo[], fresh: SessionInfo[]): SessionInfo[] {
  const prevById = new Map(prev.map((s) => [s.id, s]));
  const next = fresh.map((f) => {
    const old = prevById.get(f.id);
    if (
      old &&
      old.working === f.working &&
      old.exited === f.exited &&
      old.exitCode === f.exitCode &&
      old.exitSignal === f.exitSignal &&
      old.exitReason === f.exitReason &&
      old.exitedAt === f.exitedAt &&
      old.unreachable === f.unreachable &&
      old.unreachableReason === f.unreachableReason &&
      old.unreachableSince === f.unreachableSince &&
      old.runnerGone === f.runnerGone &&
      old.lastDataAt === f.lastDataAt &&
      old.lastUserMessageAt === f.lastUserMessageAt &&
      old.lastHumanMessageAt === f.lastHumanMessageAt &&
      old.lastAgentMessageAt === f.lastAgentMessageAt &&
      old.idleReason === f.idleReason &&
      old.idleDetail === f.idleDetail &&
      old.idleSince === f.idleSince &&
      old.lastSummary === f.lastSummary &&
      old.failureKind === f.failureKind &&
      old.failureDetail === f.failureDetail &&
      old.failureProvider === f.failureProvider &&
      old.failureAt === f.failureAt &&
      old.retry?.attempt === f.retry?.attempt &&
      old.retry?.max === f.retry?.max &&
      old.retry?.nextAt === f.retry?.nextAt &&
      old.retry?.kind === f.retry?.kind &&
      old.cwd === f.cwd &&
      old.cmd === f.cmd &&
      old.tool === f.tool &&
      old.kind === f.kind &&
      old.name === f.name &&
      old.description === f.description &&
      old.profile === f.profile &&
      old.configDir === f.configDir &&
      old.worktreePath === f.worktreePath &&
      old.branch === f.branch &&
      old.base === f.base &&
      old.sourceRepo === f.sourceRepo &&
      old.parentSessionId === f.parentSessionId &&
      old.delegationKind === f.delegationKind &&
      old.displayParentSessionId === f.displayParentSessionId &&
      old.setAsideAt === f.setAsideAt &&
      // Without this the refresh diff treats a pin change as no change at all,
      // and a toggle the user just made is silently reverted on the next poll.
      old.pinned === f.pinned &&
      old.creatorKind === f.creatorKind &&
      old.creatorId === f.creatorId &&
      old.rootCreatorKind === f.rootCreatorKind &&
      old.rootCreatorId === f.rootCreatorId &&
      old.provenanceStatus === f.provenanceStatus &&
      old.reopenedAs === f.reopenedAs &&
      old.resumedFrom === f.resumedFrom &&
      old.movedToEndpoint === f.movedToEndpoint &&
      old.movedToSessionId === f.movedToSessionId &&
      old.movedFromEndpoint === f.movedFromEndpoint &&
      old.movedFromSessionId === f.movedFromSessionId &&
      old.endedByKind === f.endedByKind &&
      old.endedById === f.endedById &&
      old.endedByName === f.endedByName &&
      old.endedByClient === f.endedByClient &&
      old.endReason === f.endReason &&
      old.endOperationId === f.endOperationId &&
      arrayEqual(old.creatorAncestry, f.creatorAncestry) &&
      old.model === f.model &&
      old.effort === f.effort &&
      old.fast === f.fast &&
      old.conversationId === f.conversationId &&
      old.cols === f.cols &&
      old.rows === f.rows &&
      old.pid === f.pid &&
      old.runnerProtocol === f.runnerProtocol &&
      old.runnerVersion === f.runnerVersion &&
      old.claudeCustomTitle === f.claudeCustomTitle &&
      old.claudeAiTitle === f.claudeAiTitle &&
      tagsEqual(old.tags, f.tags)
    ) {
      return old;
    }
    return f;
  });
  // If every element is the same object in the same order as before, return
  // the PREVIOUS array reference so subscribers to the whole `sessions` array
  // (App, SessionTabs, GridView) don't re-render at all on an idle 3s poll.
  if (next.length === prev.length && next.every((s, i) => s === prev[i])) return prev;
  return next;
}

function arrayEqual(left: string[] | undefined, right: string[] | undefined): boolean {
  if (left === right) return true;
  if (!left || !right || left.length !== right.length) return false;
  return left.every((value, index) => value === right[index]);
}

function tagsEqual(
  left: Record<string, string> | undefined,
  right: Record<string, string> | undefined
): boolean {
  const leftEntries = Object.entries(left ?? {});
  const rightEntries = Object.entries(right ?? {});
  return leftEntries.length === rightEntries.length
    && leftEntries.every(([key, value]) => right?.[key] === value);
}

interface SessionsState {
  // The daemon whose rows currently occupy this store. Session ids are only
  // meaningful inside that machine. Keeping the scope explicit prevents a
  // failed machine switch from relabelling the previous machine's cache.
  serverId: string | null;
  sessions: SessionInfo[];
  activeId: string | null;
  // Whether the store has rendered with at least one fresh refresh()
  // result from sessionsd. Stays false during the localStorage-hydrated
  // phase right after PWA cold-start. Lets the UI tell "this is the
  // last-known state, fetching fresh" from "this is live."
  hydrated: boolean;
  loading: boolean;
  error: string | null;
  setServerScope: (serverId: string) => void;
  refresh: (serverId?: string) => Promise<void>;
  create: (req: CreateSessionRequest, serverId?: string) => Promise<SessionInfo>;
  kill: (id: string, reason?: string) => Promise<void>;
  archive: (ids: string[]) => Promise<api.ArchiveResult>;
  updateName: (id: string, name: string) => Promise<void>;
  updateTags: (id: string, tags: Record<string, string>) => Promise<void>;
  updateModel: (id: string, model: string, effort: string) => Promise<void>;
  updateDisplayParent: (id: string, parentId: string | null) => Promise<void>;
  updateSetAside: (id: string, setAside: boolean) => Promise<void>;
  updatePinned: (id: string, pinned: boolean) => Promise<void>;
  setActive: (id: string | null) => void;
}

// LocalStorage cache so the PWA can render the familiar tab strip
// instantly on cold-start before the WS / refresh round-trip lands.
// The stored shape, its bounds and its write discipline live in
// ./sessionCache; this file is about turning a stored row back into a
// SessionInfo the UI can draw. On refresh() the live data overwrites
// everything within about a second.

function hydrateCachedMachine(
  serverId: string,
  cached: CachedMachine | null
): { serverId: string; sessions: SessionInfo[]; activeId: string | null } {
  try {
    if (!cached || !Array.isArray(cached.sessions)) {
      return { serverId, sessions: [], activeId: null };
    }
    const sessions: SessionInfo[] = filterSessionsForWindow(cached.sessions
        .map((c) => ({
          ...c,
          // Fields the cache no longer carries, because no first frame draws
          // them. The live listing fills them in.
          cmd: c.cmd ?? '',
          args: [],
          cwd: c.cwd ?? '',
          cols: 0,
          rows: 0,
          pid: 0,
          // Fill the live fields with neutral defaults — they'll be
          // overwritten by refresh() within ~1s of boot. We don't
          // pretend to know whether the cached session is still
          // working or even still alive.
          working: c.working ?? false,
          lastDataAt: c.lastDataAt ?? c.createdAt,
          lastUserMessageAt: c.lastUserMessageAt ?? null,
          lastHumanMessageAt: c.lastHumanMessageAt ?? null,
          lastAgentMessageAt: c.lastAgentMessageAt ?? null,
          exited: c.exited ?? false,
          exitCode: null,
          exitSignal: null,
          exitedAt: c.exitedAt ?? null
        })), windowScope);
    const savedActiveId = cached.activeId;
    const activeId = savedActiveId && sessions.some((session) => session.id === savedActiveId)
      ? savedActiveId
      : (sessions[0]?.id ?? null);
    return { serverId, sessions, activeId };
  } catch {
    return { serverId, sessions: [], activeId: null };
  }
}

function readCache(serverId?: string): { serverId: string | null; sessions: SessionInfo[]; activeId: string | null } {
  const target = serverId ?? lastCachedMachine();
  if (!target) return { serverId: null, sessions: [], activeId: null };
  return hydrateCachedMachine(target, readMachineCache(target));
}

function writeCache(serverId: string | null, sessions: SessionInfo[], activeId: string | null): void {
  cacheMachineSessions(serverId, sessions, activeId);
}

migrateLegacyCaches();

const initial = readCache();

export const useSessions = create<SessionsState>((set, get) => ({
  serverId: initial.serverId,
  sessions: initial.sessions,
  activeId: initial.activeId,
  hydrated: false,
  loading: false,
  error: null,

  setServerScope: (serverId) => {
    if (get().serverId === serverId) return;
    const cached = readCache(serverId);
    set({
      serverId,
      sessions: cached.sessions,
      activeId: cached.activeId,
      hydrated: false,
      loading: false,
      error: null
    });
  },

  refresh: async (requestedServerId) => {
    const serverId = requestedServerId ?? get().serverId;
    if (!serverId) return;
    if (get().serverId !== serverId) {
      get().setServerScope(serverId);
    }
    set({ loading: true, error: null });
    try {
      const fresh = filterSessionsForWindow(await api.listSessions(serverId), windowScope);
      // The active endpoint may have changed while this request was in
      // flight. Never commit one machine's response into another scope.
      if (get().serverId !== serverId) return;
      reconcileDurableTabLabels(fresh);
      const sessions = reconcileSessions(get().sessions, fresh);
      const active = get().activeId;
      const stillExists = active && sessions.some((s) => s.id === active);
      const nextActive = stillExists
        ? active
        : get().hydrated
        ? null
        : (sessions.find((session) => !session.exited)?.id ?? sessions[0]?.id ?? null);
      set({ sessions, loading: false, hydrated: true, activeId: nextActive });
      writeCache(serverId, sessions, nextActive);
    } catch (err) {
      if (get().serverId !== serverId) return;
      set({ loading: false, error: (err as Error).message });
      // Keep only this machine's last-known rows on a transient failure.
      // Cross-machine rows are cleared synchronously by setServerScope().
    }
  },

  create: async (req, requestedServerId) => {
    const serverId = requestedServerId ?? get().serverId;
    if (!serverId) throw new Error('Choose a computer before starting a session.');
    const info = await api.createSession(req, serverId);
    set((s) => {
      if (s.serverId !== serverId) return s;
      if (!sessionMatchesWindowScope(info, windowScope)) return s;
      const sessions = [...s.sessions, info];
      writeCache(s.serverId, sessions, info.id);
      return { sessions, activeId: info.id };
    });
    return info;
  },

  kill: async (id, reason) => {
    const serverId = get().serverId;
    if (!serverId) throw new Error('The session computer is not selected.');
    await api.killSession(id, reason, serverId);
    // Ending a process is not deleting its history. Refresh immediately so
    // the row moves to Finished/Failed while its transcript and lineage stay
    // available in the operations inbox.
    await get().refresh(serverId);
  },

  archive: async (ids) => {
    const result = await api.archiveSessions(ids);
    await get().refresh();
    return result;
  },

  updateName: async (id, requested) => {
    const name = await api.updateSessionName(id, requested);
    set((state) => {
      const sessions = state.sessions.map((session) => (
        session.id === id ? { ...session, name } : session
      ));
      writeCache(state.serverId, sessions, state.activeId);
      return { sessions };
    });
  },

  updateTags: async (id, requested) => {
    const tags = await api.updateSessionTags(id, requested);
    set((state) => {
      const sessions = state.sessions.map((session) => (
        session.id === id ? { ...session, tags } : session
      ));
      writeCache(state.serverId, sessions, state.activeId);
      return { sessions };
    });
  },

  updateModel: async (id, model, effort) => {
    const updated = await api.updateSessionModel(id, model, effort);
    set((state) => {
      const sessions = state.sessions.map((session) => (
        session.id === id ? { ...session, ...updated } : session
      ));
      writeCache(state.serverId, sessions, state.activeId);
      return { sessions };
    });
  },

  updateDisplayParent: async (id, parentId) => {
    const displayParentSessionId = await api.updateDisplayParent(id, parentId);
    set((state) => {
      const sessions = state.sessions.map((session) => (
        session.id === id ? { ...session, displayParentSessionId } : session
      ));
      writeCache(state.serverId, sessions, state.activeId);
      return { sessions };
    });
  },

  updateSetAside: async (id, setAside) => {
    const setAsideAt = await api.updateSetAside(id, setAside);
    set((state) => {
      const sessions = state.sessions.map((session) => (
        session.id === id ? { ...session, setAsideAt } : session
      ));
      writeCache(state.serverId, sessions, state.activeId);
      return { sessions };
    });
  },

  // The daemon's answer is what lands in the store, not the requested value:
  // a pin that was refused or changed on the way must not appear as applied.
  updatePinned: async (id, pinned) => {
    const stored = await api.updatePinned(id, pinned);
    set((state) => {
      const sessions = state.sessions.map((session) => (
        session.id === id ? { ...session, pinned: stored } : session
      ));
      writeCache(state.serverId, sessions, state.activeId);
      return { sessions };
    });
  },

  setActive: (id) => {
    set({ activeId: id });
    const state = get();
    writeCache(state.serverId, state.sessions, id);
  }
}));
