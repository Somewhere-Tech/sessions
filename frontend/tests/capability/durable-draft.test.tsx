import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { InputBar } from '../../src/components/InputBar';

function composer(machine = 'machine-a', session = 'session-a', submitMessage = async (): Promise<void> => {}): ReturnType<typeof render> {
  return render(
    <InputBar
      send={async () => {}}
      submitMessage={submitMessage}
      connected
      sessionId={session}
      draftMachineId={machine}
    />
  );
}

function input(): HTMLTextAreaElement {
  return screen.getByRole('textbox') as HTMLTextAreaElement;
}

afterEach(() => vi.useRealTimers());

describe('capability: durable unsent drafts', () => {
  it('restores a draft after unmount and keeps machines isolated', () => {
    const first = composer('machine-a', 'same-session');
    fireEvent.change(input(), { target: { value: 'Mac draft' } });
    first.unmount();

    const other = composer('machine-b', 'same-session');
    expect(input()).toHaveValue('');
    fireEvent.change(input(), { target: { value: 'Mini draft' } });
    other.unmount();

    const restored = composer('machine-a', 'same-session');
    expect(input()).toHaveValue('Mac draft');
    restored.unmount();
  });

  it('keeps failed sends and clears an acknowledged successful send', async () => {
    const failed = composer('machine-a', 'failed', async () => { throw new Error('offline'); });
    fireEvent.change(input(), { target: { value: 'Retry me' } });
    fireEvent.keyDown(input(), { key: 'Enter' });
    expect(await screen.findByText(/Your draft is still here/)).toBeInTheDocument();
    expect(input()).toHaveValue('Retry me');
    failed.unmount();
    const restored = composer('machine-a', 'failed');
    expect(restored.getByRole('textbox')).toHaveValue('Retry me');
    restored.unmount();

    const successful = composer('machine-a', 'success');
    fireEvent.change(input(), { target: { value: 'Delivered' } });
    fireEvent.keyDown(input(), { key: 'Enter' });
    await waitFor(() => expect(input()).toHaveValue(''));
    successful.unmount();
  });

  it('does not erase text edited while a send is awaiting acknowledgement', async () => {
    let acknowledge: (() => void) | undefined;
    const pending = new Promise<void>((resolve) => { acknowledge = resolve; });
    composer('machine-a', 'pending', () => pending);
    fireEvent.change(input(), { target: { value: 'First message' } });
    fireEvent.keyDown(input(), { key: 'Enter' });
    expect(input()).toBeEnabled();
    fireEvent.change(input(), { target: { value: 'Next message' } });
    await act(async () => acknowledge?.());
    expect(input()).toHaveValue('Next message');
  });

  it('does not let an old acknowledgement clear an identical draft on a new machine', async () => {
    let acknowledge: (() => void) | undefined;
    const pending = new Promise<void>((resolve) => { acknowledge = resolve; });
    const props = { send: async (): Promise<void> => {}, submitMessage: () => pending, connected: true, sessionId: 'same' };
    const view = render(<InputBar {...props} draftMachineId="machine-a" />);
    fireEvent.change(input(), { target: { value: 'Same words' } });
    fireEvent.keyDown(input(), { key: 'Enter' });
    view.rerender(<InputBar {...props} draftMachineId="machine-b" />);
    fireEvent.change(input(), { target: { value: 'Same words' } });
    await act(async () => acknowledge?.());
    expect(input()).toHaveValue('Same words');
  });

  it('keeps current text and warns when browser storage rejects the write', async () => {
    const setItem = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new DOMException('Quota exceeded', 'QuotaExceededError');
    });
    composer('machine-a', 'quota');
    fireEvent.change(input(), { target: { value: 'Do not lose this' } });
    fireEvent(window, new Event('pagehide'));
    expect(input()).toHaveValue('Do not lose this');
    expect(await screen.findByRole('alert')).toHaveTextContent('could not be saved');
    setItem.mockRestore();
  });

  it('does not crash and warns on mount when browser draft storage itself is unavailable', async () => {
    const storage = window.localStorage;
    const getter = vi.spyOn(window, 'localStorage', 'get').mockImplementation(() => {
      throw new DOMException('Blocked', 'SecurityError');
    });
    composer('machine-a', 'blocked-storage');
    expect(await screen.findByRole('alert')).toHaveTextContent('could not be read');
    fireEvent.change(input(), { target: { value: 'Still in the composer' } });
    fireEvent(window, new Event('pagehide'));
    expect(input()).toHaveValue('Still in the composer');
    expect(await screen.findByRole('alert')).toHaveTextContent('could not be saved');
    getter.mockRestore();
    storage.clear();
  });

  it('refuses an oversized persisted draft without truncating current text', async () => {
    const oversized = 'x'.repeat(70_000);
    composer('machine-a', 'oversized');
    fireEvent.change(input(), { target: { value: oversized } });
    fireEvent(window, new Event('pagehide'));
    expect(input()).toHaveValue(oversized);
    expect(await screen.findByRole('alert')).toHaveTextContent('too large');
  });

  it('removes acknowledged text from storage immediately', async () => {
    composer('machine-a', 'immediate-clear');
    fireEvent.change(input(), { target: { value: 'Stored before send' } });
    fireEvent(window, new Event('pagehide'));
    expect(window.localStorage.length).toBe(1);
    fireEvent.keyDown(input(), { key: 'Enter' });
    await waitFor(() => expect(input()).toHaveValue(''));
    expect(window.localStorage.length).toBe(0);
  });

  it('coalesces edit persistence and never writes merely because it mounted', () => {
    vi.useFakeTimers();
    const setItem = vi.spyOn(Storage.prototype, 'setItem');
    composer('machine-a', 'coalesced');
    expect(setItem).not.toHaveBeenCalled();
    fireEvent.change(input(), { target: { value: 'a' } });
    fireEvent.change(input(), { target: { value: 'ab' } });
    fireEvent.change(input(), { target: { value: 'abc' } });
    expect(setItem).not.toHaveBeenCalled();
    act(() => vi.advanceTimersByTime(300));
    expect(setItem).toHaveBeenCalledTimes(1);
    setItem.mockRestore();
  });
});
