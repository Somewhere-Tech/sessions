// CAPABILITY: a daemon that cannot read its durable session record says so,
// and the sessions a person was looking at stay on screen.
//
// sessionsd answers GET /api/sessions with 503 SESSION_STATE_UNAVAILABLE
// rather than a successful list missing ended sessions and start receipts.
// The store must treat that as a failed refresh: keep this machine's
// last-known rows and the open session, and surface the reason — never an
// empty list.
import { describe, expect, it } from 'vitest';
import { useSessions } from '../../src/store/sessions';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from './fake-daemon';

function localMachine(): FakeMachine {
  return {
    id: 'local',
    name: 'Fixture Mac',
    host: 'localhost',
    port: 8787,
    isDefault: true,
    sessions: [
      makeSession({ id: 'kept-audit', name: 'Release audit' }),
      makeSession({ id: 'kept-docs', name: 'Docs pass' })
    ]
  };
}

describe('capability: unreadable session record', () => {
  it('keeps the last-known sessions and the open one when the listing is unavailable', async () => {
    const machine = localMachine();
    installFakeDaemon([machine]);
    useFakeMachines([machine]);
    await useSessions.getState().refresh('local');
    useSessions.setState({ activeId: 'kept-docs' });

    machine.sessionListFailure = {
      status: 503,
      body: {
        code: 'SESSION_STATE_UNAVAILABLE',
        action: 'retry',
        error: 'Sessions could not read its durable session record. Nothing was changed.'
      }
    };
    await useSessions.getState().refresh('local');

    const state = useSessions.getState();
    expect(state.sessions.map((session) => session.id).sort()).toEqual(['kept-audit', 'kept-docs']);
    expect(state.activeId).toBe('kept-docs');
    expect(state.loading).toBe(false);
    expect(state.error).toContain('durable session record');

    // The next good answer replaces the error with the daemon's list again.
    machine.sessionListFailure = undefined;
    await useSessions.getState().refresh('local');
    expect(useSessions.getState().error).toBeNull();
    expect(useSessions.getState().sessions).toHaveLength(2);
  });
});
