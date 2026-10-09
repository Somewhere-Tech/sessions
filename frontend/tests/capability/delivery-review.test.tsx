import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { InputBar } from '../../src/components/InputBar';
import { MessageDeliveryStatus } from '../../src/components/MessageDeliveryStatus';
import { MessageDeliveryError } from '../../src/lib/messageDelivery';
import { checkMessageDelivery } from '../../src/api/sessionsd/operations';

vi.mock('../../src/api/sessionsd/operations', async (importOriginal) => ({
  ...await importOriginal<typeof import('../../src/api/sessionsd/operations')>(), checkMessageDelivery: vi.fn()
}));

const receipt = (status: 'accepted' | 'not-delivered' | 'unknown' = 'accepted') => ({
  operation_id: 'receipt-one', session_id: 'session-a', status, delivered: status === 'accepted', retry: status === 'not-delivered'
});
const input = (): HTMLTextAreaElement => screen.getByRole('textbox');
function setup(submitMessage = vi.fn().mockResolvedValue(undefined), machine = 'machine-a', onSubmitted = vi.fn()) {
  const props = { send: async (): Promise<void> => {}, submitMessage, connected: true,
    sessionId: 'session-a', draftMachineId: machine, onSubmitted, onSubmitting: () => 4 };
  return { ...render(<InputBar {...props} />), props, submitMessage, onSubmitted };
}
function send(text = 'Review this message'): void {
  fireEvent.change(input(), { target: { value: text } });
  fireEvent.keyDown(input(), { key: 'Enter' });
}
const uncertain = () => vi.fn().mockRejectedValue(new MessageDeliveryError('No receipt yet.', 'unknown', 'receipt-one'));

describe('capability: deliberate delivery recovery', () => {
  it('shows pending acknowledgement and coalesces rapid Enter/click without clearing edits', async () => {
    let done!: () => void;
    const view = setup(vi.fn(() => new Promise<void>((resolve) => { done = resolve; })));
    send('Original');
    expect(screen.getByRole('status')).toHaveTextContent('waiting for acknowledgement');
    fireEvent.keyDown(input(), { key: 'Enter' });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    expect(view.submitMessage).toHaveBeenCalledOnce();
    fireEvent.change(input(), { target: { value: 'Next draft' } });
    await act(async () => done());
    expect(input()).toHaveValue('Next draft');
    expect(screen.queryByText(/waiting for acknowledgement/)).not.toBeInTheDocument();
  });

  it('retains uncertainty on reload and never replays on Enter or reconnect', async () => {
    const first = setup(uncertain());
    send();
    await screen.findByText('Delivery not confirmed');
    fireEvent.keyDown(input(), { key: 'Enter' });
    expect(first.submitMessage).toHaveBeenCalledOnce();
    first.unmount();
    const reopened = setup();
    expect(input()).toHaveValue('Review this message');
    expect(screen.getByText('Delivery not confirmed')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Send' })).toBeDisabled();
    fireEvent.keyDown(input(), { key: 'Enter' });
    expect(reopened.submitMessage).not.toHaveBeenCalled();
  });

  it('allows a new intentional send only after review, never from the review button itself', async () => {
    const submit = uncertain();
    const view = setup(submit);
    send();
    await screen.findByText('Delivery not confirmed');
    fireEvent.click(screen.getByRole('button', { name: "I've checked — enable send" }));
    expect(submit).toHaveBeenCalledOnce();
    submit.mockResolvedValueOnce(undefined);
    fireEvent.keyDown(input(), { key: 'Enter' });
    await waitFor(() => expect(input()).toHaveValue(''));
    expect(view.submitMessage).toHaveBeenCalledTimes(2);
  });

  it('settles an accepted receipt without resending or erasing a newer draft', async () => {
    const view = setup(uncertain());
    send();
    await screen.findByText('Delivery not confirmed');
    fireEvent.change(input(), { target: { value: 'Different new draft' } });
    expect(screen.getByRole('button', { name: 'Send' })).toBeEnabled();
    vi.mocked(checkMessageDelivery).mockResolvedValueOnce(receipt());
    fireEvent.click(screen.getByRole('button', { name: 'Check delivery' }));
    await waitFor(() => expect(screen.queryByText('Delivery not confirmed')).not.toBeInTheDocument());
    expect(view.onSubmitted).toHaveBeenCalledWith('Review this message', false, 4);
    expect(input()).toHaveValue('Different new draft');
    expect(view.submitMessage).toHaveBeenCalledOnce();
  });

  it('clears only the unchanged original draft when the receipt proves acceptance', async () => {
    setup(uncertain()); send(); await screen.findByText('Delivery not confirmed');
    vi.mocked(checkMessageDelivery).mockResolvedValueOnce(receipt());
    fireEvent.click(screen.getByRole('button', { name: 'Check delivery' }));
    await waitFor(() => expect(input()).toHaveValue(''));
  });

  it('offers a safe manual send when the receipt proves non-delivery', async () => {
    const view = setup(uncertain()); send(); await screen.findByText('Delivery not confirmed');
    vi.mocked(checkMessageDelivery).mockResolvedValueOnce(receipt('not-delivered'));
    fireEvent.click(screen.getByRole('button', { name: 'Check delivery' }));
    await screen.findByText('The receipt confirms nothing was delivered. Your draft is still here; you can send it when ready.');
    expect(input()).toHaveValue('Review this message');
    expect(screen.getByRole('button', { name: 'Send' })).toBeEnabled();
    expect(view.onSubmitted).not.toHaveBeenCalled();
    expect(view.submitMessage).toHaveBeenCalledOnce();
  });

  it('preserves uncertainty after an unreadable receipt, and distinguishes partial text delivery', async () => {
    setup(vi.fn().mockRejectedValue(new MessageDeliveryError('Partial.', 'text-delivered', 'receipt-one')));
    send(); await screen.findByText('Text delivered · Enter not confirmed');
    vi.mocked(checkMessageDelivery).mockRejectedValueOnce(new Error('Offline; check the conversation.'));
    fireEvent.click(screen.getByRole('button', { name: 'Check delivery' }));
    await screen.findByText('Offline; check the conversation.');
    expect(screen.getByRole('button', { name: 'Send' })).toBeDisabled();
  });

  it('cannot apply a late accepted receipt to another machine with identical draft text', async () => {
    const view = setup(uncertain()); send(); await screen.findByText('Delivery not confirmed');
    let resolve!: (value: ReturnType<typeof receipt>) => void;
    vi.mocked(checkMessageDelivery).mockReturnValueOnce(new Promise((done) => { resolve = done; }));
    fireEvent.click(screen.getByRole('button', { name: 'Check delivery' }));
    view.rerender(<InputBar {...view.props} draftMachineId="machine-b" />);
    fireEvent.change(input(), { target: { value: 'Review this message' } });
    await act(async () => resolve(receipt()));
    expect(input()).toHaveValue('Review this message');
    expect(view.onSubmitted).not.toHaveBeenCalled();
  });

  it('keeps distinct uncertain messages attributable while allowing unrelated new text', async () => {
    const submit = uncertain().mockRejectedValueOnce(new MessageDeliveryError('First.', 'unknown', 'receipt-first'));
    setup(submit); send('First delegation'); await screen.findByText('Delivery not confirmed');
    send('Second delegation');
    await waitFor(() => expect(screen.getAllByText('Delivery not confirmed')).toHaveLength(2));
    expect(screen.getByText('“First delegation”')).toBeInTheDocument();
    expect(screen.getByText('“Second delegation”')).toBeInTheDocument();
    expect(submit).toHaveBeenCalledTimes(2);
  });

  it('does not settle an unmounted receipt check or erase a newer reopened draft', async () => {
    const first = setup(uncertain()); send(); await screen.findByText('Delivery not confirmed');
    let resolve!: (value: ReturnType<typeof receipt>) => void;
    vi.mocked(checkMessageDelivery).mockReturnValueOnce(new Promise((done) => { resolve = done; }));
    fireEvent.click(screen.getByRole('button', { name: 'Check delivery' }));
    first.unmount();
    setup(); fireEvent.change(input(), { target: { value: 'New draft after reopening' } });
    fireEvent(window, new Event('pagehide'));
    await act(async () => resolve(receipt()));
    expect(input()).toHaveValue('New draft after reopening');
    expect(first.onSubmitted).not.toHaveBeenCalled();
    expect(screen.getByText('Delivery not confirmed')).toBeInTheDocument();
  });

  it('shows accepted separately from completed and does not offer a resend', () => {
    render(<MessageDeliveryStatus message={{ id: 'one', role: 'user', content: 'Go', status: 'accepted', createdAt: 1 }} restore={vi.fn()} remove={vi.fn()} />);
    expect(screen.getByRole('status')).toHaveTextContent('Accepted · waiting for conversation');
    expect(screen.queryByRole('button')).not.toBeInTheDocument();
  });
});
