import type { SessionInfo } from '../types';

export type WindowScope =
  | { kind: 'server'; value: string }
  | { kind: 'tool'; value: 'codex' | 'claude' | 'shell' }
  | { kind: 'session'; value: string }
  | null;

export function readWindowScope(search = typeof window === 'undefined' ? '' : window.location.search): WindowScope {
  const params = new URLSearchParams(search);
  const sessionId = params.get('session')?.trim();
  if (params.get('mode') === 'single' && sessionId) {
    return { kind: 'session', value: sessionId };
  }

  const serverId = params.get('server')?.trim();
  if (serverId) return { kind: 'server', value: serverId };

  const tool = params.get('tool');
  if (tool === 'codex' || tool === 'claude' || tool === 'shell') {
    return { kind: 'tool', value: tool };
  }
  return null;
}

export function sessionMatchesWindowScope(
  session: SessionInfo,
  scope = readWindowScope()
): boolean {
  if (!scope || scope.kind !== 'tool') return true;
  if (scope.value === 'claude') return session.tool === 'claude-code';
  if (scope.value === 'shell') return session.tool === 'terminal';
  return session.tool === 'codex';
}

export function filterSessionsForWindow(
  sessions: SessionInfo[],
  scope = readWindowScope()
): SessionInfo[] {
  if (!scope || scope.kind !== 'tool') return sessions;
  return sessions.filter((session) => sessionMatchesWindowScope(session, scope));
}
