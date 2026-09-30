// CAPABILITY: a person can end a session, and see that it ended.
//
// The SUCCESS path, which has never been covered. scripts/surface-truth-fixture.tsx
// mounts these surfaces over a fetch that answers every `DELETE /api/sessions/*`
// with a hard-coded 503 (line 129 of that file), so the only end-a-session
// behaviour any test has ever exercised is the failure banner. A daemon that
// accepted the request and did nothing, or a UI that ended the wrong session,
// would have passed.
//
// Here the fake daemon behaves the way runtime/CONTRACT/http-api.md says it
// does — `200 {"ok":true}`, then the runner EXIT path retires the record — and
// the assertion is what the person sees afterwards.
import { describe, expect, it } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ENDED_GROUP, Workbench } from './harness';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from './fake-daemon';

function localMachine(): FakeMachine {
  return {
    id: 'local',
    name: 'Fixture Mac',
    host: 'localhost',
    port: 8787,
    isDefault: true,
    sessions: [
      makeSession({ id: 'running', name: 'Long build', working: true }),
      makeSession({ id: 'bystander', name: 'Do not touch this one' })
    ]
  };
}

describe('capability: end a session', () => {
  it('ends the session the person chose, and only that one', async () => {
    const machine = localMachine();
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine]);
    const user = userEvent.setup();

    render(<Workbench />);

    await user.click(await screen.findByRole('button', { name: 'Actions for Long build' }));
    await user.click(await screen.findByRole('menuitem', { name: 'End session…' }));

    // A confirmation, because ending is not undoable.
    const sheet = await screen.findByRole('dialog', { name: /Stop “Long build”/ });
    await user.click(within(sheet).getByRole('button', { name: 'End session' }));

    // The daemon ended exactly one session, and it was the right one.
    await waitFor(() => expect(daemon.ended).toEqual(['running']));
    expect(daemon.session('bystander')?.exited).toBe(false);

    // The confirmation is gone, and no failure is claimed.
    await waitFor(() => expect(screen.queryByRole('dialog', { name: /Stop “Long build”/ })).not.toBeInTheDocument());
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();

    // The session is now shown as ended rather than still running: it has left
    // Live and can be found under Ended.
    await waitFor(() => {
      expect(screen.getByRole('button', { name: ENDED_GROUP })).toBeInTheDocument();
    });
    await user.click(screen.getByRole('button', { name: ENDED_GROUP }));
    // The ended session stays visible: in its project's Finished fold summary
    // and in the Ended group, so the label may legitimately appear twice.
    expect((await screen.findAllByText('Long build')).length).toBeGreaterThan(0);
    expect(await screen.findByRole('button', { name: 'Actions for Do not touch this one' })).toBeInTheDocument();
  });
});
