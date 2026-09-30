import { describe, expect, it, vi } from 'vitest';
import { render, renderHook, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useSessionSidebar } from '../../src/hooks/useSessionSidebar';
import { ClaudeRuntimeControl } from '../../src/components/ClaudeRuntimeControl';
import { ComposerModelControl } from '../../src/components/ComposerModelControl';
import { observedSessionModel } from '../../src/lib/sessionModelLabel';
import { InputBar } from '../../src/components/InputBar';
import { makeSession } from './fake-daemon';
import type { ClaudeSessionEvent } from '../../src/types';

const events: ClaudeSessionEvent[] = [
  { type: 'user', timestamp: '2026-09-11T18:26:00Z', message: { content: 'Make a doc' } },
  { type: 'assistant', timestamp: '2026-09-11T18:27:00Z', message: {
    model: 'claude-fable-5', stop_reason: 'tool_use',
    content: [{ type: 'tool_use', name: 'Bash', input: { command: 'true' } }]
  } },
  // Structured completion need not rewrite the old assistant stop_reason.
  { type: 'result', subtype: 'success', timestamp: '2026-09-11T18:28:00Z' }
];

describe('structured Claude controls', () => {
  it('keeps a refused draft and allows exactly one send after completion', async () => {
    const user = userEvent.setup();
    const submitMessage = vi.fn().mockResolvedValue(undefined);
    const props = { send: vi.fn(), submitMessage, connected: true, sessionId: 'draft-test', richSession: true };
    const { rerender } = render(<InputBar {...props} providerWorking />);
    const input = screen.getByRole('textbox');
    await user.type(input, 'Please use my own words');
    await user.click(screen.getByRole('button', { name: 'Send' }));
    expect(submitMessage).not.toHaveBeenCalled();
    expect(screen.getByText('Claude is still working')).toBeInTheDocument();
    rerender(<InputBar {...props} providerWorking={false} />);
    expect(screen.queryByText('Claude is still working')).not.toBeInTheDocument();
    expect(input).toHaveValue('Please use my own words');
    await user.click(screen.getByRole('button', { name: 'Send' }));
    expect(submitMessage).toHaveBeenCalledTimes(1);
  });
  it('does not let a replayed tool call overrule runner completion', () => {
    const session = makeSession({ id: 'structured', tool: 'claude-code', kind: 'claude-structured' });
    const { result, rerender } = renderHook(({ working }) => useSessionSidebar({
      session, events, daemonWorking: working
    }), { initialProps: { working: true } });
    expect(result.current.isWorking).toBe(true);
    rerender({ working: false });
    expect(result.current.isWorking).toBe(false);
    expect(result.current.currentTask).toBe('');
    expect(result.current.timer).toBe('');
    rerender({ working: true });
    expect(result.current.isWorking).toBe(true);
  });

  it('retains transcript inference for a terminal runner', () => {
    const session = makeSession({ id: 'terminal', tool: 'claude-code' });
    const { result } = renderHook(() => useSessionSidebar({ session, events, daemonWorking: false }));
    expect(result.current.isWorking).toBe(true);
  });

  it('offers an explicit continuation and reports refusal without retrying', async () => {
    const user = userEvent.setup();
    const onContinue = vi.fn().mockRejectedValue(new Error('Enable Remote Control in Settings first'));
    render(<ClaudeRuntimeControl working={false} onContinue={onContinue} />);
    await user.click(screen.getByRole('button', { name: 'Terminal / Remote Control' }));
    expect(onContinue).not.toHaveBeenCalled();
    expect(screen.getByText(/ends the current runtime/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Use Remote Control' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Enable Remote Control');
    expect(onContinue).toHaveBeenCalledExactlyOnceWith(true);
  });

  it('does not switch a genuinely working runtime', async () => {
    const user = userEvent.setup();
    const onContinue = vi.fn();
    render(<ClaudeRuntimeControl working onContinue={onContinue} />);
    await user.click(screen.getByRole('button', { name: 'Terminal / Remote Control' }));
    expect(screen.getByRole('button', { name: 'Continue in Terminal' })).toBeDisabled();
    expect(onContinue).not.toHaveBeenCalled();
  });

  it('uses observed identity only when no explicit selection exists', () => {
    expect(observedSessionModel(undefined, events)).toBe('claude-fable-5');
    expect(observedSessionModel('opus', events)).toBe('opus');
    expect(observedSessionModel(undefined, [])).toBeUndefined();
  });

  it('offers a model choice instead of an unidentified provider default', async () => {
    const user = userEvent.setup();
    render(<ComposerModelControl sessionId="test" provider="claude-code" supported working={false} onChange={vi.fn()} />);
    await user.click(screen.getByRole('button', { name: /Choose model/ }));
    expect(screen.getByRole('option', { name: 'Automatic' })).toBeInTheDocument();
    expect(screen.queryByText('Provider default')).not.toBeInTheDocument();
  });
});
