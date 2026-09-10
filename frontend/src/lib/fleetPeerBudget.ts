// What a fleet-wide read is allowed to cost, and how a machine that did not
// answer is described.
//
// A read across the fleet used to be one Promise.all: every machine's answer
// waited for the slowest, so a peer that was powered off or mid-install held
// the person's own local history for as long as its host took to give up. A
// machine that cannot be reached is a fact about that machine; it is not a
// reason to withhold the rest, and it is never absence.

/** One peer's own budget. Past this it has not answered, which is not an error. */
export const PEER_BUDGET_MS = 5_000;

/** The whole fan-out. No machine may extend a read beyond this. */
export const FLEET_BUDGET_MS = 8_000;

/** The local machine still gets a bound, generously, because it is not a peer. */
export const LOCAL_BUDGET_MS = 30_000;

export type PeerStatus = 'answered' | 'timed-out' | 'unreachable';

export interface PeerReport {
  serverId: string;
  serverName: string;
  status: PeerStatus;
  detail: string | null;
}

export interface PeerBudget {
  /** Aborts when the caller aborts, or when this machine's budget elapses. */
  readonly signal: AbortSignal;
  /** Stop the clock. Idempotent; call it once the read is over either way. */
  release(): void;
}

/**
 * A read's own signal, bounded by what that machine is allowed to cost.
 *
 * Written by hand rather than with AbortSignal.any and AbortSignal.timeout:
 * `any` needs WebKit 17.4, which is newer than the iOS this app is built
 * against, and a phone whose WebView lacks it would throw here and lose the
 * search screen. A controller, a listener and a timer are the same thing and
 * run everywhere. The budget aborts with a TimeoutError so a machine that ran
 * out of time stays distinguishable from one that refused.
 */
export function peerBudget(base: AbortSignal, local: boolean): PeerBudget {
  const controller = new AbortController();
  let timer: ReturnType<typeof setTimeout> | undefined;
  function release(): void {
    if (timer !== undefined) {
      clearTimeout(timer);
      timer = undefined;
    }
    base.removeEventListener('abort', onCallerAbort);
  }
  function onCallerAbort(): void {
    release();
    controller.abort(base.reason);
  }
  if (base.aborted) {
    controller.abort(base.reason);
    return { signal: controller.signal, release };
  }
  base.addEventListener('abort', onCallerAbort);
  timer = setTimeout(() => {
    timer = undefined;
    release();
    controller.abort(new DOMException('This machine did not answer inside its budget.', 'TimeoutError'));
  }, local ? LOCAL_BUDGET_MS : PEER_BUDGET_MS);
  return { signal: controller.signal, release };
}

/**
 * A machine that ran out of time and a machine that refused are different
 * things to a person: one may work on the next try, the other needs attention.
 * An abort raised by the budget is the first; anything else is the second.
 */
export function classifyPeerFailure(reason: unknown): PeerStatus {
  if (reason instanceof DOMException && reason.name === 'TimeoutError') return 'timed-out';
  if (reason instanceof DOMException && reason.name === 'AbortError') return 'timed-out';
  return 'unreachable';
}

export function peerReportText(report: PeerReport): string {
  if (report.status === 'timed-out') {
    return `${report.serverName} did not answer in time — these results are missing its history.`;
  }
  return `${report.serverName} could not be reached${report.detail ? `: ${report.detail}` : ''}.`;
}

/** True when anything is missing, which is what a count caveat depends on. */
export function anyPeerMissing(reports: PeerReport[]): boolean {
  return reports.some((report) => report.status !== 'answered');
}
