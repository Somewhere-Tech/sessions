import { useCallback, useEffect, useState } from 'react';
import {
  decideMachineAccessRequest,
  listMachineAccessRequests,
  type MachineAccessRequest
} from '../api/sessionsd';
import { useServers } from '../lib/servers';
import { isTauri } from '../lib/tauriBridge';

export function TailnetAccessInbox(): JSX.Element | null {
  const [requests, setRequests] = useState<MachineAccessRequest[]>([]);
  const [busy, setBusy] = useState<string | null>(null);
  const [unavailable, setUnavailable] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const localServer = useServers((state) =>
    state.servers.find((server) => server.isDefault && server.host === 'localhost')
      ?? state.servers.find((server) => server.isDefault)
  );

  const refresh = useCallback(async (): Promise<void> => {
    if (!isTauri() || unavailable || !localServer) return;
    try {
      const next = await listMachineAccessRequests(localServer);
      if (next === null) {
        setUnavailable(true);
        setRequests([]);
        return;
      }
      setRequests(next);
    } catch {
      // The main connection surface owns authentication and reachability
      // errors. A background approval poll should never obscure it.
    }
  }, [localServer, unavailable]);

  useEffect(() => {
    void refresh();
    const interval = window.setInterval(() => { void refresh(); }, 2000);
    return () => window.clearInterval(interval);
  }, [refresh]);

  const decide = async (
    request: MachineAccessRequest,
    decision: 'accept' | 'deny'
  ): Promise<void> => {
    if (busy) return;
    setBusy(request.request_id);
    setError(null);
    try {
      if (!localServer) return;
      await decideMachineAccessRequest(request.request_id, decision, localServer);
      setRequests((current) => current.filter((item) => item.request_id !== request.request_id));
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setBusy(null);
      void refresh();
    }
  };

  if (!isTauri() || requests.length === 0) return null;

  return (
    <aside className="tailnet-access-inbox" aria-live="polite" aria-label="Machine access requests">
      {error ? <div className="tailnet-access-error">{error}</div> : null}
      {requests.map((request) => (
        <article key={request.request_id} className="tailnet-access-request">
          <div className="tailnet-access-request-mark" aria-hidden="true">↗</div>
          <div>
            <strong>
              {request.transport === 'nearby'
                ? `${request.name} wants to connect`
                : `${request.user_name || request.login} wants to connect`}
            </strong>
            <span>
              {request.transport === 'nearby'
                ? `Nearby private-network address: ${request.address ?? 'unknown'}`
                : `${request.user_name ? `${request.login} · ` : ''}Self-reported device: ${request.name}`}
            </span>
            <small>
              {request.transport === 'nearby'
                ? 'Only accept a device you recognize on this trusted network. Nearby traffic is not encrypted.'
                : 'Only accept a device you recognize on your tailnet.'}
            </small>
          </div>
          <div className="tailnet-access-request-actions">
            <button
              type="button"
              className="btn btn-ghost"
              disabled={busy !== null}
              onClick={() => void decide(request, 'deny')}
            >
              Deny
            </button>
            <button
              type="button"
              className="btn"
              disabled={busy !== null}
              onClick={() => void decide(request, 'accept')}
            >
              {busy === request.request_id ? 'Responding…' : 'Accept'}
            </button>
          </div>
        </article>
      ))}
    </aside>
  );
}
