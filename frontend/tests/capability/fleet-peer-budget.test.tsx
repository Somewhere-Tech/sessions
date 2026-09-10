// CAPABILITY: the budget that keeps one machine from holding a fleet-wide read
// works on the WebViews these phones actually ship.
//
// AbortSignal.any needs WebKit 17.4, newer than the iOS this app is built
// against. On a WebView without it this helper would throw where a read starts,
// and the search screen would fail whole instead of one machine being late.
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  classifyPeerFailure,
  peerBudget,
  peerReportText,
  LOCAL_BUDGET_MS,
  PEER_BUDGET_MS
} from '../../src/lib/fleetPeerBudget';

afterEach(() => { vi.useRealTimers(); });

function aborted(signal: AbortSignal): Promise<unknown> {
  return new Promise((resolve) => {
    if (signal.aborted) { resolve(signal.reason); return; }
    signal.addEventListener('abort', () => resolve(signal.reason), { once: true });
  });
}

describe('capability: a per-machine budget without a modern-WebView dependency', () => {
  it('is built from a controller, a listener and a timer', () => {
    // The helper must not reach for the two APIs the shipped WebViews lack.
    const source = peerBudget.toString();
    expect(source).not.toContain('AbortSignal.any');
    expect(source).not.toContain('AbortSignal.timeout');
  });

  it('ends the read when the caller gives up, carrying the caller reason', async () => {
    vi.useFakeTimers();
    const caller = new AbortController();
    const budget = peerBudget(caller.signal, false);
    expect(budget.signal.aborted).toBe(false);

    const stopped = aborted(budget.signal);
    caller.abort(new DOMException('unmounted', 'AbortError'));
    const reason = await stopped;

    expect(budget.signal.aborted).toBe(true);
    expect((reason as DOMException).name).toBe('AbortError');
    // The caller's own abort retires the clock too.
    expect(vi.getTimerCount()).toBe(0);
  });

  it('ends the read when the machine runs out of its budget, as a timeout', async () => {
    vi.useFakeTimers();
    const budget = peerBudget(new AbortController().signal, false);
    const stopped = aborted(budget.signal);

    await vi.advanceTimersByTimeAsync(PEER_BUDGET_MS - 1);
    expect(budget.signal.aborted).toBe(false);
    await vi.advanceTimersByTimeAsync(1);
    const reason = await stopped;

    expect((reason as DOMException).name).toBe('TimeoutError');
    // A machine that ran out of time is late, not broken.
    expect(classifyPeerFailure(reason)).toBe('timed-out');
    expect(vi.getTimerCount()).toBe(0);
  });

  it('gives the local machine its own longer budget', async () => {
    vi.useFakeTimers();
    const budget = peerBudget(new AbortController().signal, true);
    await vi.advanceTimersByTimeAsync(PEER_BUDGET_MS + 1);
    expect(budget.signal.aborted).toBe(false);
    await vi.advanceTimersByTimeAsync(LOCAL_BUDGET_MS - PEER_BUDGET_MS);
    expect(budget.signal.aborted).toBe(true);
  });

  it('stops the clock when a read finishes on its own', async () => {
    vi.useFakeTimers();
    const caller = new AbortController();
    const budget = peerBudget(caller.signal, true);
    expect(vi.getTimerCount()).toBe(1);

    budget.release();
    budget.release(); // idempotent: a read may release in more than one place

    expect(vi.getTimerCount()).toBe(0);
    await vi.advanceTimersByTimeAsync(LOCAL_BUDGET_MS * 2);
    expect(budget.signal.aborted).toBe(false);

    // And the released budget is no longer listening to the caller either.
    caller.abort();
    expect(budget.signal.aborted).toBe(false);
  });

  it('is already over when the caller had given up before the read started', () => {
    vi.useFakeTimers();
    const caller = new AbortController();
    caller.abort(new DOMException('gone', 'AbortError'));
    const budget = peerBudget(caller.signal, false);

    expect(budget.signal.aborted).toBe(true);
    expect(vi.getTimerCount()).toBe(0);
  });

  it('still tells a late machine apart from an unreachable one', () => {
    expect(classifyPeerFailure(new DOMException('slow', 'TimeoutError'))).toBe('timed-out');
    expect(classifyPeerFailure(new DOMException('gone', 'AbortError'))).toBe('timed-out');
    expect(classifyPeerFailure(new TypeError('Failed to fetch'))).toBe('unreachable');
    expect(classifyPeerFailure('not an error')).toBe('unreachable');

    expect(peerReportText({ serverId: 'mini', serverName: 'Mac mini', status: 'timed-out', detail: null }))
      .toContain('did not answer in time');
    expect(peerReportText({ serverId: 'mini', serverName: 'Mac mini', status: 'unreachable', detail: 'refused' }))
      .toContain('could not be reached: refused');
  });
});
