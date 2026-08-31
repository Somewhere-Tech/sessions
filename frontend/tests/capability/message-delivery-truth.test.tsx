// CAPABILITY: once sessionsd acknowledges a composer submission, a slow or
// unavailable provider-history mirror must never relabel it as undelivered.
import { act, renderHook } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { useDispatch } from '../../src/hooks/useDispatch';
import { useFakeMachines, type FakeMachine } from './fake-daemon';

describe('capability: acknowledged messages stay acknowledged', () => {
  it('does not invent a transcript-confirmation timeout', () => {
    vi.useFakeTimers();
    const machine: FakeMachine = {
      id: 'local',
      name: 'Fixture Mac',
      host: 'localhost',
      port: 8787,
      isDefault: true,
      sessions: []
    };
    useFakeMachines([machine]);
    const { result } = renderHook(() => useDispatch({
      sessionId: 'receipt-backed-session',
      eventUserContentCounts: new Map()
    }));

    act(() => result.current.recordSent('Keep this acknowledged'));
    expect(result.current.messages.at(-1)?.status).toBe('accepted');

    act(() => vi.advanceTimersByTime(60_000));
    expect(result.current.messages.at(-1)).toMatchObject({
      content: 'Keep this acknowledged',
      status: 'accepted'
    });
    expect(result.current.messages.at(-1)?.failureReason).toBeUndefined();
    vi.useRealTimers();
  });
});
