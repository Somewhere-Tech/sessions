import { act, renderHook, waitFor } from '@testing-library/react';
import { expect, it, vi } from 'vitest';
import { useAccountFleet } from '../../src/hooks/useAccountFleet';
import { fetchAccountUsage, type AccountProfile, type AccountUsage } from '../../src/api/sessionsd';
import { useFakeMachines } from './fake-daemon';

vi.mock('../../src/api/sessionsd', async (original) => ({
  ...await original<typeof import('../../src/api/sessionsd')>(),
  fetchAccountUsage: vi.fn()
}));

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}

it('clears earlier quota at sign-in and ignores a late response from before that sign-in', async () => {
  useFakeMachines([{ id: 'local', name: 'MacBook', host: 'localhost', port: 8787, sessions: [] }], 'local');
  const initial = deferred<AccountUsage[]>();
  const oldRefresh = deferred<AccountUsage[]>();
  const newRead = deferred<AccountUsage[]>();
  vi.mocked(fetchAccountUsage).mockReturnValueOnce(initial.promise).mockReturnValueOnce(oldRefresh.promise).mockReturnValueOnce(newRead.promise);
  const profiles: AccountProfile[] = [];
  const { result } = renderHook(() => useAccountFleet({ homeId: 'local', homeName: 'MacBook', homeProfiles: profiles }));
  const oldUsage: AccountUsage = { tool: 'codex', name: 'work', state: 'available', read_at: 1, buckets: [] };
  const freshUsage: AccountUsage = { ...oldUsage, read_at: 2 };
  await act(async () => { initial.resolve([oldUsage]); });
  expect(result.current.machines[0]?.usage?.[0]?.read_at).toBe(1);
  let refresh!: Promise<void>;
  act(() => { refresh = result.current.refreshUsage(); });
  act(() => { result.current.accept('local', profiles); });
  expect(result.current.machines[0]?.usage).toBeUndefined();
  await act(async () => { newRead.resolve([freshUsage]); });
  await waitFor(() => expect(result.current.machines[0]?.usage?.[0]?.read_at).toBe(2));
  await act(async () => { oldRefresh.resolve([oldUsage]); await refresh; });
  expect(result.current.machines[0]?.usage?.[0]?.read_at).toBe(2);
});
