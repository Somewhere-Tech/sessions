// CAPABILITY: resuming picks the conversation the person is looking at, decided
// by its provider identity — never by the folder it ran in, the name it shares
// with a neighbour, or whatever else happens to be live in that workspace.
//
// This is the shape of the failure that used to cost the most: old history
// paired with an unrelated fresh runtime. Three conversations share one working
// directory here, two of them share a name, and one of them has a live
// successor. Only exact provider ids tell them apart.
import { describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { SessionHistoryView } from '../../src/components/SessionHistoryView';
import { useExactResume } from '../../src/hooks/useExactResume';
import { Workbench } from './harness';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeDaemon, type FakeMachine } from './fake-daemon';
import type { SessionInfo } from '../../src/types';

const SHARED_CWD = '/Users/example/project';
const PROVIDER_ALONE = '11111111-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const PROVIDER_NEIGHBOUR = '22222222-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
const PROVIDER_CONTINUED = '33333333-cccc-4ccc-8ccc-cccccccccccc';

const NOW = Date.now();

// Same folder, same name, different conversation: the neighbour is live and has
// nothing to do with the ended row the person opened.
const endedAlone = makeSession({
  id: 'ended-alone', name: 'Migration plan', cwd: SHARED_CWD,
  conversationId: PROVIDER_ALONE, createdAt: NOW - 7_200_000,
  exited: true, exitCode: 0, exitedAt: NOW - 3_600_000
});
const liveNeighbour = makeSession({
  id: 'live-neighbour', name: 'Migration plan', cwd: SHARED_CWD,
  conversationId: PROVIDER_NEIGHBOUR, createdAt: NOW - 1_800_000
});
const endedContinued = makeSession({
  id: 'ended-continued', name: 'Index rebuild', cwd: SHARED_CWD,
  conversationId: PROVIDER_CONTINUED, createdAt: NOW - 7_200_000,
  exited: true, exitCode: 0, exitedAt: NOW - 3_600_000
});
const liveContinuation = makeSession({
  id: 'live-continuation', name: 'Index rebuild (resumed)', cwd: SHARED_CWD,
  conversationId: PROVIDER_CONTINUED, createdAt: NOW - 1_200_000,
  resumedFrom: 'ended-continued'
});

function historyRow(session: SessionInfo, providerSessionID: string) {
  return {
    id: session.id, name: session.name ?? session.id, tool: 'claude' as const,
    provider_session_id: providerSessionID, cwd: session.cwd, machine: 'Fixture Mac',
    created_at: session.createdAt, last_activity_at: session.lastDataAt,
    message_count: 1, conversation_available: true
  };
}

function workspace(): FakeMachine {
  const sessions = [endedAlone, liveNeighbour, endedContinued, liveContinuation];
  return {
    id: 'local', name: 'Fixture Mac', host: 'localhost', port: 8787, isDefault: true,
    sessions,
    history: [
      historyRow(endedAlone, PROVIDER_ALONE),
      historyRow(liveNeighbour, PROVIDER_NEIGHBOUR),
      historyRow(endedContinued, PROVIDER_CONTINUED),
      historyRow(liveContinuation, PROVIDER_CONTINUED)
    ],
    transcripts: Object.fromEntries(sessions.map((session) => [session.id, [{
      index: 0, id: `${session.id}-message`, role: 'user' as const,
      text: `Opening line of ${session.id}`, timestamp: null
    }]]))
  };
}

function openHistory(session: SessionInfo, onOpenSession: (id: string) => void): FakeDaemon {
  const machine = workspace();
  const daemon = installFakeDaemon([machine]);
  useFakeMachines([machine]);
  function Flow(): JSX.Element {
    const { resume } = useExactResume(() => {});
    return <Workbench>
      <SessionHistoryView session={session} onResume={resume} onOpenSession={onOpenSession} />
    </Workbench>;
  }
  render(<Flow />);
  return daemon;
}

describe('capability: resume the conversation on screen, not its neighbour', () => {
  it('resumes the exact provider conversation when a different one is live in the same folder', async () => {
    const daemon = openHistory(endedAlone, () => {});
    const user = userEvent.setup();

    // A live session sharing this folder and this name is not this
    // conversation, so nothing about it is offered here.
    expect(await screen.findByText(`Opening line of ${endedAlone.id}`)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Open live continuation/ })).not.toBeInTheDocument();
    expect(screen.getByText(/read-only history/)).toBeInTheDocument();

    await user.click(await screen.findByRole('button', { name: /Resume conversation/ }));

    // Exactly the conversation on screen, named by its provider id.
    await waitFor(() => expect(daemon.adopted).toEqual([PROVIDER_ALONE]));
    expect(daemon.adopted).not.toContain(PROVIDER_NEIGHBOUR);
    expect(daemon.adopted).not.toContain(PROVIDER_CONTINUED);
  });

  it('offers the live continuation of the same conversation instead of resuming it twice', async () => {
    const opened = vi.fn();
    const daemon = openHistory(endedContinued, opened);

    expect(await screen.findByText(`Opening line of ${endedContinued.id}`)).toBeInTheDocument();
    expect(screen.getByText('Continued · live')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Resume conversation/ })).not.toBeInTheDocument();

    await userEvent.setup().click(screen.getByRole('button', { name: /Open live continuation/ }));

    // Viewing and opening, never a second runtime for a conversation that
    // already has one.
    expect(opened).toHaveBeenCalledWith('live-continuation');
    expect(daemon.adopted).toEqual([]);
  });
});
