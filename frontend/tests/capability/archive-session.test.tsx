// CAPABILITY: a person can archive a finished session out of their list.
//
// Archiving is a list-visibility operation. runtime/internal/session/retention.go
// says so in its own words: it "removes it from retained list surfaces while
// leaving the append-only lifecycle and transcript evidence intact."
//
// The daemon records archive intent in its append-only ledger, so a parent row
// can leave the inbox without deleting its child's record or either transcript.
import { describe, expect, it } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ENDED_GROUP, Workbench } from './harness';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from './fake-daemon';

const ENDED = {
  exited: true,
  exitCode: 0,
  exitedAt: Date.now() - 30 * 60_000,
  lastDataAt: Date.now() - 30 * 60_000,
  exitReason: 'ended-by-user'
};

function localMachine(): FakeMachine {
  return {
    id: 'local',
    name: 'Fixture Mac',
    host: 'localhost',
    port: 8787,
    isDefault: true,
    sessions: [
      // Live work, so the Ended group starts collapsed the way it does for a
      // person who still has something running.
      makeSession({ id: 'still-going', name: 'This week', working: true }),
      makeSession({ id: 'finished-parent', name: 'Friday release prep', ...ENDED }),
      // One delegated child, also finished. Nothing about this record is at
      // risk from archiving its parent's row.
      makeSession({
        id: 'finished-child',
        name: 'Regenerate the changelog',
        parentSessionId: 'finished-parent',
        delegationKind: 'user',
        ...ENDED
      })
    ]
  };
}

describe('capability: archive from the list', () => {
  it('removes the finished session from the list the person is looking at', async () => {
    const machine = localMachine();
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine]);
    const user = userEvent.setup();

    render(<Workbench />);

    // Ended is collapsed by default; a person opens it to find finished work.
    await user.click(await screen.findByRole('button', { name: ENDED_GROUP }));

    await user.click(await screen.findByRole('button', { name: 'Actions for Friday release prep' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Archive from list' }));

    // The daemon archived it…
    await waitFor(() => expect(daemon.archived).toContain('finished-parent'));

    // …and it is off the list. Nothing was refused, so nothing is reported.
    await waitFor(() => expect(screen.queryByText('Friday release prep')).not.toBeInTheDocument());
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();

    // Its child's record is untouched: archiving hides a row, it does not
    // delete history.
    expect(daemon.session('finished-child')).toBeDefined();
  });
});
