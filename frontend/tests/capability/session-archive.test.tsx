// CAPABILITY: a person can put a session away from the session itself.
//
// Archiving shipped as a daemon route, a CLI verb and a row menu item, and the
// person reading a finished conversation had no way to reach it from the thing
// they were reading. It is never automatic: nothing archives on a timer, and a
// session that went quiet stays exactly where it is.
//
// A live session cannot be archived — the daemon archives durably closed
// records only — so the control for one says what it will do first, and says
// that the work in progress stops.
import { describe, expect, it } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { SessionView } from '../../src/components/SessionView';
import { ConversationBrowser } from '../../src/components/ConversationBrowser';
import { DEFAULT_BROWSE_FILTERS } from '../../src/lib/conversationBrowser';
import { Workbench } from './harness';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from './fake-daemon';
import type { SessionInfo } from '../../src/types';

const ENDED = {
  exited: true,
  exitCode: 0,
  exitedAt: Date.now() - 20 * 60_000,
  lastDataAt: Date.now() - 20 * 60_000,
  exitReason: 'ended-by-user'
};

function machineWith(sessions: SessionInfo[], extra: Partial<FakeMachine> = {}): FakeMachine {
  return {
    id: 'local', name: 'Fixture Mac', host: 'localhost', port: 8787, isDefault: true,
    sessions, ...extra
  };
}

describe('capability: archive a session from its own header', () => {
  it('archives an ended session and takes it off the list', async () => {
    const ended = makeSession({ id: 'wrapped-up', name: 'Friday release prep', ...ENDED });
    const machine = machineWith([
      makeSession({ id: 'still-going', name: 'This week', working: true }),
      ended
    ]);
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine]);
    const user = userEvent.setup();

    render(<Workbench><SessionView sessionId={ended.id} isActive /></Workbench>);

    const archive = await screen.findByRole('button', { name: 'Archive' });
    expect(screen.queryByRole('button', { name: 'End and archive…' })).not.toBeInTheDocument();

    await user.click(archive);

    await waitFor(() => expect(daemon.archived).toContain(ended.id));
    // Off the list the person is looking at, with nothing deleted: the other
    // session is untouched and this one's record is still in the daemon.
    await waitFor(() => expect(screen.queryByText('Friday release prep')).not.toBeInTheDocument());
    expect(screen.getByText('This week')).toBeInTheDocument();
  }, 20_000);

  it('asks before ending a live session, and says what that costs', async () => {
    const live = makeSession({ id: 'mid-flight', name: 'Refactoring the parser', working: true });
    const machine = machineWith([live]);
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine]);
    const user = userEvent.setup();

    render(<Workbench><SessionView sessionId={live.id} isActive /></Workbench>);

    // No bare "Archive" on something that is running: the daemon would refuse
    // it, so the button says what it actually does.
    expect(screen.queryByRole('button', { name: 'Archive' })).not.toBeInTheDocument();
    await user.click(await screen.findByRole('button', { name: 'End and archive…' }));

    const dialog = screen.getByRole('dialog');
    expect(within(dialog).getByText(/work in progress will be interrupted/i)).toBeInTheDocument();
    expect(within(dialog).getByText(/conversation is kept/i)).toBeInTheDocument();

    // Backing out archives nothing and ends nothing.
    await user.click(within(dialog).getByRole('button', { name: 'Keep working' }));
    expect(daemon.ended).not.toContain(live.id);
    expect(daemon.archived).not.toContain(live.id);

    await user.click(screen.getByRole('button', { name: 'End and archive…' }));
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'End and archive' }));

    await waitFor(() => expect(daemon.ended).toContain(live.id));
    await waitFor(() => expect(daemon.archived).toContain(live.id));
  }, 20_000);

  // The daemon answers per session and can refuse. Its reason is the only
  // thing that explains why the row is still there.
  it('says why the daemon refused instead of reporting success', async () => {
    const ended = makeSession({ id: 'stubborn', name: 'Looks finished', ...ENDED });
    const machine = machineWith([ended], {
      archiveRefusals: { stubborn: 'runner is still live' }
    });
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine]);
    const user = userEvent.setup();

    render(<Workbench><SessionView sessionId={ended.id} isActive /></Workbench>);
    await user.click(await screen.findByRole('button', { name: 'Archive' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('runner is still live');
    expect(daemon.archived).not.toContain(ended.id);
    // It was refused, so it is still on the list it was on.
    expect(screen.getByText('Looks finished', { selector: '.session-nav-title' })).toBeInTheDocument();
  }, 20_000);
});

describe('capability: an archived conversation is still findable', () => {
  it('keeps it in History, marked Archived', async () => {
    const ended = makeSession({ id: 'put-away', name: 'Friday release prep', ...ENDED });
    const machine = machineWith([ended], {
      history: [{
        id: 'put-away', name: 'Friday release prep', tool: 'claude',
        cwd: '/Users/example/project', machine: 'Fixture Mac',
        created_at: Date.now() - 86_400_000, last_activity_at: ENDED.lastDataAt,
        message_count: 12, conversation_available: true
      }]
    });
    installFakeDaemon([machine]);
    useFakeMachines([machine]);
    const user = userEvent.setup();

    const session = render(<Workbench><SessionView sessionId={ended.id} isActive /></Workbench>);
    await user.click(await screen.findByRole('button', { name: 'Archive' }));
    await waitFor(() => expect(screen.queryByText('Friday release prep')).not.toBeInTheDocument());
    session.unmount();

    // Later, the person goes looking for it. The conversation did not go
    // anywhere — archiving hides a row and deletes nothing — so History still
    // lists it, and says which it is.
    render(
      <ConversationBrowser
        filters={DEFAULT_BROWSE_FILTERS}
        filtered={false}
        onOpen={() => {}}
        onResume={() => {}}
      />
    );
    const card = await screen.findByText('Friday release prep', { selector: '.search-result-source strong' });
    const row = card.closest('.conversation-row') as HTMLElement;
    expect(within(row).getByText('Archived')).toBeInTheDocument();
  }, 20_000);
});
