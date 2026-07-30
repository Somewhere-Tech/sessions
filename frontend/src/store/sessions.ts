import { create } from 'zustand';
import * as api from '../api/sessionsd';
import type { CreateSessionRequest, SessionInfo } from '../types';
import {
  filterSessionsForWindow,
  readWindowScope,
  sessionMatchesWindowScope
} from '../lib/windowScope';
import { reconcileDurableTabLabels } from '../lib/tabLabels';

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
      old.lastDataAt === f.lastDataAt &&
      old.lastUserMessageAt === f.lastUserMessageAt &&
      old.idleReason === f.idleReason &&
      old.idleDetail === f.idleDetail &&
      old.idleSince === f.idleSince &&
      old.lastSummary === f.lastSummary &&
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
      old.displayParentSessionId === f.displayParentSessionId &&
      old.setAsideAt === f.setAsideAt &&
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
  sessions: SessionInfo[];
  activeId: string | null;
  // Whether the store has rendered with at least one fresh refresh()
  // result from sessionsd. Stays false during the localStorage-hydrated
  // phase right after PWA cold-start. Lets the UI tell "this is the
  // last-known state, fetching fresh" from "this is live."
  hydrated: boolean;
  loading: boolean;
  error: string | null;
  refresh: () => Promise<void>;
  create: (req: CreateSessionRequest) => Promise<SessionInfo>;
  kill: (id: string, reason?: string) => Promise<void>;
  archive: (ids: string[]) => Promise<api.ArchiveResult>;
  updateName: (id: string, name: string) => Promise<void>;
  updateTags: (id: string, tags: Record<string, string>) => Promise<void>;
  updateModel: (id: string, model: string, effort: string) => Promise<void>;
  updateDisplayParent: (id: string, parentId: string | null) => Promise<void>;
  updateSetAside: (id: string, setAside: boolean) => Promise<void>;
  setActive: (id: string | null) => void;
}

// LocalStorage cache so the PWA can render the familiar tab strip
// instantly on cold-start before the WS / refresh round-trip lands.
// We only stash what the UI needs to draw a plausible first frame —
// not the full SessionInfo (working/lastDataAt are stale within
// seconds anyway). On refresh() the live data overwrites everything.
const CACHE_KEY = 'sessions:sessions-cache:v1';
const ACTIVE_KEY = 'sessions:active-session:v1';

interface CachedSession {
  id: string;
  name?: string;
  description?: string;
  tags?: Record<string, string>;
  cmd: string;
  args: string[];
  cwd: string;
  cols: number;
  rows: number;
  createdAt: number;
  pid: number;
  runnerProtocol?: number;
  runnerVersion?: string;
  tool: SessionInfo['tool'];
  kind?: string;
  model?: string;
  effort?: string;
  fast?: boolean;
  conversationId?: string;
  profile?: string;
  configDir?: string;
  worktreePath?: string;
  branch?: string;
  base?: string;
  sourceRepo?: string;
  parentSessionId?: string;
  displayParentSessionId?: string;
  setAsideAt?: number | null;
  creatorKind?: string;
  creatorId?: string;
  creatorAncestry?: string[];
  rootCreatorKind?: string;
  rootCreatorId?: string;
  provenanceStatus?: string;
  reopenedAs?: string;
  resumedFrom?: string;
  movedToEndpoint?: string;
  movedToSessionId?: string;
  movedFromEndpoint?: string;
  movedFromSessionId?: string;
  endedByKind?: string;
  endedById?: string;
  endedByName?: string;
  endedByClient?: string;
  endReason?: string;
  endOperationId?: string;
  working?: boolean;
  lastDataAt?: number;
  lastUserMessageAt?: number | null;
  idleReason?: SessionInfo['idleReason'];
  idleDetail?: string;
  idleSince?: number | null;
  lastSummary?: string;
  exited?: boolean;
  exitCode?: number | null;
  exitSignal?: string | null;
  exitReason?: string;
  exitedAt?: number | null;
  // Cache Claude-side titles so the PWA cold-start renders the correct
  // tab label without a flash-of-wrong-name before live data arrives.
  claudeCustomTitle?: string;
  claudeAiTitle?: string;
}

function readCache(): { sessions: SessionInfo[]; activeId: string | null } {
  try {
    const raw = window.localStorage.getItem(CACHE_KEY);
    const sessions: SessionInfo[] = filterSessionsForWindow(raw
        ? (JSON.parse(raw) as CachedSession[]).map((c) => ({
          ...c,
          args: Array.isArray(c.args) ? c.args : [],
          // Fill the live fields with neutral defaults — they'll be
          // overwritten by refresh() within ~1s of boot. We don't
          // pretend to know whether the cached session is still
          // working or even still alive.
          working: c.working ?? false,
          lastDataAt: c.lastDataAt ?? c.createdAt,
          lastUserMessageAt: c.lastUserMessageAt ?? null,
          exited: c.exited ?? false,
          exitCode: c.exitCode ?? null,
          exitSignal: c.exitSignal ?? null,
          exitReason: c.exitReason,
          exitedAt: c.exitedAt ?? null
        }))
      : [], windowScope);
    const savedActiveId = window.localStorage.getItem(ACTIVE_KEY);
    const activeId = savedActiveId && sessions.some((session) => session.id === savedActiveId)
      ? savedActiveId
      : (sessions[0]?.id ?? null);
    return { sessions, activeId };
  } catch {
    return { sessions: [], activeId: null };
  }
}

function writeCache(sessions: SessionInfo[], activeId: string | null): void {
  try {
    const stripped: CachedSession[] = sessions.map((s) => ({
      id: s.id,
      name: s.name,
      description: s.description,
      tags: s.tags,
      cmd: s.cmd,
      args: s.args,
      cwd: s.cwd,
      cols: s.cols,
      rows: s.rows,
      createdAt: s.createdAt,
      pid: s.pid,
      runnerProtocol: s.runnerProtocol,
      runnerVersion: s.runnerVersion,
      tool: s.tool,
      kind: s.kind,
      model: s.model,
      effort: s.effort,
      fast: s.fast,
      conversationId: s.conversationId,
      profile: s.profile,
      configDir: s.configDir,
      worktreePath: s.worktreePath,
      branch: s.branch,
      base: s.base,
      sourceRepo: s.sourceRepo,
      parentSessionId: s.parentSessionId,
      displayParentSessionId: s.displayParentSessionId,
      setAsideAt: s.setAsideAt,
      creatorKind: s.creatorKind,
      creatorId: s.creatorId,
      creatorAncestry: s.creatorAncestry,
      rootCreatorKind: s.rootCreatorKind,
      rootCreatorId: s.rootCreatorId,
      provenanceStatus: s.provenanceStatus,
      reopenedAs: s.reopenedAs,
      resumedFrom: s.resumedFrom,
      movedToEndpoint: s.movedToEndpoint,
      movedToSessionId: s.movedToSessionId,
      movedFromEndpoint: s.movedFromEndpoint,
      movedFromSessionId: s.movedFromSessionId,
      endedByKind: s.endedByKind,
      endedById: s.endedById,
      endedByName: s.endedByName,
      endedByClient: s.endedByClient,
      endReason: s.endReason,
      endOperationId: s.endOperationId,
      working: s.working,
      lastDataAt: s.lastDataAt,
      lastUserMessageAt: s.lastUserMessageAt,
      idleReason: s.idleReason,
      idleDetail: s.idleDetail,
      idleSince: s.idleSince,
      lastSummary: s.lastSummary,
      exited: s.exited,
      exitCode: s.exitCode,
      exitSignal: s.exitSignal,
      exitReason: s.exitReason,
      exitedAt: s.exitedAt,
      // Persist titles so they survive a PWA cold-start without flashing.
      claudeCustomTitle: s.claudeCustomTitle,
      claudeAiTitle: s.claudeAiTitle
    }));
    window.localStorage.setItem(CACHE_KEY, JSON.stringify(stripped));
    if (activeId) window.localStorage.setItem(ACTIVE_KEY, activeId);
    else window.localStorage.removeItem(ACTIVE_KEY);
  } catch {
    // quota / private mode — drop the cache silently
  }
}

const initial = readCache();

export const useSessions = create<SessionsState>((set, get) => ({
  sessions: initial.sessions,
  activeId: initial.activeId,
  hydrated: false,
  loading: false,
  error: null,

  refresh: async () => {
    set({ loading: true, error: null });
    try {
      const fresh = filterSessionsForWindow(await api.listSessions(), windowScope);
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
      writeCache(sessions, nextActive);
    } catch (err) {
      set({ loading: false, error: (err as Error).message });
      // Don't clear the cached sessions on a transient fetch failure —
      // the user keeps seeing their last-known tabs while reconnect
      // attempts happen in the background.
    }
  },

  create: async (req) => {
    const info = await api.createSession(req);
    set((s) => {
      if (!sessionMatchesWindowScope(info, windowScope)) return s;
      const sessions = [...s.sessions, info];
      writeCache(sessions, info.id);
      return { sessions, activeId: info.id };
    });
    return info;
  },

  kill: async (id, reason) => {
    await api.killSession(id, reason);
    // Ending a process is not deleting its history. Refresh immediately so
    // the row moves to Finished/Failed while its transcript and lineage stay
    // available in the operations inbox.
    await get().refresh();
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
      writeCache(sessions, state.activeId);
      return { sessions };
    });
  },

  updateTags: async (id, requested) => {
    const tags = await api.updateSessionTags(id, requested);
    set((state) => {
      const sessions = state.sessions.map((session) => (
        session.id === id ? { ...session, tags } : session
      ));
      writeCache(sessions, state.activeId);
      return { sessions };
    });
  },

  updateModel: async (id, model, effort) => {
    const updated = await api.updateSessionModel(id, model, effort);
    set((state) => {
      const sessions = state.sessions.map((session) => (
        session.id === id ? { ...session, ...updated } : session
      ));
      writeCache(sessions, state.activeId);
      return { sessions };
    });
  },

  updateDisplayParent: async (id, parentId) => {
    const displayParentSessionId = await api.updateDisplayParent(id, parentId);
    set((state) => {
      const sessions = state.sessions.map((session) => (
        session.id === id ? { ...session, displayParentSessionId } : session
      ));
      writeCache(sessions, state.activeId);
      return { sessions };
    });
  },

  updateSetAside: async (id, setAside) => {
    const setAsideAt = await api.updateSetAside(id, setAside);
    set((state) => {
      const sessions = state.sessions.map((session) => (
        session.id === id ? { ...session, setAsideAt } : session
      ));
      writeCache(sessions, state.activeId);
      return { sessions };
    });
  },

  setActive: (id) => {
    set({ activeId: id });
    try {
      if (id) window.localStorage.setItem(ACTIVE_KEY, id);
      else window.localStorage.removeItem(ACTIVE_KEY);
    } catch { /* ignore */ }
  }
}));
