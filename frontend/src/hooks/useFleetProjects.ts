import { useEffect, useState } from 'react';
import { fetchProjects, type ProjectView } from '../api/sessionsd';
import type { ServerConfig } from '../lib/servers';

export function useFleetProjects(servers: ServerConfig[], enabled: boolean): { projects: Record<string, ProjectView[]>; incomplete: boolean } {
  const [projects, setProjects] = useState<Record<string, ProjectView[]>>({});
  const [errors, setErrors] = useState<Record<string, boolean>>({});
  useEffect(() => {
    if (!enabled) return;
    let stopped = false;
    const controllers = new Set<AbortController>();
    const timers = new Set<number>();
    for (const server of servers) {
      const poll = async (): Promise<void> => {
        const controller = new AbortController();
        controllers.add(controller);
        const deadline = window.setTimeout(() => controller.abort(), 8_000);
        try {
          const rows = await fetchProjects(controller.signal, server);
          if (!stopped) {
            setProjects((current) => ({ ...current, [server.id]: rows }));
            setErrors((current) => ({ ...current, [server.id]: false }));
          }
        } catch {
          if (!stopped) setErrors((current) => ({ ...current, [server.id]: true }));
        }
        finally {
          controllers.delete(controller);
          window.clearTimeout(deadline);
          if (!stopped) {
            const timer = window.setTimeout(() => { timers.delete(timer); void poll(); }, 30_000);
            timers.add(timer);
          }
        }
      };
      void poll();
    }
    return () => { stopped = true; controllers.forEach((c) => c.abort()); timers.forEach(window.clearTimeout); };
  }, [servers, enabled]);
  return { projects, incomplete: servers.some((server) => errors[server.id]) };
}
