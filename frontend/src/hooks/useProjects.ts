import { useEffect, useMemo, useState } from 'react';
import { fetchProjects, type ProjectView } from '../api/sessionsd';
import type { ProjectRef } from '../lib/inboxSections';
import { getServer } from '../lib/servers';

const EMPTY_PROJECTS: ProjectView[] = [];

// Which project each session belongs to, as the daemon resolved it. The
// listing is cheap (it reads a few files per session) and refreshed when the
// set of sessions changes, so a new session lands in its project as soon as
// the navigator sees it. A daemon without the route leaves the map empty and
// the inbox falls back to one section.
export function useProjects(sessionIds: string[], enabled: boolean, serverId: string): {
  bySession: Map<string, ProjectRef>;
  projects: ProjectView[];
  error: string | null;
} {
  const [snapshots, setSnapshots] = useState<Record<string, { projects: ProjectView[]; error: string | null }>>({});
  const projects = snapshots[serverId]?.projects ?? EMPTY_PROJECTS;
  const error = snapshots[serverId]?.error ?? null;
  const key = sessionIds.slice().sort().join(',');
  useEffect(() => {
    if (!enabled) return;
    let alive = true;
    const load = async (): Promise<void> => {
      try {
        const listed = await fetchProjects(undefined, getServer(serverId));
        if (alive) {
          setSnapshots((current) => ({ ...current, [serverId]: { projects: listed, error: null } }));
        }
      } catch (reason) {
        if (alive) {
          setSnapshots((current) => ({ ...current, [serverId]: {
            projects: current[serverId]?.projects ?? [],
            error: reason instanceof Error ? reason.message : 'Project groups could not be loaded.'
          } }));
        }
      }
    };
    void load();
    const timer = window.setInterval(() => { void load(); }, 30_000);
    return () => { alive = false; window.clearInterval(timer); };
  }, [enabled, key, serverId]);
  const bySession = useMemo(() => {
    const map = new Map<string, ProjectRef>();
    for (const project of projects) {
      const ref: ProjectRef = { id: project.id, name: project.name, implicit: project.implicit, pinned: project.pinned };
      for (const id of project.session_ids) map.set(id, ref);
    }
    return map;
  }, [projects]);
  return { bySession, projects, error };
}
