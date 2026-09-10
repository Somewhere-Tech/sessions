// CAPABILITY: Usage says what it is waiting for.
//
// Reported from the native app (history-fast, item 3): while a fleet's reports
// were still in flight, Usage listed the machines as "Unavailable" and told the
// person to update older machines for exact deduplication — both before a
// single report had arrived. Neither was known yet.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { UsageDashboard } from '../../src/components/UsageDashboard';
import { installFakeDaemon, useFakeMachines, type FakeMachine } from './fake-daemon';

// Each test gets its own machine ids and ports: the usage cache is keyed by
// server and lives for the module, so a shared id would hand one test's answer
// to the next one before it asked.
let fleetSeq = 0;
function fleet(): FakeMachine[] {
  fleetSeq += 1;
  return [
    { id: `local-${fleetSeq}`, name: 'This Mac', host: 'localhost', port: 8787 + fleetSeq, isDefault: true, sessions: [] },
    { id: `mini-${fleetSeq}`, name: 'Mac mini', host: `10.0.0.${fleetSeq}`, port: 8787, sessions: [] }
  ];
}

// One machine answers its usage report late; the other never answers, the way a
// machine mid-install holds a connection open.
function slowFleet(machines: FakeMachine[], lateMS: number): void {
  const realFetch = globalThis.fetch;
  const never = new Promise<never>(() => {});
  const silent = machines[1]!.host;
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (url.includes('/api/usage')) {
      if (url.includes(silent)) await never;
      await new Promise((resolve) => setTimeout(resolve, lateMS));
    }
    return realFetch(input, init);
  }) as typeof fetch;
}

afterEach(() => { vi.useRealTimers(); });

describe('capability: Usage while its reports are still in flight', () => {
  it('says which machines it is still waiting for, and claims nothing else', async () => {
    const machines = fleet();
    installFakeDaemon(machines);
    useFakeMachines(machines, 'local');
    slowFleet(machines, 3_000);
    render(<UsageDashboard />);

    // At once, and a second later: both machines are pending, and that is all
    // the screen knows about them.
    for (const moment of ['immediately', 'after a second']) {
      await waitFor(() => expect(screen.getByText(/Loading from/)).toBeInTheDocument());
      const status = screen.getByText(/Loading from/).textContent ?? '';
      expect(status, moment).toContain('This Mac');
      expect(status, moment).toContain('Mac mini');
      expect(screen.queryByText(/Unavailable:/), moment).not.toBeInTheDocument();
      expect(screen.queryByText(/update older machines/), moment).not.toBeInTheDocument();
      expect(screen.queryByText(/did not answer in time/), moment).not.toBeInTheDocument();
      await new Promise((resolve) => setTimeout(resolve, 1_000));
    }
  }, 20_000);

  it('describes an older-host answer only once that answer is in hand', async () => {
    const machines = fleet();
    installFakeDaemon(machines);
    useFakeMachines(machines, 'local');
    slowFleet(machines, 3_000);
    render(<UsageDashboard />);

    expect(screen.queryByText(/deduplicat/i)).not.toBeInTheDocument();
    // The fixture's daemon answers in the older shape: a report with no events.
    await waitFor(() => expect(screen.getByText(/deduplicat/i)).toBeInTheDocument(), { timeout: 10_000 });
    expect(screen.getByText(/deduplicat/i).textContent).toContain('This Mac');
    expect(screen.getByText(/Loading from/).textContent).toContain('Mac mini');
  }, 20_000);

  it('calls a machine that never answered timed out, and offers to ask again', async () => {
    const machines = fleet();
    installFakeDaemon(machines);
    useFakeMachines(machines, 'local');
    slowFleet(machines, 3_000);
    render(<UsageDashboard />);

    await waitFor(
      () => expect(screen.getByText(/did not answer in time/)).toBeInTheDocument(),
      { timeout: 15_000 }
    );
    expect(screen.getByText(/did not answer in time/).textContent).toContain('Mac mini');
    expect(screen.getByRole('button', { name: 'Try again' })).toBeEnabled();
    expect(screen.queryByText(/Loading from/)).not.toBeInTheDocument();
    // And the screen stops calling itself busy: before this, one machine that
    // never answered left Refresh reading "Indexing…" for as long as the view
    // stayed open.
    expect(screen.getByRole('button', { name: 'Refresh' })).toBeEnabled();
  }, 25_000);

  it('says how much of the fleet the headline covers while any of it is missing', async () => {
    const machines = fleet();
    installFakeDaemon(machines);
    useFakeMachines(machines, machines[0]!.id);
    slowFleet(machines, 3_000);
    render(<UsageDashboard />);

    // The cost headline, not only the machine counter above it: a total that
    // covers one machine out of two must say so where the number is read.
    const cost = await screen.findByText(/not your bill/, {}, { timeout: 10_000 });
    expect(cost.textContent).toContain('1 of 2 machines');
    // The partial-cost sentence is untouched by the coverage clause.
    expect(cost.textContent).toMatch(/not your bill/);
    expect(screen.getByText('1 of 2 machines reporting')).toBeInTheDocument();
  }, 20_000);
});
