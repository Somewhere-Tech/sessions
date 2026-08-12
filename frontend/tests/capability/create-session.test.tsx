// CAPABILITY: a person can start a new session and see it in their list.
//
// The outcome asserted is the one a person cares about: the session they asked
// for is on screen afterwards. Not "create() was called", not "the dialog
// rendered a submit button" — the row exists in the navigator, named what they
// typed, because the daemon really has it.
import { useState } from 'react';
import { describe, expect, it } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { NewSessionDialog } from '../../src/components/NewSessionDialog';
import { Workbench } from './harness';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from './fake-daemon';

function localMachine(): FakeMachine {
  return {
    id: 'local',
    name: 'Fixture Mac',
    host: 'localhost',
    port: 8787,
    isDefault: true,
    sessions: [makeSession({ id: 'existing', name: 'Already running' })],
    directories: [{ path: '/Users/example/project', label: 'project', kind: 'project' }]
  };
}

// The launcher closes itself once the session is started, exactly as App.tsx
// lets it, so the only place the new name can still be on screen is the list.
function Launcher(): JSX.Element {
  const [open, setOpen] = useState(true);
  return (
    <Workbench>
      {open ? <NewSessionDialog onClose={() => setOpen(false)} onStarted={() => {}} embedded /> : null}
    </Workbench>
  );
}

describe('capability: create a session', () => {
  it('starts the session the person described and shows it in the list', async () => {
    const machine = localMachine();
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine]);
    const user = userEvent.setup();

    render(<Launcher />);

    // The dialog cannot start anything until it has a working folder; the
    // daemon's directory list is where it comes from.
    const start = await screen.findByRole('button', { name: 'Start session' });
    await waitFor(() => expect(start).toBeEnabled());

    await user.type(
      screen.getByPlaceholderText(/Ask an agent to work/),
      'Ship the release notes'
    );
    await user.click(start);

    // The daemon really created it…
    await waitFor(() => expect(daemon.created).toHaveLength(1));
    expect(daemon.created[0].name).toBe('Ship the release notes');

    // …and the person can see it in their list.
    expect(await screen.findByText('Ship the release notes')).toBeInTheDocument();
  });

  it('creates and submits an initial request only once across duplicate gestures', async () => {
    const machine = { ...localMachine(), createDelayMS: 50 };
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine]);

    render(<Launcher />);
    const start = await screen.findByRole('button', { name: 'Start session' });
    await waitFor(() => expect(start).toBeEnabled());
    fireEvent.change(screen.getByPlaceholderText(/Ask an agent to work/), {
      target: { value: 'Inspect this repository' }
    });
    fireEvent.click(start);
    fireEvent.click(start);

    await waitFor(() => expect(daemon.created).toHaveLength(1));
    await waitFor(() => expect(daemon.delivered[daemon.created[0]!.id]).toEqual(['Inspect this repository']));
  });
});
