// What a fleet-wide read is allowed to cost, and how a machine that did not
// answer is described.
//
// A read across the fleet used to be one Promise.all: every machine's answer
// waited for the slowest, so a peer that was powered off or mid-install held
// the person's own local history for as long as its host took to give up. A
// machine that cannot be reached is a fact about that machine; it is not a
// reason to withhold the rest, and it is never absence.

import { abortBudget, type AbortBudget } from './abortBudget';

/** One peer's own budget. Past this it has not answered, which is not an error. */
export const PEER_BUDGET_MS = 5_000;

/** The whole fan-out. No machine may extend a read beyond this. */
export const FLEET_BUDGET_MS = 8_000;

/** The local machine still gets a bound, generously, because it is not a peer. */
export const LOCAL_BUDGET_MS = 30_000;

export type PeerStatus = 'answered' | 'timed-out' | 'unreachable' | 'restarting';

/** How often a machine that is restarting is asked again. */
export const RESTART_RETRY_INTERVAL_MS = 2_000;

/**
 * How long a refused local read is treated as a restart. An install restarts
 * the daemon for two to four minutes on the machine itself, but the app is
 * usually pointed at a daemon that comes back much sooner; past this window,
 * saying "restarting" would be a guess about a machine that is simply down.
 */
export const RESTART_RETRY_WINDOW_MS = 60_000;

export interface PeerReport {
  serverId: string;
  serverName: string;
  status: PeerStatus;
  detail: string | null;
}

/** A read's own signal, bounded by what that machine is allowed to cost. */
export function peerBudget(base: AbortSignal, local: boolean): AbortBudget {
  return abortBudget(local ? LOCAL_BUDGET_MS : PEER_BUDGET_MS, base);
}

/**
 * A machine that ran out of time and a machine that refused are different
 * things to a person: one may work on the next try, the other needs attention.
 * An abort raised by the budget is the first; anything else is the second.
 *
 * The machine in front of you is a third case. A refused connection to this
 * Mac, during the minutes an install restarts its daemon, is a state that ends
 * on its own — and the browser's own words for it ("Load failed" in WebKit) are
 * no help to anyone. A refused peer stays unreachable: nothing here knows that
 * another machine is mid-install, and guessing would be worse than saying what
 * happened.
 */
export function classifyPeerFailure(reason: unknown, local = false): PeerStatus {
  if (reason instanceof DOMException && reason.name === 'TimeoutError') return 'timed-out';
  if (reason instanceof DOMException && reason.name === 'AbortError') return 'timed-out';
  return local ? 'restarting' : 'unreachable';
}

export function peerReportText(report: PeerReport, retrying = false): string {
  if (report.status === 'restarting') {
    // While it is coming back, say so and keep asking. Once the window is over,
    // this is the same thing the connection banner says: sessionsd is not
    // responding, and the person has to look at the machine.
    return retrying
      ? `Sessions is restarting on ${report.serverName} — retrying…`
      : `sessionsd is not responding on ${report.serverName}. Check that it is running.`;
  }
  if (report.status === 'timed-out') {
    return `${report.serverName} did not answer in time — these results are missing its history.`;
  }
  return `${report.serverName} could not be reached${report.detail ? `: ${report.detail}` : ''}.`;
}

/** True when anything is missing, which is what a count caveat depends on. */
export function anyPeerMissing(reports: PeerReport[]): boolean {
  return reports.some((report) => report.status !== 'answered');
}
