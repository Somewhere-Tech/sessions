import { useEffect, useRef } from 'react';
import { rememberNativeMachineClaim } from '../lib/hostedBootstrap';
import type { ServerConfig } from '../lib/servers';
import {
  claimNativeMachineAccess,
  type NativeMachinePeer,
  type NativeTailnetRequest
} from '../lib/tauriBridge';

// The "waiting for the other machine to approve this device" poll.
//
// This existed three times — ConnectScreen, FleetView, ConnectionsView — as
// ~35 near-identical lines each, and the copies had already drifted apart:
// one told the user "The other machine denied this request", another "The
// other Mac denied this request", and the expiry sentences differed the same
// way. Sessions runs on more than Macs, so the machine-neutral wording is the
// one that survives; every surface now gets the same sentence because there
// is only one of it.

export type MachineAccessOutcome = 'denied' | 'expired';

export const MACHINE_ACCESS_OUTCOME_MESSAGE: Record<MachineAccessOutcome, string> = {
  denied: 'The other machine denied this request.',
  expired: 'The request expired. Send a new one when someone is at the other machine.'
};

export interface PendingMachineAccess {
  transport: NativeMachinePeer['transport'];
  request: Pick<NativeTailnetRequest, 'endpoint' | 'requestId' | 'requestSecret'>;
}

export interface MachineAccessPairingOptions {
  /** The outstanding request, or null when nothing is waiting for approval. */
  pending: PendingMachineAccess | null;
  /**
   * Whether accepting should also switch the app to the new machine. Fleet
   * adds machines without stealing the user's current selection; the connect
   * surfaces are a deliberate "connect me to that machine" action.
   */
  select?: boolean;
  /** Approved. The claim is already persisted as `server` when this runs. */
  onAccepted: (server: ServerConfig) => void;
  /** Declined or timed out. `message` is the shared sentence for `outcome`. */
  onSettled: (outcome: MachineAccessOutcome, message: string) => void;
  /** The claim call itself failed (transport, storage, native bridge). */
  onError: (message: string) => void;
  /** Poll period. Only overridden by tests. */
  intervalMs?: number;
}

/**
 * Polls a pending machine-access request until it is accepted, denied, or
 * expires. Re-entrancy is guarded (a slow claim never overlaps the next tick)
 * and every callback is gated on the effect still being current, so a reply
 * arriving after the request was cleared cannot resurrect stale UI.
 */
export function useMachineAccessPairing({
  pending,
  select = true,
  onAccepted,
  onSettled,
  onError,
  intervalMs = 2_000
}: MachineAccessPairingOptions): void {
  // Callers pass inline handlers. Keeping them in a ref rather than in the
  // dependency list means a parent re-render (a status message landing, say)
  // does not tear down and immediately restart the poll — which would fire an
  // extra claim on every keystroke in the surrounding form.
  const handlers = useRef({ onAccepted, onSettled, onError });
  handlers.current = { onAccepted, onSettled, onError };

  useEffect(() => {
    if (!pending) return;
    let cancelled = false;
    let checking = false;

    const check = async (): Promise<void> => {
      if (checking || cancelled) return;
      checking = true;
      try {
        const result = await claimNativeMachineAccess(pending.transport, pending.request);
        if (cancelled || result.status === 'pending') return;
        if (result.status === 'accepted' && result.claim) {
          const server = await rememberNativeMachineClaim(result.claim, { select });
          if (!cancelled) handlers.current.onAccepted(server);
          return;
        }
        const outcome: MachineAccessOutcome = result.status === 'denied' ? 'denied' : 'expired';
        if (!cancelled) handlers.current.onSettled(outcome, MACHINE_ACCESS_OUTCOME_MESSAGE[outcome]);
      } catch (reason) {
        if (!cancelled) {
          handlers.current.onError(reason instanceof Error ? reason.message : String(reason));
        }
      } finally {
        checking = false;
      }
    };

    void check();
    const interval = window.setInterval(() => { void check(); }, intervalMs);
    return () => {
      cancelled = true;
      window.clearInterval(interval);
    };
  }, [intervalMs, pending, select]);
}
