import { act, renderHook, waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { ServerMsg } from '../../src/types';
import { useTerminal } from '../../src/hooks/useTerminal';
import { useFakeMachines } from './fake-daemon';

const transport = vi.hoisted(() => ({
  messages: undefined as ((message: ServerMsg) => void) | undefined,
  detach: vi.fn(),
  attach: vi.fn()
}));

vi.mock('../../src/lib/wsMux', () => ({
  attachSession: (_url: string, _id: string, handlers: {
    onMessage: (message: ServerMsg) => void; onStatus: (status: string) => void;
  }) => {
    transport.attach();
    transport.messages = handlers.onMessage;
    handlers.onStatus('open');
    return { detach: transport.detach, sendInput: vi.fn(), sendResize: vi.fn() };
  },
  sendSessionInput: vi.fn()
}));

vi.mock('../../src/api/sessionsd', () => ({
  muxEndpointKey: () => 'ws://fixture.invalid/ws?mux=1',
  snapshot: vi.fn(),
  submitMessage: vi.fn(),
  fetchClaudeEvents: async () => ({ events: [], nextIndex: 0, totalCount: 0, startIndex: 0, endIndex: 0 })
}));

describe('capability: streaming resource refusals are visible without a terminal', () => {
  beforeEach(() => {
    transport.messages = undefined;
    vi.clearAllMocks();
    useFakeMachines([{ id: 'fixture', name: 'Fixture', host: 'localhost', port: 8787, sessions: [] }]);
  });

  it('shows recovery instructions without a phantom exit or detached session', async () => {
    const { result } = renderHook(() => useTerminal('active-chat', false, true));
    await waitFor(() => expect(transport.attach).toHaveBeenCalledOnce());
    const message = 'Close an unused chat view and reopen this one. This view limit does not stop sessions.';
    act(() => transport.messages?.({ type: 'error', code: 'mux_attachment_limit', message, sessionId: 'active-chat' }));
    expect(result.current.streamNotice).toBe(message);
    expect(result.current.exitInfo).toBeNull();
    expect(result.current.status).toBe('open');
    expect(transport.detach).not.toHaveBeenCalled();
  });

  it('clears the notice when this view reattaches successfully', async () => {
    const { result } = renderHook(() => useTerminal('active-chat', false, true));
    await waitFor(() => expect(transport.attach).toHaveBeenCalledOnce());
    act(() => transport.messages?.({ type: 'error', code: 'mux_attachment_limit', message: 'Close another view.', sessionId: 'active-chat' }));
    // A successful hello remains the authority for this view's stream.
    act(() => transport.messages?.({
      type: 'hello', protocol: 2, currentSeq: 0, resumedFromSeq: null,
      claudeEventsCount: 0, claudeReplayStart: 0, sessionId: 'active-chat',
      session: { id: 'active-chat', unreachable: false }
    } as ServerMsg));
    expect(result.current.streamNotice).toBeNull();
    expect(result.current.exitInfo).toBeNull();
  });
});
