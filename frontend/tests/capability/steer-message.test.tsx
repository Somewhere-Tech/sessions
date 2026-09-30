// CAPABILITY: a follow-up can explicitly steer a working Codex turn without
// becoming an ordinary queued message, and a refused steer remains editable.
import { describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { InputBar } from '../../src/components/InputBar';
import { RemoteView } from '../../src/components/RemoteView';
import { MessageDeliveryError } from '../../src/lib/messageDelivery';

const workingSidebar = {
  parserName: 'Codex',
  parserIcon: '🟢',
  isWorking: true,
  timer: '',
  tokens: '',
  context: '',
  finalElapsed: '',
  currentTask: '',
  checklist: []
};

function inputBar(overrides: Partial<React.ComponentProps<typeof InputBar>> = {}): JSX.Element {
  return <InputBar
    send={vi.fn().mockResolvedValue(undefined)}
    submitMessage={vi.fn().mockResolvedValue(undefined)}
    connected
    sessionId="steer-session"
    provider="codex"
    providerWorking
    {...overrides}
  />;
}

describe('capability: steer a working Codex turn', () => {
  it('uses explicit steering for Enter and ignores repeated input while acknowledgment is pending', async () => {
    let acknowledge!: () => void;
    const steerMessage = vi.fn(() => new Promise<void>((resolve) => { acknowledge = resolve; }));
    const submitMessage = vi.fn().mockResolvedValue(undefined);
    render(inputBar({ steerMessage, submitMessage }));
    const composer = screen.getByPlaceholderText(/Message Codex/);
    fireEvent.change(composer, { target: { value: 'Change the final answer' } });
    fireEvent.keyDown(composer, { key: 'Enter' });
    fireEvent.keyDown(composer, { key: 'Enter' });
    fireEvent.click(screen.getByRole('button', { name: 'Steer now' }));
    expect(steerMessage).toHaveBeenCalledTimes(1);
    expect(submitMessage).not.toHaveBeenCalled();
    expect(composer).toHaveValue('Change the final answer');
    await act(async () => acknowledge());
    expect(composer).toHaveValue('');
  });

  it('keeps a refused in-flight steer through the working-to-idle transition', async () => {
    let refuse!: (error: Error) => void;
    const steerMessage = vi.fn(() => new Promise<void>((_resolve, reject) => { refuse = reject; }));
    const submitMessage = vi.fn().mockResolvedValue(undefined);
    const view = render(inputBar({ steerMessage, submitMessage }));
    const composer = screen.getByPlaceholderText(/Message Codex/);
    fireEvent.change(composer, { target: { value: 'Change the final answer' } });
    fireEvent.keyDown(composer, { key: 'Enter' });
    view.rerender(inputBar({ steerMessage, submitMessage, providerWorking: false }));
    await act(async () => refuse(new MessageDeliveryError('Codex has no active turn.', 'not-delivered', 'idle-race')));
    expect(composer).toHaveValue('Change the final answer');
    expect(screen.getByRole('alert')).toHaveTextContent('Message not sent');
    expect(submitMessage).not.toHaveBeenCalled();
    expect(screen.queryByRole('button', { name: 'Steer now' })).not.toBeInTheDocument();
  });

  it('offers steering only for a supported working Codex composer', async () => {
    const user = userEvent.setup();
    const { rerender } = render(inputBar({ steerMessage: vi.fn().mockResolvedValue(undefined) }));
    const composer = screen.getByPlaceholderText(/Message Codex/);

    await user.type(composer, 'Check the Windows artifact too');
    expect(screen.getByRole('button', { name: 'Steer now' })).toBeEnabled();

    rerender(inputBar({ providerWorking: false, steerMessage: vi.fn().mockResolvedValue(undefined) }));
    expect(screen.queryByRole('button', { name: 'Steer now' })).not.toBeInTheDocument();

    rerender(inputBar({ provider: 'claude-code', steerMessage: vi.fn().mockResolvedValue(undefined) }));
    expect(screen.queryByRole('button', { name: 'Steer now' })).not.toBeInTheDocument();
  });

  it('routes Steer now exactly once through the steer callback, not normal send', async () => {
    const user = userEvent.setup();
    const steerMessage = vi.fn().mockResolvedValue(undefined);
    const submitMessage = vi.fn().mockResolvedValue(undefined);
    const send = vi.fn().mockResolvedValue(undefined);
    render(<RemoteView
      sessionId="steer-session"
      events={[]}
      historyPending={false}
      sendConfirmed={send}
      submitMessage={submitMessage}
      steerMessage={steerMessage}
      connected
      hasEarlierClaudeEvents={false}
      loadingEarlierClaudeEvents={false}
      onLoadEarlierClaudeEvents={() => {}}
      sidebar={workingSidebar}
      cwd="/Users/example/project"
      onOpenTerminal={() => {}}
      terminalAvailable={false}
      provider="codex"
    />);

    const composer = screen.getByPlaceholderText(/Message Codex/);
    await user.type(composer, 'Check the Windows artifact too');
    await user.click(screen.getByRole('button', { name: 'Steer now' }));

    expect(steerMessage).toHaveBeenCalledTimes(1);
    expect(steerMessage).toHaveBeenCalledWith('Check the Windows artifact too');
    expect(submitMessage).not.toHaveBeenCalled();
    expect(send).not.toHaveBeenCalled();
    expect(composer).toHaveValue('');
  });

  it('keeps the draft when Codex refuses the steer', async () => {
    const user = userEvent.setup();
    const steerMessage = vi.fn().mockRejectedValue(new Error('The active turn is no longer accepting input.'));
    const submitMessage = vi.fn().mockResolvedValue(undefined);
    render(inputBar({ steerMessage, submitMessage }));

    const composer = screen.getByPlaceholderText(/Message Codex/);
    await user.type(composer, 'Run the signing check before finishing');
    await user.click(screen.getByRole('button', { name: 'Steer now' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('Message not sent');
    expect(screen.getByRole('alert')).toHaveTextContent('The active turn is no longer accepting input. Your draft is still here.');
    expect(composer).toHaveValue('Run the signing check before finishing');
    expect(submitMessage).not.toHaveBeenCalled();
  });

  it('keeps the draft without claiming an uncertain steer was not sent', async () => {
    const user = userEvent.setup();
    const steerMessage = vi.fn().mockRejectedValue(new MessageDeliveryError(
      'Sessions could not confirm whether Codex received this follow-up.',
      'unknown',
      'operation-uncertain'
    ));
    const submitMessage = vi.fn().mockResolvedValue(undefined);
    render(inputBar({ steerMessage, submitMessage }));

    const composer = screen.getByPlaceholderText(/Message Codex/);
    await user.type(composer, 'Verify whether the release upload finished');
    await user.click(screen.getByRole('button', { name: 'Steer now' }));

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Delivery not confirmed');
    expect(alert).not.toHaveTextContent('Message not sent');
    expect(composer).toHaveValue('Verify whether the release upload finished');
    expect(steerMessage).toHaveBeenCalledOnce();
    expect(submitMessage).not.toHaveBeenCalled();
  });
});
