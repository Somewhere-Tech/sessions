// CAPABILITY: a person can rename a session and the new name is what they see.
//
// The failure this guards against is the shipped one: a session marked renamed
// while the old name is still on screen. So the assertion is the name, after
// the round trip, on the surface the person renamed it from — and the daemon's
// stored name, which is what every other surface will read next.
import { describe, expect, it } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useSessions } from '../../src/store/sessions';
import { SessionTabs } from '../../src/components/SessionTabs';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from './fake-daemon';

const SESSION_ID = 'to-rename';

function localMachine(): FakeMachine {
  return {
    id: 'local',
    name: 'Fixture Mac',
    host: 'localhost',
    port: 8787,
    isDefault: true,
    sessions: [makeSession({ id: SESSION_ID, name: 'Untitled session' })]
  };
}

function Tabs(): JSX.Element {
  const sessions = useSessions((state) => state.sessions);
  return (
    <SessionTabs
      sessions={sessions}
      activeId={SESSION_ID}
      statusBySession={{ [SESSION_ID]: 'idle' }}
      iconBySession={{ [SESSION_ID]: '🟠' }}
      onSwitch={() => {}}
      onAdd={() => {}}
      onClose={() => {}}
    />
  );
}

describe('capability: rename a session', () => {
  it('stores the new name and shows it instead of the old one', async () => {
    const machine = localMachine();
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine]);
    const user = userEvent.setup();

    render(<Tabs />);

    await user.dblClick(await screen.findByText('Untitled session'));
    const field = await screen.findByDisplayValue('Untitled session');
    await user.clear(field);
    await user.type(field, 'Release notes');
    await user.keyboard('{Enter}');

    // The daemon holds the new name…
    await waitFor(() => expect(daemon.session(SESSION_ID)?.name).toBe('Release notes'));

    // …and so does the screen. The old name is gone, not merely covered.
    expect(await screen.findByText('Release notes')).toBeInTheDocument();
    expect(screen.queryByText('Untitled session')).not.toBeInTheDocument();
  });
});
