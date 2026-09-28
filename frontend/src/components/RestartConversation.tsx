import { lazy, Suspense } from 'react';
import { create } from 'zustand';
import type { SessionInfo } from '../types';
import { getActiveServer } from '../lib/servers';

export interface RestartConversationProps {
  session: SessionInfo;
  onOpen: (id: string) => void;
  appearance?: 'menuitem' | 'toolbar';
  serverId?: string;
  initialRemoteControl?: boolean;
  initialRuntimeMode?: 'rich' | 'terminal';
}

const RestartConversationDialog = lazy(() => import('./RestartConversationDialog').then((module) => ({ default: module.RestartConversationDialog })));

const useRestartDialog = create<{ request: RestartConversationProps | null }>(() => ({ request: null }));

export function reviewConversationRestart(props: RestartConversationProps): void {
  useRestartDialog.setState({ request: { ...props, serverId: props.serverId ?? getActiveServer().id } });
}

export function RestartConversation(props: RestartConversationProps): JSX.Element | null {
  const { session, appearance = 'toolbar' } = props;
  if (session.exited || session.runnerGone || !['claude-code', 'codex'].includes(session.tool)) return null;
  return <button type="button" role={appearance === 'menuitem' ? 'menuitem' : undefined}
    className={appearance === 'toolbar' ? 'view-toggle-btn' : undefined}
    onClick={() => reviewConversationRestart(props)}>Restart / change permissions…</button>;
}

// The host outlives the source row/tab when that runtime ends during the request.
export function RestartConversationHost(): JSX.Element | null {
  const request = useRestartDialog((state) => state.request);
  return request ? <Suspense fallback={null}><RestartConversationDialog key={request.session.id} {...request} onClose={() => useRestartDialog.setState({ request: null })} /></Suspense> : null;
}
