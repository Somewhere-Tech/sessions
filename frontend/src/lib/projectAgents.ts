import type { ProjectView } from '../api/sessionsd';
import type { SessionInfo } from '../types';
import type { ServerConfig } from './servers';
import { collapseConversationRuntimes, humanEngagementAt, isAgentLedChild, isPinned } from './workingSet';
import { resolvedSessionLabel } from './tabLabels';

export interface Agent { session: SessionInfo; server: ServerConfig; unavailable: boolean; projectName?: string }
export interface AgentProject { id: string; name: string; rows: Agent[] }

export function isSavedAgent(session: SessionInfo): boolean {
  return Boolean(session.exited || session.runnerGone || session.unreachable);
}

export function projectIdentity(session: SessionInfo, server: ServerConfig, project?: ProjectView): { id: string; name: string } {
  const tag = session.tags?.project?.trim() || session.tags?.product?.trim();
  if (tag) return { id: `tag:${tag}`, name: tag };
  if (project?.somewhere) return { id: `somewhere:${project.somewhere}`, name: project.name };
  if (project?.github) return { id: `github:${project.github.toLowerCase()}`, name: project.name.split('/').pop() || project.name };
  if (project) return { id: `${server.id}:${project.id}`, name: project.name };
  const path = session.sourceRepo || session.cwd;
  return { id: `${server.id}:folder:${path}`, name: path.split(/[\\/]/).filter(Boolean).pop() || 'Other work' };
}

export function projectMembershipKey(serverId: string, sessionId: string): string {
  return JSON.stringify([serverId, sessionId]);
}

export function resolvedProjects(
  snapshots: Array<{ server: ServerConfig; sessions: SessionInfo[] }>,
  projects: Record<string, ProjectView[]>
): Map<string, { id: string; name: string }> {
  const result = new Map<string, { id: string; name: string }>();
  for (const { server, sessions } of snapshots) {
    const membership = new Map((projects[server.id] ?? []).flatMap((p) => p.session_ids.map((id) => [id, p] as const)));
    for (const session of sessions) result.set(projectMembershipKey(server.id, session.id), projectIdentity(session, server, membership.get(session.id)));
  }
  return result;
}

export function recentAgents(groups: AgentProject[]): AgentProject[] {
  const rows = groups.flatMap((group) => group.rows.map((row) => ({ ...row, projectName: group.name })));
  rows.sort((a, b) => Number(isPinned(b.session)) - Number(isPinned(a.session)) || humanEngagementAt(b.session) - humanEngagementAt(a.session));
  return rows.length ? [{ id: 'recent', name: 'Most recent', rows }] : [];
}

export function navigationProjectOptions(membership: Map<string, { id: string; name: string }>, servers: ServerConfig[]): Array<{ id: string; name: string }> {
  const options = new Map<string, { id: string; name: string; servers: Set<string> }>();
  for (const [key, project] of membership) {
    const serverId = JSON.parse(key)[0] as string;
    const server = servers.find((item) => item.id === serverId);
    const option = options.get(project.id) ?? { ...project, servers: new Set<string>() };
    option.servers.add(server?.name || serverId);
    options.set(project.id, option);
  }
  const rows = [...options.values()];
  return rows.map((row) => ({ id: row.id, name: rows.some((other) => other.id !== row.id && other.name === row.name)
    ? `${row.name} · ${[...row.servers].sort().join(', ')}` : row.name })).sort((a, b) => a.name.localeCompare(b.name));
}

export function matchesResolvedProject(session: SessionInfo, resolved: { id: string; name: string } | undefined, project: string, query: string): boolean {
  if (project !== 'all' && resolved?.id !== project) return false;
  const needle = query.trim().toLowerCase();
  const haystack = `${resolved?.name ?? ''} ${resolvedSessionLabel(session)} ${session.name ?? ''} ${session.description ?? ''} ${session.cwd} ${session.lastSummary ?? ''} ${Object.values(session.tags ?? {}).join(' ')}`.toLowerCase();
  return !needle || haystack.includes(needle);
}

// Never merge unrelated folders just because their last component matches.
// Shared project tags and daemon-provided repository identities are explicit
// cross-machine evidence; otherwise retain host-qualified identities.
export function groupAgents(
  snapshots: Array<{ server: ServerConfig; sessions: SessionInfo[]; error: string | null }>,
  projects: Record<string, ProjectView[]>,
  saved: boolean,
  matches: (session: SessionInfo, serverId: string) => boolean
): AgentProject[] {
  const groups = new Map<string, AgentProject>();
  for (const snapshot of snapshots) {
    const membership = new Map((projects[snapshot.server.id] ?? []).flatMap((p) => p.session_ids.map((id) => [id, p] as const)));
    for (const session of collapseConversationRuntimes(snapshot.sessions)) {
      if (isAgentLedChild(session) || isSavedAgent(session) !== saved || !matches(session, snapshot.server.id)) continue;
      if (!saved && session.setAsideAt && !isPinned(session)) continue;
      const identity = projectIdentity(session, snapshot.server, membership.get(session.id));
      const group = groups.get(identity.id) ?? { ...identity, rows: [] };
      group.rows.push({ session, server: snapshot.server, unavailable: Boolean(snapshot.error) });
      groups.set(identity.id, group);
    }
  }
  const rank = (row: Agent): number => humanEngagementAt(row.session);
  for (const group of groups.values()) group.rows.sort((a, b) => Number(isPinned(b.session)) - Number(isPinned(a.session)) || rank(b) - rank(a));
  return [...groups.values()].sort((a, b) => Math.max(...b.rows.map(rank)) - Math.max(...a.rows.map(rank)));
}
