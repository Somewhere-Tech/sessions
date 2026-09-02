import { useEffect, useMemo, useState } from 'react';
import { fetchProjects, type ProjectView } from '../api/sessionsd';
import type { ProjectRef } from '../lib/inboxSections';

// Which project each session belongs to, as the daemon resolved it. The
// listing is cheap (it reads a few files per session) and refreshed when the
// set of sessions changes, so a new session lands in its project as soon as
// the navigator sees it. A daemon without the route leaves the map empty and
// the inbox falls back to one section.
export function useProjects(sessionIds: string[], enabled: boolean): {
  bySession: Map<string, ProjectRef>;
  projects: ProjectView[];
  error: string | null;
} {
  const [projects, setProjects] = useState<ProjectView[]>([]);
  const [error, setError] = useState<string | null>(null);
  const key = sessionIds.slice().sort().join(',');
  useEffect(() => {
    if (!enabled) return;
    let alive = true;
    const load = async (): Promise<void> => {
      try {
        const listed = await fetchProjects();
        if (alive) {
          setProjects(listed);
          setError(null);
        }
      } catch (reason) {
        if (alive) {
          setProjects([]);
          setError(reason instanceof Error ? reason.message : 'Project groups could not be loaded.');
        }
      }
    };
    void load();
    const timer = window.setInterval(() => { void load(); }, 30_000);
    return () => { alive = false; window.clearInterval(timer); };
  }, [enabled, key]);
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
