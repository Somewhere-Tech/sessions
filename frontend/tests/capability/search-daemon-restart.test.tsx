// CAPABILITY: searching while this Mac's daemon is restarting says so and keeps
// asking, rather than reporting the restart as a failure or as an empty history.
//
// An install restarts sessionsd for a few minutes. The app stays open and a
// person may search in that window. What they saw was WebKit's own words for a
// refused connection — "Load failed" — and nothing happening until they hit
// Refresh by hand.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { SearchView } from '../../src/components/SearchView';
import { ConnectionStatus } from '../../src/components/ConnectionStatus';
import { useSessions } from '../../src/store/sessions';
import { AuthError, DaemonResponseError } from '../../src/api/sessionsd';
import { classifyPeerFailure, RESTART_RETRY_WINDOW_MS } from '../../src/lib/fleetPeerBudget';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from './fake-daemon';

function localMachine(): FakeMachine {
  return {
    id: 'local', name: 'This Mac', host: 'localhost', port: 8787, isDefault: true,
    sessions: [makeSession({ id: 'drafts', name: 'Drafts rollout' })],
    searchCorpus: [{
      sessionId: 'drafts', name: 'Drafts rollout', tool: 'claude', role: 'user',
      text: 'the drafts rollout should ship behind a flag first'
    }]
  };
}

// A restarting daemon refuses the connection outright, the way a closed port
// does, until it is listening again.
function restartingFleet(): { machines: FakeMachine[]; comeBack: () => void } {
  const machine = localMachine();
  machine.reachable = false;
  return { machines: [machine], comeBack: () => { machine.reachable = true; } };
}

async function searchFor(user: ReturnType<typeof userEvent.setup>, text: string): Promise<void> {
  await user.type(await screen.findByPlaceholderText(/Search a chat title/), text);
  await user.click(screen.getByRole('button', { name: 'Search' }));
}

afterEach(() => { vi.useRealTimers(); });

describe('capability: search during a daemon restart', () => {
  it('says the machine is restarting, keeps asking, and loads when it answers', async () => {
    const { machines, comeBack } = restartingFleet();
    installFakeDaemon(machines);
    useFakeMachines(machines, 'local');
    const user = userEvent.setup();
    render(<SearchView onResumeConversation={async () => {}} />);
    await searchFor(user, 'rollout');

    // Not "Load failed", and not a failure the person has to act on.
    const notice = await screen.findByText(/Sessions is restarting on This Mac/);
    expect(notice).toHaveTextContent('retrying');
    expect(screen.queryByText(/Load failed/)).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Try again' })).not.toBeInTheDocument();
    // Silence is not an empty history.
    expect(screen.queryByText('No matching conversations.')).not.toBeInTheDocument();

    // The daemon comes back. Nobody presses anything.
    comeBack();
    expect(await screen.findByText('Drafts rollout', {}, { timeout: 10_000 })).toBeInTheDocument();
    expect(screen.queryByText(/Sessions is restarting/)).not.toBeInTheDocument();
  }, 20_000);

  it('stops calling it a restart once the window is over', async () => {
    const { machines } = restartingFleet();
    installFakeDaemon(machines);
    useFakeMachines(machines, 'local');
    const user = userEvent.setup();
    render(<SearchView onResumeConversation={async () => {}} />);
    await searchFor(user, 'rollout');
    await screen.findByText(/Sessions is restarting on This Mac/);

    // Past the window this is a machine that is not running, which is what the
    // connection banner has always said, and the person gets the button back.
    vi.setSystemTime(Date.now() + RESTART_RETRY_WINDOW_MS + 1_000);
    await waitFor(
      () => expect(screen.getByText(/sessionsd is not responding on This Mac/)).toBeInTheDocument(),
      { timeout: 10_000 }
    );
    expect(await screen.findByRole('button', { name: 'Try again' })).toBeEnabled();
    // Still not an empty history.
    expect(screen.queryByText('No matching conversations.')).not.toBeInTheDocument();
    vi.useRealTimers();
  }, 20_000);

  it('does not call an answered failure a restart', async () => {
    const machine = localMachine();
    // The daemon is running and says so — it just cannot serve this read.
    machine.searchFailure = { status: 500, message: 'search index is rebuilding' };
    installFakeDaemon([machine]);
    useFakeMachines([machine], 'local');
    const user = userEvent.setup();
    render(<SearchView onResumeConversation={async () => {}} />);
    await searchFor(user, 'rollout');

    // What the daemon said, not what this screen guessed.
    const notice = await screen.findByText(/search index is rebuilding/);
    expect(notice).toHaveTextContent('sessionsd 500');
    expect(screen.queryByText(/Sessions is restarting/)).not.toBeInTheDocument();
    // A machine that answers is not polled every two seconds; the person decides.
    expect(await screen.findByRole('button', { name: 'Try again' })).toBeEnabled();
  }, 20_000);

  it('keeps every answered local failure out of the restart state', () => {
    // These reach classifyPeerFailure from readServerForSearch's catch. Only a
    // rejected connection is a restart; each of these is a daemon that replied.
    const answered: unknown[] = [
      new DaemonResponseError(500, '{"error":"search index is rebuilding"}', 'Internal Server Error'),
      new DaemonResponseError(409, '{"error":"session is paused","code":"SESSION_NEEDS_RECREATE"}', 'Conflict'),
      new Error('Smart features is not available on this runtime. Update Sessions or connect to a current sessionsd.'),
      new AuthError()
    ];
    for (const reason of answered) {
      expect(classifyPeerFailure(reason, true)).toBe('unreachable');
    }
    expect(classifyPeerFailure(new TypeError('Load failed'), true)).toBe('restarting');
    // The same refusal from a peer is still just a peer that is not there.
    expect(classifyPeerFailure(new TypeError('Load failed'), false)).toBe('unreachable');
  });

  it('agrees with the header about the same machine at the same moment', async () => {
    const { machines } = restartingFleet();
    installFakeDaemon(machines);
    useFakeMachines(machines, 'local');
    const user = userEvent.setup();
    // The header indicator App renders, reading the same store the app does.
    function Header(): JSX.Element {
      const hydrated = useSessions((state) => state.hydrated);
      const error = useSessions((state) => state.error);
      return <ConnectionStatus machine="This Mac" hydrated={hydrated} error={error} />;
    }
    render(<><Header /><SearchView onResumeConversation={async () => {}} /></>);
    // The refusal the header sees is the refusal Search sees: one daemon, one moment.
    await act(async () => { await useSessions.getState().refresh(); });
    await searchFor(user, 'rollout');

    await screen.findByText(/Sessions is restarting on This Mac/);
    // The header does not present the machine as connected while Search waits
    // on it, and Search does not claim a complete result set while the header
    // says the machine is not answering.
    expect(screen.getByText(/Can.t reach This Mac/)).toBeInTheDocument();
    expect(screen.queryByText('No matching conversations.')).not.toBeInTheDocument();
  }, 20_000);
});
