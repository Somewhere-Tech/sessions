// CAPABILITY: the budget that keeps one machine from holding a fleet-wide read
// works on the WebViews these phones actually ship.
//
// AbortSignal.any needs WebKit 17.4, newer than the iOS this app is built
// against. On a WebView without it this helper would throw where a read starts,
// and the search screen would fail whole instead of one machine being late.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { abortBudget } from '../../src/lib/abortBudget';
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
    const source = abortBudget.toString();
    expect(source).not.toContain('AbortSignal.any');
    expect(source).not.toContain('AbortSignal.timeout');
  });

  // The guard that matters is the tree, not this one helper: any read written
  // tomorrow with AbortSignal.any would break the same phones, silently, and
  // pass every test that runs in jsdom because jsdom has it.
  it('leaves no call to either API anywhere the app ships', () => {
    const offenders: string[] = [];
    const walk = (directory: string): void => {
      for (const entry of readdirSync(directory, { withFileTypes: true })) {
        const path = join(directory, entry.name);
        if (entry.isDirectory()) { walk(path); continue; }
        if (!/\.(ts|tsx)$/.test(entry.name)) continue;
        const source = readFileSync(path, 'utf8');
        // The call, not the name: the helper's own comment explains why these
        // are avoided and must not be mistaken for a use of them.
        if (source.includes('AbortSignal.any(') || source.includes('AbortSignal.timeout(')) {
          offenders.push(path);
        }
      }
    };
    walk(join(process.cwd(), 'src'));
    expect(offenders).toEqual([]);
  });

  // Each caller's own budget and error, unchanged by the move to one helper.
  it('keeps each existing budget and the error name its caller sees', async () => {
    vi.useFakeTimers();
    const hostFleet = abortBudget(10_000);
    const transportProbe = abortBudget(5_000);

    await vi.advanceTimersByTimeAsync(4_999);
    expect(transportProbe.signal.aborted).toBe(false);
    await vi.advanceTimersByTimeAsync(1);
    expect(transportProbe.signal.aborted).toBe(true);
    expect((transportProbe.signal.reason as DOMException).name).toBe('TimeoutError');

    expect(hostFleet.signal.aborted).toBe(false);
    await vi.advanceTimersByTimeAsync(5_000);
    expect(hostFleet.signal.aborted).toBe(true);
    expect((hostFleet.signal.reason as DOMException).name).toBe('TimeoutError');
  });

  // A budget with no caller behind it is what selectTransport uses.
  it('works without a caller signal at all', async () => {
    vi.useFakeTimers();
    const budget = abortBudget(1_000);
    expect(budget.signal.aborted).toBe(false);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(budget.signal.aborted).toBe(true);
    budget.release();
    expect(vi.getTimerCount()).toBe(0);
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
