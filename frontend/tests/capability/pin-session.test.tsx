// CAPABILITY: a person can pin and unpin a session from the row it belongs to.
//
// This is the capability whose absence this whole harness exists to catch.
// Pinning shipped with a CLI verb, an HTTP route, a store action and a details
// toggle, and no way to reach it from the list — and every test passed, because
// the tests checked the sorting function (scripts/pin-smoke.mjs) and the API
// surface, never the row.
//
// So the assertion is reachability first and behaviour second: open the menu on
// a session's own row, and the pin must be there.
import { describe, expect, it } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Workbench } from './harness';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from './fake-daemon';

function localMachine(): FakeMachine {
  return {
    id: 'local',
    name: 'Fixture Mac',
    host: 'localhost',
    port: 8787,
    isDefault: true,
    sessions: [
      makeSession({ id: 'keeper', name: 'The one I keep coming back to' }),
      makeSession({ id: 'other', name: 'Something else' })
    ]
  };
}

describe('capability: pin and unpin from the row', () => {
  it('pins from the row menu and shows the row as pinned', async () => {
    const machine = localMachine();
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine]);
    const user = userEvent.setup();

    render(<Workbench />);

    await user.click(await screen.findByRole('button', {
      name: 'Actions for The one I keep coming back to'
    }));

    // Reachability. A capability with no affordance is not shipped.
    const pin = await screen.findByRole('menuitem', { name: /^Pin\b/ });
    await user.click(pin);

    await waitFor(() => expect(daemon.session('keeper')?.pinned).toBe(true));
    expect(await screen.findByLabelText('Pinned')).toBeInTheDocument();
  });

  it('unpins the same way it pinned', async () => {
    const machine = localMachine();
    machine.sessions[0].pinned = true;
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine]);
    const user = userEvent.setup();

    render(<Workbench />);

    await user.click(await screen.findByRole('button', {
      name: 'Actions for The one I keep coming back to'
    }));
    await user.click(await screen.findByRole('menuitem', { name: /^Unpin\b/ }));

    await waitFor(() => expect(daemon.session('keeper')?.pinned).toBe(false));
    await waitFor(() => expect(screen.queryByLabelText('Pinned')).not.toBeInTheDocument());
  });
});
