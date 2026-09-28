import { httpBaseForServer, json, requestedServer, serverFetch } from './core';
import type { AdoptConversationResult } from './operations';

export interface RestartResult {
  ok: boolean;
  partial?: boolean;
  sourceSessionId: string;
  sourceEnded: boolean;
  operationId: string;
  laneId?: string;
  error?: string;
  adoption?: AdoptConversationResult;
}

export async function restartConversation(sourceSessionId: string, permissions: 'constrained' | 'full', remoteControl: boolean, serverId?: string, runtimeMode?: 'rich' | 'terminal'): Promise<RestartResult> {
  const server = requestedServer(serverId);
  const response = await serverFetch(server, `${httpBaseForServer(server)}/api/recovery/restart`, {
    method: 'POST', headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ sourceSessionId, confirmSessionId: sourceSessionId, permissions, remoteControl, runtimeMode })
  });
  return json<RestartResult>(response);
}
