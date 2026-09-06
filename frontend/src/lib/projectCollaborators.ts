import type { ProjectView } from '../api/sessionsd';
import type { SessionInfo } from '../types';
import type { ServerConfig } from './servers';
import { collapseConversationRuntimes, humanEngagementAt, isAgentLedChild, isPinned } from './workingSet';

export interface Collaborator { session: SessionInfo; server: ServerConfig; unavailable: boolean }
export interface CollaboratorProject { id: string; name: string; rows: Collaborator[] }

export function isSavedCollaborator(session: SessionInfo): boolean {
  return Boolean(session.exited || session.runnerGone || session.unreachable);
}

function projectIdentity(session: SessionInfo, server: ServerConfig, project?: ProjectView): { id: string; name: string } {
  const tag = session.tags?.project?.trim() || session.tags?.product?.trim();
  if (tag) return { id: `tag:${tag}`, name: tag };
  if (project?.somewhere) return { id: `somewhere:${project.somewhere}`, name: project.name };
  if (project?.github) return { id: `github:${project.github.toLowerCase()}`, name: project.name.split('/').pop() || project.name };
  if (project) return { id: `${server.id}:${project.id}`, name: project.name };
  const path = session.sourceRepo || session.cwd;
  return { id: `${server.id}:folder:${path}`, name: path.split(/[\\/]/).filter(Boolean).pop() || 'Other work' };
}

// Never merge unrelated folders just because their last component matches.
// Shared project tags and daemon-provided repository identities are explicit
// cross-machine evidence; otherwise retain host-qualified identities.
export function groupCollaborators(
  snapshots: Array<{ server: ServerConfig; sessions: SessionInfo[]; error: string | null }>,
  projects: Record<string, ProjectView[]>,
  saved: boolean,
  matches: (session: SessionInfo) => boolean
): CollaboratorProject[] {
  const groups = new Map<string, CollaboratorProject>();
  for (const snapshot of snapshots) {
    const membership = new Map((projects[snapshot.server.id] ?? []).flatMap((p) => p.session_ids.map((id) => [id, p] as const)));
    for (const session of collapseConversationRuntimes(snapshot.sessions)) {
      if (isAgentLedChild(session) || isSavedCollaborator(session) !== saved || !matches(session)) continue;
      if (!saved && session.setAsideAt && !isPinned(session)) continue;
      const identity = projectIdentity(session, snapshot.server, membership.get(session.id));
      const group = groups.get(identity.id) ?? { ...identity, rows: [] };
      group.rows.push({ session, server: snapshot.server, unavailable: Boolean(snapshot.error) });
      groups.set(identity.id, group);
    }
  }
  const rank = (row: Collaborator): number => humanEngagementAt(row.session);
  for (const group of groups.values()) group.rows.sort((a, b) => Number(isPinned(b.session)) - Number(isPinned(a.session)) || rank(b) - rank(a));
  return [...groups.values()].sort((a, b) => Math.max(...b.rows.map(rank)) - Math.max(...a.rows.map(rank)));
}
