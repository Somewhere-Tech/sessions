// CAPABILITY: when the daemon refuses a request, everything it said about
// recovering stays with the error. A reboot-paused session answers 409 with a
// machine-readable code, the session id, a sentence, and the exact command that
// brings it back — and whoever reads that error needs all of it, not a tidy
// summary. Fleet wanting one readable line must not cost anyone else the rest.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { DaemonResponseError, daemonErrorDisplay } from '../../src/api/sessionsd/core';
import { listServerSessions } from '../../src/api/sessionsd';
import type { ServerConfig } from '../../src/lib/servers';

const host: ServerConfig = {
  id: 'paused-host',
  machineId: 'machine-a',
  name: 'Mac A',
  systemName: 'Mac A',
  host: '10.0.0.5',
  port: 8787,
  scheme: 'http',
  token: 'device-token',
  isDefault: false
};

const pausedSession = {
  code: 'SESSION_NEEDS_RECREATE',
  sessionId: 'session-a',
  error: 'session is paused after reboot and cannot be read or controlled until it is resumed: the runner stayed paused after reboot',
  action: 'sessions resume session-a'
};

afterEach(() => { vi.restoreAllMocks(); });

describe('capability: a refusal keeps the way out of it', () => {
  it('keeps the code, the id and the recovery command on the thrown error', async () => {
    vi.spyOn(window, 'fetch').mockResolvedValue(new Response(JSON.stringify(pausedSession), {
      status: 409, headers: { 'content-type': 'application/json' }
    }));

    const failure = await listServerSessions(host).then(() => null, (reason: unknown) => reason);
    expect(failure).toBeInstanceOf(Error);
    const error = failure as Error;
    for (const detail of ['SESSION_NEEDS_RECREATE', 'session-a', 'sessions resume session-a', pausedSession.error]) {
      expect(error.message).toContain(detail);
    }
    expect(error).toBeInstanceOf(DaemonResponseError);
    expect((error as DaemonResponseError).status).toBe(409);
    expect((error as DaemonResponseError).body).toBe(JSON.stringify(pausedSession));
  });

  // The one-line form is opt-in and lives beside the whole body, never instead
  // of it, so a surface with room for one sentence and a surface that needs the
  // command can read the same error.
  it('offers one readable line without becoming the only thing left', () => {
    const error = new DaemonResponseError(409, JSON.stringify(pausedSession), 'Conflict');
    expect(daemonErrorDisplay(error)).toBe(`sessionsd 409: ${pausedSession.error}`);
    expect(error.message).toContain('sessions resume session-a');
  });

  it('falls back to what was actually sent when the body is not a daemon error', () => {
    const plain = new DaemonResponseError(502, 'upstream said no', 'Bad Gateway');
    expect(plain.message).toBe('sessionsd 502: upstream said no');
    expect(daemonErrorDisplay(plain)).toBe('sessionsd 502: upstream said no');

    const empty = new DaemonResponseError(500, '', 'Internal Server Error');
    expect(empty.message).toBe('sessionsd 500: Internal Server Error');
    expect(daemonErrorDisplay(empty)).toBe('sessionsd 500: Internal Server Error');

    expect(daemonErrorDisplay(new TypeError('Failed to fetch'))).toBe('Failed to fetch');
    expect(daemonErrorDisplay('not an error')).toBeNull();
  });
});
