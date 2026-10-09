import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, it, vi } from 'vitest';
import { AddAccountDialog } from '../../src/components/AddAccountDialog';

const computers = [{ serverId: 'local', machineName: 'This Mac', profiles: [] }];

it('contains keyboard focus during sign-in preparation, even with no enabled controls', async () => {
  const user = userEvent.setup();
  const cancel = vi.fn();
  const { rerender } = render(<><button>Background</button><AddAccountDialog busy={false} computers={computers}
    initialComputer="local" error={null} onAdd={() => {}} onCancel={cancel} /></>);
  await user.click(screen.getByRole('button', { name: 'Claude' }));
  rerender(<><button>Background</button><AddAccountDialog busy computers={computers}
    initialComputer="local" error={null} onAdd={() => {}} onCancel={cancel} /></>);
  await user.tab();
  expect(screen.getByRole('dialog', { name: 'Add account' })).toHaveFocus();
  await user.tab({ shift: true });
  expect(screen.getByRole('dialog', { name: 'Add account' })).toHaveFocus();
  screen.getByRole('button', { name: 'Background' }).focus();
  expect(screen.getByRole('dialog', { name: 'Add account' })).toHaveFocus();
  await user.keyboard('{Escape}');
  expect(cancel).not.toHaveBeenCalled();
});

it('wraps first and last controls and explains ChatGPT device-code setup before creating an account', async () => {
  const user = userEvent.setup();
  const add = vi.fn();
  render(<AddAccountDialog busy={false} computers={computers} initialComputer="local" error={null}
    onAdd={add} onCancel={() => {}} />);
  const close = screen.getByRole('button', { name: 'Close account picker' });
  expect(close).toHaveFocus();
  await user.click(screen.getByRole('button', { name: 'ChatGPT' }));
  expect(screen.getByRole('link', { name: /Open ChatGPT security settings/ })).toHaveAttribute('href', 'https://chatgpt.com/#settings/Security');
  expect(screen.getByText(/Managed workspaces may need an administrator/)).toBeVisible();
  expect(add).not.toHaveBeenCalled();
  const signIn = screen.getByRole('button', { name: 'Sign in to ChatGPT' });
  signIn.focus();
  await user.tab();
  expect(close).toHaveFocus();
  await user.tab({ shift: true });
  expect(signIn).toHaveFocus();
  await user.click(signIn);
  expect(add).toHaveBeenCalledExactlyOnceWith('local', 'codex', '');
});
