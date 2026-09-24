import { act, renderHook } from '@testing-library/react';
import { expect, it, vi } from 'vitest';
import { useGuidedAccountLogin } from '../../src/components/AccountSignIn';
import { installFakeDaemon, useFakeMachines, type FakeMachine } from './fake-daemon';

it('does not replace the selected computer accounts with a delayed refresh from the previous computer', async () => {
  const machines: FakeMachine[] = [
    { id: 'local', name: 'MacBook', host: 'localhost', port: 8787, isDefault: true, sessions: [],
      profiles: [{ tool: 'claude', name: 'old-account' }] },
    { id: 'mini', name: 'Mac mini', host: 'mini.test', port: 8787, sessions: [],
      profiles: [{ tool: 'claude', name: 'current-account' }] }
  ];
  installFakeDaemon(machines);
  useFakeMachines(machines, 'local');
  const onReload = vi.fn();
  const { result, rerender } = renderHook(({ id }) => useGuidedAccountLogin({ serverId: id, onReload }), {
    initialProps: { id: 'local' }
  });
  const fetch = globalThis.fetch;
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  globalThis.fetch = async (input, init) => {
    if (String(input).includes('localhost') && String(input).includes('/api/profiles')) await gate;
    return fetch(input, init);
  };
  const pending = result.current.reload();
  try {
    rerender({ id: 'mini' });
    release();
    await act(async () => { await pending; });
    expect(onReload).not.toHaveBeenCalled();
    await act(async () => { await result.current.reload(); });
    expect(onReload).toHaveBeenCalledOnce();
    expect(onReload.mock.calls[0]![0][0].name).toBe('current-account');
  } finally {
    release();
    await pending;
    globalThis.fetch = fetch;
  }
});
