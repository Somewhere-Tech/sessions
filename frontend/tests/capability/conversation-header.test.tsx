import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { SessionActionsMenu } from '../../src/components/SessionActionsMenu';
import { SessionTitleRename } from '../../src/components/SessionTitleRename';
import { AccountBadge } from '../../src/components/AccountBadge';
import { ComposerModelControl } from '../../src/components/ComposerModelControl';
import { makeSession } from './fake-daemon';

describe('capability: calm conversation header', () => {
  it('keeps confirmation actions mounted and closes the menu with Escape or outside click', async () => {
    const user = userEvent.setup();
    const action = vi.fn();
    const { container } = render(<><SessionActionsMenu>
      <button onClick={action}>Review restart</button>
    </SessionActionsMenu><button>Outside</button><div role="dialog"><button>Confirm later</button></div></>);
    const menu = container.querySelector('details')!;
    const trigger = screen.getByLabelText('Conversation actions', { selector: 'summary' });
    await user.click(trigger);
    expect(menu.open).toBe(true);
    await user.click(screen.getByRole('button', { name: 'Review restart' }));
    expect(action).toHaveBeenCalledTimes(1);
    expect(menu.open).toBe(true);
    await user.click(screen.getByRole('button', { name: 'Confirm later' }));
    await user.keyboard('{Escape}');
    expect(menu.open).toBe(true);
    await user.click(screen.getByRole('button', { name: 'Outside' }));
    expect(menu.open).toBe(false);
    await user.click(trigger);
    await user.keyboard('{Escape}');
    expect(menu.open).toBe(false);
    expect(trigger).toHaveFocus();
  });

  it('exposes the full title and a named rename control without changing the rename contract', async () => {
    const user = userEvent.setup();
    const rename = vi.fn().mockResolvedValue(undefined);
    const title = 'A long conversation title that should remain readable';
    render(<SessionTitleRename label={title} onRename={rename} />);
    expect(screen.getByRole('heading', { name: title })).toHaveAttribute('title', title);
    await user.click(screen.getByRole('button', { name: 'Rename session' }));
    fireEvent.change(screen.getByRole('textbox', { name: 'Session name' }), { target: { value: 'Workspace review' } });
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(rename).toHaveBeenCalledExactlyOnceWith('Workspace review');
  });

  it('uses a readable account fallback and respects an explicit nickname', () => {
    const session = makeSession({ id: 'account', tool: 'codex', profile: 'acct-413d791d8c697724c3a5c39e' });
    const view = render(<AccountBadge session={session} />);
    expect(screen.getByText('ChatGPT account')).toHaveAttribute('title', expect.stringContaining(session.profile!));
    view.rerender(<AccountBadge session={session} label="Personal" />);
    expect(screen.getByText('Personal')).toBeInTheDocument();
  });

  it('does not direct a Codex user to Claude when model changes are unavailable', async () => {
    const user = userEvent.setup();
    render(<ComposerModelControl sessionId="codex" provider="codex" supported={false} working={false} onChange={vi.fn()} />);
    await user.click(screen.getByRole('button', { name: /Choose model/i }));
    expect(screen.getByText(/Choose a model when starting a new Codex session/)).toBeInTheDocument();
    expect(screen.queryByText(/inside Claude/)).not.toBeInTheDocument();
  });
});
