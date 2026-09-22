import { act, render, screen, waitFor } from '@testing-library/react';
import { expect, it } from 'vitest';
import { Workbench } from './harness';
import { installFakeDaemon, makeSession, useFakeMachines } from './fake-daemon';
import { useSessions } from '../../src/store/sessions';

it('keeps Recently closed in place instead of inserting a loading row on background refresh', async () => {
  window.localStorage.setItem('sessions:projects-machine-scope', 'all-machines');
  const machines = [{ id: 'local', name: 'MacBook', host: 'localhost', port: 8787, isDefault: true,
    sessions: [makeSession({ id: 'manager', name: 'Release manager' })] }];
  installFakeDaemon(machines);
  useFakeMachines(machines);
  render(<Workbench />);
  await screen.findByText('Release manager');
  await waitFor(() => expect(useSessions.getState().hydrated).toBe(true));
  expect(screen.queryByText('Loading agents…')).not.toBeInTheDocument();
  const closed = screen.getByRole('button', { name: 'Recently closed' });
  const tree = closed.closest('.session-tree')!;
  const before = tree.innerHTML;
  const fetch = globalThis.fetch;
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  globalThis.fetch = async (input, init) => {
    if (String(input).includes('/api/sessions')) await gate;
    return fetch(input, init);
  };
  let refresh!: Promise<void>;
  try {
    act(() => { refresh = useSessions.getState().refresh(); });
    expect(useSessions.getState().loading).toBe(true);
    // No inserted row, removed agents, or remounted disclosure while the
    // network response is outstanding. Actual first-load indicators remain.
    expect(screen.queryByText('Loading agents…')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Recently closed' })).toBe(closed);
    expect(tree.innerHTML).toBe(before);
  } finally {
    release();
    await act(async () => { await refresh; });
    globalThis.fetch = fetch;
  }
});
