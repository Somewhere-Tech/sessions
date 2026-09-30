// CAPABILITY: resuming an already selected conversation is scoped to the
// selected machine, deduplicated while pending, and preserves its run choices.
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { act, renderHook, waitFor } from '@testing-library/react';
import { useExactResume } from '../../src/hooks/useExactResume';
import { useServers, type ServerConfig } from '../../src/lib/servers';
import { useSessions } from '../../src/store/sessions';
import { makeSession } from './fake-daemon';

const mocks = vi.hoisted(() => ({
  resumeExactSession: vi.fn(),
  adoptionWarning: vi.fn(() => null)
}));

vi.mock('../../src/lib/resumeExactSession', () => ({
  resumeExactSession: mocks.resumeExactSession
}));

vi.mock('../../src/lib/adoptConversation', () => ({
  adoptionWarning: mocks.adoptionWarning
}));

const serverA: ServerConfig = {
  id: 'machine-a',
  machineId: 'daemon-a',
  name: 'Mac A',
  host: '192.168.1.10',
  port: 8787,
  isDefault: true
};

const serverB: ServerConfig = {
  ...serverA,
  id: 'machine-b',
  machineId: 'daemon-b',
  name: 'Mac B',
  host: '192.168.1.20',
  isDefault: false
};

function outcome(laneId: string) {
  return {
    result: { laneId },
    repairError: null,
    unresolved: false,
    repair: null
  };
}

function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve!: (value: T) => void;
  return { promise: new Promise<T>((done) => { resolve = done; }), resolve };
}

describe('capability: resume one exact conversation', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useServers.setState({ servers: [serverA, serverB], activeId: serverA.id });
  });

  it('deduplicates concurrent resumes of the same session into one adoption', async () => {
    const pending = deferred<ReturnType<typeof outcome>>();
    mocks.resumeExactSession.mockReturnValue(pending.promise);
    const refresh = vi.fn().mockResolvedValue(undefined);
    useSessions.setState({ refresh });
    const onOpen = vi.fn();
    const { result } = renderHook(() => useExactResume(onOpen));
    const session = makeSession({ id: 'ended-session', exited: true });

    let first!: Promise<void>;
    let second!: Promise<void>;
    act(() => {
      first = result.current.resume(session);
      second = result.current.resume(session);
    });
    expect(first).toBe(second);
    await waitFor(() => expect(mocks.resumeExactSession).toHaveBeenCalledTimes(1));

    pending.resolve(outcome('resumed-lane'));
    await act(async () => { await Promise.all([first, second]); });

    expect(refresh).toHaveBeenCalledOnce();
    expect(refresh).toHaveBeenCalledWith(serverA.id);
    expect(onOpen).toHaveBeenCalledOnce();
    expect(onOpen).toHaveBeenCalledWith('resumed-lane');
  });

  it('does not open the resumed lane after the person switches machines', async () => {
    const pending = deferred<ReturnType<typeof outcome>>();
    mocks.resumeExactSession.mockReturnValue(pending.promise);
    const refresh = vi.fn().mockResolvedValue(undefined);
    useSessions.setState({ refresh });
    const onOpen = vi.fn();
    const { result } = renderHook(() => useExactResume(onOpen));

    let resume!: Promise<void>;
    act(() => { resume = result.current.resume(makeSession({ id: 'ended-session', exited: true })); });
    act(() => { useServers.setState({ activeId: serverB.id }); });
    pending.resolve(outcome('lane-on-a'));
    await act(async () => { await resume; });

    expect(refresh).toHaveBeenCalledWith(serverA.id);
    expect(onOpen).not.toHaveBeenCalled();
  });

  it('preserves the selected model, effort, and explicit server id', async () => {
    mocks.resumeExactSession.mockResolvedValue(outcome('configured-lane'));
    useSessions.setState({ refresh: vi.fn().mockResolvedValue(undefined) });
    const onOpen = vi.fn();
    const { result } = renderHook(() => useExactResume(onOpen));
    const session = makeSession({
      id: 'configured-session',
      exited: true,
      model: 'gpt-6-astra',
      effort: 'high'
    });

    await act(async () => { await result.current.resume(session); });

    expect(mocks.resumeExactSession).toHaveBeenCalledWith(
      expect.objectContaining({ id: 'configured-session', model: 'gpt-6-astra', effort: 'high' }),
      undefined,
      undefined,
      serverA.id
    );
    expect(onOpen).toHaveBeenCalledWith('configured-lane');
  });
});
