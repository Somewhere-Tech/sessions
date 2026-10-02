import { act, fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { DeliveryReviewNotice } from '../../src/components/MessageDeliveryStatus';
import { checkMessageDelivery } from '../../src/api/sessionsd/operations';

vi.mock('../../src/api/sessionsd/operations', () => ({ checkMessageDelivery: vi.fn() }));

describe('capability: recover an interrupted first send', () => {
  it('bounds receipt reads and enables deliberate recovery without resending', async () => {
    vi.useFakeTimers();
    const check = vi.mocked(checkMessageDelivery);
    check.mockRejectedValue(new Error('This computer is offline.'));
    const reviewed = vi.fn(), settled = vi.fn();
    try {
      render(<DeliveryReviewNotice review={{ operationId: 'operation', text: 'The original task', queued: false,
        status: 'sending', firstRequest: true }} machineId="local" sessionId="session"
        reviewed={reviewed} settled={settled} />);
      const recover = screen.getByRole('button', { name: "I've checked — enable send" });
      expect(recover).toBeDisabled();
      for (let count = 0; count < 30; count++) {
        await act(async () => { await vi.advanceTimersByTimeAsync(1000); });
      }
      expect(screen.getByText('Delivery not confirmed')).toBeInTheDocument();
      expect(recover).toBeEnabled();
      expect(check).toHaveBeenCalledTimes(30);
      await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
      expect(check).toHaveBeenCalledTimes(30);
      fireEvent.click(recover);
      expect(reviewed).toHaveBeenCalledTimes(1);
      expect(settled).not.toHaveBeenCalled();
    } finally { vi.useRealTimers(); }
  });
});
