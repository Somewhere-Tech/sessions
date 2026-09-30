// CAPABILITY: a client paired with one host can tell WHY one inherited machine
// is unavailable, while it keeps using the healthy ones. The host already
// explains itself — "no saved address this host can use" — and the client used
// to throw that sentence away and render a bare "unreachable".
import { beforeEach, describe, expect, it } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import { FleetRelaySync } from '../../src/components/FleetRelaySync';
import { FleetView } from '../../src/components/FleetView';
import { httpBaseForServer } from '../../src/api/sessionsd/core';
import { useServers } from '../../src/lib/servers';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeDaemon, type FakeMachine } from './fake-daemon';

const UNUSABLE_REASON =
  'no saved address this host can use: saved machine "machine-d" has an address this host cannot use as a tailnet route';

function pairedHostFleet(): FakeMachine[] {
  return [
    {
      id: 'host-a',
      name: 'Mac A',
      host: '10.0.0.5',
      port: 8787,
      machineId: 'machine-a',
      isDefault: true,
      sessions: [makeSession({ id: 'a-1', name: 'Compiling the runtime' })],
      fleetPeers: [
        { id: 'machine-b', name: 'Mac B', transport: 'lan', reachable: true },
        {
          id: 'machine-d', name: 'Mac D', transport: 'tailnet', reachable: false,
          reason: 'saved-endpoint-unusable', message: UNUSABLE_REASON,
          relayFailure: { status: 502, error: UNUSABLE_REASON, reason: 'saved-endpoint-unusable' }
        }
      ]
    },
    {
      // Mac B is reached only through Mac A: the client never holds its
      // credential and never dials it directly.
      id: 'peer-b',
      name: 'Mac B',
      host: '10.0.0.6',
      port: 8787,
      machineId: 'machine-b',
      sessions: [makeSession({ id: 'b-1', name: 'Rebuilding the index' })]
    },
    {
      // Mac D is running the whole time. What fails is Mac A's saved address
      // for it, which is why the explanation has to come from Mac A.
      id: 'peer-d',
      name: 'Mac D',
      host: '10.0.0.7',
      port: 8787,
      machineId: 'machine-d',
      sessions: [makeSession({ id: 'd-1', name: 'Reindexing history' })]
    }
  ];
}

function hostOnly(machines: FakeMachine[]): FakeMachine[] {
  return machines.filter((machine) => machine.id === 'host-a');
}

// The card's explanation slot: the same element that already reports a failed
// session refresh, so no new surface was invented to carry this.
function explanationOn(card: HTMLElement): string {
  return card.querySelector('.fleet-session-error')?.textContent ?? '';
}

function cardFor(name: string): HTMLElement {
  const heading = screen.getByRole('heading', { name, level: 2 });
  const card = heading.closest('section');
  if (!card) throw new Error(`no machine card around the heading "${name}"`);
  return card;
}

function renderInheritedFleet(machines: FakeMachine[]): FakeDaemon {
  const daemon = installFakeDaemon(machines);
  useFakeMachines(hostOnly(machines), 'host-a');
  render(<><FleetRelaySync /><FleetView onOpenSession={() => {}} onOpenMachine={() => {}} /></>);
  return daemon;
}

describe('capability: understand why one inherited machine is unavailable', () => {
  beforeEach(() => {
    window.localStorage.clear();
    useServers.setState({
      servers: [], activeId: '', pairingError: null, credentialError: null, tokenRequiredServerId: null
    });
  });

  it('explains the unusable peer, keeps the healthy one working, and adds no authority', async () => {
    const machines = pairedHostFleet();
    const daemon = renderInheritedFleet(machines);

    // Both inherited machines appear once, through the paired host.
    await waitFor(() => expect(cardFor('Mac D')).toBeInTheDocument());
    const inherited = useServers.getState().servers.filter((server) => server.relayMachineId);
    expect(inherited.map((server) => server.machineId)).toEqual(['machine-b', 'machine-d']);

    // The host's own explanation is on the card, not a bare "unreachable".
    await waitFor(() => expect(explanationOn(cardFor('Mac D'))).toContain(UNUSABLE_REASON));
    expect(within(cardFor('Mac D')).getByText('unreachable')).toBeVisible();

    // The healthy neighbour is reachable and its sessions load, over the relay.
    await waitFor(() => expect(within(cardFor('Mac B')).getByText('Rebuilding the index')).toBeVisible());
    expect(within(cardFor('Mac B')).getByText('reachable')).toBeVisible();
    expect(explanationOn(cardFor('Mac B'))).toBe('');

    // Nothing gained authority: every inherited request went to the host's
    // origin under its relay prefix, carrying the host's own credential.
    const peer = inherited.find((server) => server.machineId === 'machine-b')!;
    expect(httpBaseForServer(peer)).toBe('http://10.0.0.5:8787/api/fleet/machine-b');
    expect(peer.token).toBe(useServers.getState().servers.find((server) => server.isDefault)?.token);
    const direct = daemon.requests.filter((request) => request.origin !== 'http://10.0.0.5:8787' && !request.relayedFrom);
    expect(direct).toEqual([]);
  });

  it('clears the stale explanation once the peer answers again, without a duplicate entry', async () => {
    const machines = pairedHostFleet();
    renderInheritedFleet(machines);
    await waitFor(() => expect(explanationOn(cardFor('Mac D'))).toContain(UNUSABLE_REASON));

    // Mac A can use its saved address again. The observation was a snapshot of
    // one attempt, so the next successful probe has to retire it; nothing about
    // the earlier failure may stop the client from retrying.
    const host = machines.find((machine) => machine.id === 'host-a')!;
    const peer = host.fleetPeers!.find((candidate) => candidate.id === 'machine-d')!;
    peer.relayFailure = undefined;
    peer.reachable = true;
    peer.reason = undefined;
    peer.message = undefined;
    await waitFor(() => expect(within(cardFor('Mac D')).getByText('reachable')).toBeVisible(), { timeout: 10_000 });
    expect(explanationOn(cardFor('Mac D'))).toBe('');
    expect(screen.getAllByRole('heading', { name: 'Mac D', level: 2 })).toHaveLength(1);
    expect(useServers.getState().servers.filter((server) => server.machineId === 'machine-d')).toHaveLength(1);
  });
});
