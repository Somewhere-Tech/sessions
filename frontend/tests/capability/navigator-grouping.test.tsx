// CAPABILITY: the person chooses how the navigator is arranged.
//
// Asked for by the founder: grouping by project is right when you are working
// on one thing, and wrong when you know which conversation you want and not
// which project it lives in. One control, two arrangements, remembered on this
// device — and the same sessions in both, because the choice is about
// arrangement and nothing else.
import { describe, expect, it } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Workbench } from './harness';
import { buildInboxLayout } from '../../src/lib/inboxSections';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from './fake-daemon';
import type { SessionInfo } from '../../src/types';

const GROUPING_KEY = 'sessions:navigator-grouping';
// This person is looking at one computer: the inbox with its project sections,
// not the all-machines collaborator view.
const MACHINE_SCOPE_KEY = 'sessions:projects-machine-scope';
const LATER = Date.now() - 60_000;
const EARLIER = LATER - 3_600_000;

/** Two projects on one machine, the older project holding the newer session. */
function machineWithProjects(): FakeMachine {
  const older = makeSession({ id: 'older', name: 'Older conversation', cwd: '/Users/example/atlas' });
  const newer = makeSession({ id: 'newer', name: 'Newer conversation', cwd: '/Users/example/beacon' });
  // "Older conversation" is the older name and the newer session: recency has
  // to decide the order, not the fixture's own arrangement.
  Object.assign(older, { createdAt: EARLIER, lastDataAt: LATER, lastAgentMessageAt: LATER, lastSummary: 'Still the newest.' });
  Object.assign(newer, { createdAt: EARLIER - 60_000, lastDataAt: EARLIER, lastAgentMessageAt: EARLIER, lastSummary: 'Older than the other.' });
  const finished = makeSession({ id: 'finished', name: 'Finished conversation', cwd: '/Users/example/atlas' });
  Object.assign(finished, {
    createdAt: EARLIER - 120_000, lastDataAt: EARLIER - 90_000, lastAgentMessageAt: EARLIER - 90_000,
    exited: true, exitedAt: EARLIER - 90_000, exitCode: 0, lastSummary: 'Finished a while ago.'
  });
  return {
    id: 'local',
    name: 'Fixture Mac',
    host: 'localhost',
    port: 8787,
    isDefault: true,
    sessions: [newer, older, finished],
    projects: [
      {
        id: 'atlas', name: 'Atlas', implicit: false, roots: ['/Users/example/atlas'],
        session_ids: ['older', 'finished'], live: 1, needs_input: 0, updated_at: LATER
      },
      {
        id: 'beacon', name: 'Beacon', implicit: false, roots: ['/Users/example/beacon'],
        session_ids: ['newer'], live: 1, needs_input: 0, updated_at: EARLIER
      }
    ]
  };
}

function mountNavigator(): FakeMachine {
  window.localStorage.setItem(MACHINE_SCOPE_KEY, 'local');
  const machine = machineWithProjects();
  installFakeDaemon([machine]);
  useFakeMachines([machine]);
  render(<Workbench />);
  return machine;
}

/** The session rows in the order the navigator paints them. */
function rowTitles(root: ParentNode = document): string[] {
  return Array.from(root.querySelectorAll('.session-nav-row .session-nav-title'))
    .map((node) => node.textContent ?? '');
}

function recentList(): HTMLElement {
  return screen.getByRole('group', { name: 'Sessions, most recent first' });
}

describe('capability: choose how the navigator groups sessions', () => {
  it('groups by project until the person asks for one recent list', async () => {
    window.localStorage.removeItem(GROUPING_KEY);
    mountNavigator();
    const user = userEvent.setup();

    // By project is the arrangement that has always been there, and the one a
    // person who has never touched the control still gets.
    expect(await screen.findByRole('button', { name: 'By project' })).toHaveAttribute('aria-pressed', 'true');
    await screen.findByRole('button', { name: /Atlas/ });
    await screen.findByRole('button', { name: /Beacon/ });

    await user.click(screen.getByRole('button', { name: 'Most recent' }));

    // One list: no project headers, and each row says which project it is in.
    await waitFor(() => expect(screen.queryByRole('button', { name: /Beacon\s*1/ })).not.toBeInTheDocument());
    const list = recentList();
    expect(within(list).getByText('Atlas')).toBeInTheDocument();
    expect(within(list).getByText('Beacon')).toBeInTheDocument();
  }, 20_000);

  it('orders the flat list newest first, whatever project a session is in', async () => {
    window.localStorage.setItem(GROUPING_KEY, 'recent');
    mountNavigator();

    await screen.findByText('Older conversation');
    // One list, and inside it "Older conversation" — the more recently active
    // session — comes first. The project it belongs to does not enter into it.
    expect(rowTitles(recentList())).toEqual(['Older conversation', 'Newer conversation']);

    // Ended work is still folded away rather than mixed into the live list.
    const finished = within(recentList()).getByRole('button', { name: /Finished · 1/ });
    expect(rowTitles(recentList())).not.toContain('Finished conversation');
    await userEvent.setup().click(finished);
    await waitFor(() => expect(rowTitles(recentList())).toContain('Finished conversation'));
  }, 20_000);

  it('remembers the choice on this device', async () => {
    window.localStorage.removeItem(GROUPING_KEY);
    mountNavigator();
    const user = userEvent.setup();

    await user.click(await screen.findByRole('button', { name: 'Most recent' }));
    await waitFor(() => expect(window.localStorage.getItem(GROUPING_KEY)).toBe('recent'));

    // A second visit on this device starts where the person left off.
    const machine = machineWithProjects();
    installFakeDaemon([machine]);
    useFakeMachines([machine]);
    render(<Workbench />);
    await waitFor(() => {
      const controls = screen.getAllByRole('button', { name: 'Most recent' });
      expect(controls[controls.length - 1]).toHaveAttribute('aria-pressed', 'true');
    });
  }, 20_000);

  it('keeps the search working in the flat list', async () => {
    window.localStorage.setItem(GROUPING_KEY, 'recent');
    mountNavigator();
    const user = userEvent.setup();

    await screen.findByText('Older conversation');
    await user.type(screen.getByLabelText('Filter sessions'), 'Newer');
    await waitFor(() => expect(rowTitles(recentList())).toEqual(['Newer conversation']));
  }, 20_000);

  // The preference is a preference. Storage that refuses to answer costs the
  // person the memory of the choice, never the choice itself.
  it('still rearranges when storage refuses to remember', async () => {
    window.localStorage.removeItem(GROUPING_KEY);
    mountNavigator();
    const user = userEvent.setup();
    await screen.findByText('Older conversation');

    const real = window.localStorage;
    Object.defineProperty(window, 'localStorage', {
      configurable: true,
      value: { ...real, getItem: () => { throw new Error('denied'); }, setItem: () => { throw new Error('denied'); } }
    });
    try {
      await user.click(screen.getByRole('button', { name: 'Most recent' }));
      await waitFor(() => expect(rowTitles(recentList())).toEqual(['Older conversation', 'Newer conversation']));
    } finally {
      Object.defineProperty(window, 'localStorage', { configurable: true, value: real });
    }
  }, 20_000);
});

describe('capability: the flat list is one list, not a flattened grouping', () => {
  it('puts every live session in one section and folds the finished ones', () => {
    const live = [
      makeSession({ id: 'a', lastDataAt: EARLIER }),
      makeSession({ id: 'b', lastDataAt: LATER })
    ];
    const ended: SessionInfo[] = [Object.assign(makeSession({ id: 'c', lastDataAt: EARLIER - 1 }), { exited: true })];

    const layout = buildInboxLayout({
      live, ended,
      lastActivity: (session) => session.lastDataAt,
      projectFor: () => ({ id: 'p', name: 'A project', implicit: false }),
      grouping: 'recent'
    });

    expect(layout.sections).toEqual([]);
    expect(layout.other).toBeNull();
    expect(layout.flat?.live.map((session) => session.id)).toEqual(['b', 'a']);
    expect(layout.flat?.finished.map((session) => session.id)).toEqual(['c']);
  });
});
