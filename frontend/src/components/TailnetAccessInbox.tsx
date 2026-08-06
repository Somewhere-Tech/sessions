import { useCallback, useEffect, useRef, useState } from 'react';
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

  // The poll is cancellable and it stops. Both were missing:
  //
  //   • No abort/alive flag — a response landing after unmount, or after the
  //     local machine changed, still called setState. Every await is now
  //     gated on the run's own AbortController, and teardown aborts the
  //     request in flight rather than only ignoring its answer.
  //   • Once `unavailable` was set (this daemon has no approval endpoint) the
  //     2s interval kept ticking for the life of the app doing nothing. This
  //     component is mounted unconditionally in App.tsx, so that was forever.
  //     The effect is now keyed on `unavailable` and never starts the timer.
  const abortRef = useRef<AbortController | null>(null);

  const refresh = useCallback(async (signal?: AbortSignal): Promise<void> => {
    if (!isTauri() || !localServer) return;
    try {
      const next = await listMachineAccessRequests(localServer, signal);
      if (signal?.aborted) return;
      if (next === null) {
        setUnavailable(true);
        setRequests([]);
        return;
      }
      setRequests(next);
    } catch {
      // The main connection surface owns authentication and reachability
      // errors. A background approval poll should never obscure it — and an
      // abort during teardown is not an error at all.
    }
  }, [localServer]);

  useEffect(() => {
    if (!isTauri() || unavailable || !localServer) return;
    const controller = new AbortController();
    abortRef.current = controller;
    void refresh(controller.signal);
    const interval = window.setInterval(() => { void refresh(controller.signal); }, 2000);
    return () => {
      window.clearInterval(interval);
      controller.abort();
      if (abortRef.current === controller) abortRef.current = null;
    };
  }, [localServer, refresh, unavailable]);

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
      if (!abortRef.current?.signal.aborted) setBusy(null);
      void refresh(abortRef.current?.signal);
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
