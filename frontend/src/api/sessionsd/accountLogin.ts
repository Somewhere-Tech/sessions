import { httpBaseForServer, json, requestedServer, serverFetch } from './core';
import type { AccountProfile } from './sessions';

export interface AccountLogin {
  id: string;
  tool: 'claude' | 'codex';
  profile: string;
  state: 'opening' | 'waiting' | 'connected' | 'failed' | 'expired' | 'cancelled';
  url?: string;
  code?: string;
  message?: string;
  identity?: AccountProfile['identity'];
  expires_at: number;
}

export async function accountLoginRequest(
  serverId: string | undefined, id: string, method = 'GET', body?: object
): Promise<AccountLogin> {
  const server = requestedServer(serverId);
  const response = await serverFetch(server, `${httpBaseForServer(server)}/api/account-logins${id ? `/${encodeURIComponent(id)}` : ''}`, {
    method,
    headers: body ? { 'content-type': 'application/json' } : undefined,
    body: body ? JSON.stringify(body) : undefined
  });
  return json<AccountLogin>(response);
}
