// CAPABILITY: a person with more than one machine can see what is running on
// each of them, and tell which is which.
//
// This one has never had a real test. scripts/surface-truth-fixture.tsx mounts
// FleetView, but its window.fetch answers ONE canned `{ sessions }` to every
// /api/sessions request regardless of which machine asked (line 142 of that
// file). Under that fake, a Fleet that silently showed the same machine twice —
// or that dropped a machine entirely — passed. Here the two machines have
// different sessions on different origins, so mixing them up is a failure.
import { describe, expect, it } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import { FleetView } from '../../src/components/FleetView';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from './fake-daemon';

function fleet(): FakeMachine[] {
  return [
    {
      id: 'alpha',
      name: 'Alpha',
      host: '10.0.0.5',
      port: 8787,
      machineId: 'machine-alpha',
      sessions: [makeSession({ id: 'alpha-1', name: 'Compiling the runtime' })]
    },
    {
      id: 'beta',
      name: 'Beta',
      host: '10.0.0.6',
      port: 8787,
      machineId: 'machine-beta',
      sessions: [makeSession({ id: 'beta-1', name: 'Rebuilding the index' })]
    }
  ];
}

function cardFor(name: string): HTMLElement {
  const heading = screen.getByRole('heading', { name, level: 2 });
  const card = heading.closest('section');
  if (!card) throw new Error(`no machine card around the heading "${name}"`);
  return card;
}

describe('capability: browse the fleet', () => {
  it('shows each machine with its own sessions, not another machine\'s', async () => {
    const machines = fleet();
    const daemon = installFakeDaemon(machines);
    useFakeMachines(machines, 'alpha');

    render(<FleetView onOpenSession={() => {}} onOpenMachine={() => {}} />);

    expect(screen.getByRole('button', { name: /Find machines in Sessions\.app/ })).toBeDisabled();
    expect(screen.getByText(/Open Sessions\.app › Settings › Fleet/)).toBeVisible();

    await waitFor(() => expect(screen.getByRole('heading', { name: 'Alpha', level: 2 })).toBeInTheDocument());
    await waitFor(() => expect(screen.getByRole('heading', { name: 'Beta', level: 2 })).toBeInTheDocument());

    // Each machine was actually asked, at its own address.
    await waitFor(() => {
      expect(daemon.requests.some((r) => r.origin === 'http://10.0.0.5:8787' && r.path === '/api/sessions')).toBe(true);
      expect(daemon.requests.some((r) => r.origin === 'http://10.0.0.6:8787' && r.path === '/api/sessions')).toBe(true);
    });

    // And each machine's card lists its own work, and only its own.
    await waitFor(() => {
      expect(within(cardFor('Alpha')).getByText('Compiling the runtime')).toBeInTheDocument();
    });
    expect(within(cardFor('Alpha')).queryByText('Rebuilding the index')).not.toBeInTheDocument();

    await waitFor(() => {
      expect(within(cardFor('Beta')).getByText('Rebuilding the index')).toBeInTheDocument();
    });
    expect(within(cardFor('Beta')).queryByText('Compiling the runtime')).not.toBeInTheDocument();
  });

  it('names the machine it could not reach instead of hiding it', async () => {
    const machines = fleet();
    machines[1].reachable = false;
    installFakeDaemon(machines);
    useFakeMachines(machines, 'alpha');

    render(<FleetView onOpenSession={() => {}} onOpenMachine={() => {}} />);

    // The unreachable machine is still listed — a machine that vanishes from
    // the fleet reads as "I have one computer", which is a lie.
    await waitFor(() => {
      expect(within(cardFor('Beta')).getByText('unreachable')).toBeInTheDocument();
    });
    expect(within(cardFor('Beta')).getByText('Session data unavailable')).toBeInTheDocument();
    // The reachable one is unaffected.
    await waitFor(() => {
      expect(within(cardFor('Alpha')).getByText('Compiling the runtime')).toBeInTheDocument();
    });
  });
});
