// CAPABILITY: Accounts is a place a person can find, and an account's nickname
// is the daemon's fact, not a label the window invented.
//
// Accounts used to live as the third tab inside Settings. Renaming one was not
// possible at all, and a finished sign-in card stayed on screen above the next
// add form, so the page read as two flows at once.
import { describe, expect, it } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { AccountsView } from '../../src/components/AccountsView';
import { MobileNav } from '../../src/components/MobileNav';
import { ProductSidebar, type ProductView } from '../../src/components/ProductSidebar';
import { installFakeDaemon, useFakeMachines, type FakeMachine } from './fake-daemon';

function machines(): FakeMachine[] {
  return [{
    id: 'local', name: 'This Mac', host: 'localhost', port: 8787, isDefault: true, sessions: [],
    profiles: [
      {
        tool: 'claude', name: 'work', label: 'Work', signed_in: true,
        identity: { email: 'work@example.test', plan: 'max', checked_at: Date.UTC(2026, 8, 1) }
      },
      {
        tool: 'codex', name: 'acct-home', signed_in: true,
        identity: { email: 'home@example.test', checked_at: Date.UTC(2026, 8, 2) }
      },
      // A login file on disk and nothing else: not a verified account.
      { tool: 'claude', name: 'acct-file', signed_in: true }
    ]
  }];
}

async function openAccounts(fleet = machines()) {
  const daemon = installFakeDaemon(fleet);
  useFakeMachines(fleet, 'local');
  render(<AccountsView hostName="This Mac" serverId="local" />);
  const list = await screen.findByRole('list', { name: 'Accounts' });
  return { daemon, list, user: userEvent.setup() };
}

function row(list: HTMLElement, title: string): HTMLElement {
  return within(list).getByText(title, { selector: 'strong' }).closest('li') as HTMLElement;
}

describe('capability: Accounts is its own destination', () => {
  it('is in the desktop rail and the phone More sheet, and nowhere twice', async () => {
    const visited: ProductView[] = [];
    const user = userEvent.setup();
    const { unmount } = render(
      <ProductSidebar active="home" theme="light" onNavigate={(view) => visited.push(view)}
        onNewSession={() => {}} onOpenCommandPalette={() => {}} onToggleTheme={() => {}} />
    );
    const rail = screen.getByRole('navigation', { name: 'Sessions' });
    expect(within(rail).getAllByRole('button', { name: 'Accounts' })).toHaveLength(1);
    await user.click(within(rail).getByRole('button', { name: 'Accounts' }));
    expect(visited).toEqual(['accounts']);
    unmount();

    const modes: string[] = [];
    render(<MobileNav layoutMode="accounts" showingSessionDetail={false} onLayoutChange={(mode) => modes.push(mode)} onShowSessions={() => {}} />);
    // Accounts sits behind More, and More reads as the current place.
    const more = screen.getByRole('button', { name: 'Open More' });
    expect(more).toHaveAttribute('aria-current', 'page');
    await user.click(more);
    const sheet = screen.getByRole('dialog', { name: 'More' });
    await user.click(within(sheet).getByRole('button', { name: /Accounts/ }));
    expect(modes).toEqual(['accounts']);
  });
});

describe('capability: an account row says who it is once, and only what is known', () => {
  it('leads with the nickname, puts the email second, and never repeats it', async () => {
    const { list } = await openAccounts();
    const work = row(list, 'Work');
    expect(within(work).getAllByText(/work@example\.test/)).toHaveLength(1);
    expect(within(work).getByText(/Claude · work@example\.test · max plan/)).toBeInTheDocument();
    expect(within(work).getByText(/^Verified /)).toBeInTheDocument();

    // No nickname: the verified email is the title, and is not said again.
    const home = row(list, 'home@example.test');
    expect(within(home).getAllByText(/home@example\.test/)).toHaveLength(1);
    expect(within(home).getByText('ChatGPT')).toBeInTheDocument();
  });

  it('does not treat a login file as a verified account', async () => {
    const { list } = await openAccounts();
    const file = row(list, 'Claude account');
    expect(within(file).getByText('Identity not checked')).toBeInTheDocument();
    expect(within(file).queryByText(/Verified|Ready|Signed in/)).not.toBeInTheDocument();
    expect(within(file).getByRole('button', { name: 'Sign in' })).toBeEnabled();
    // The raw account ID is not what a person is shown.
    expect(within(list).queryByText('acct-file')).not.toBeInTheDocument();
  });
});

describe('capability: renaming an account is a daemon change', () => {
  it('saves the nickname on the computer and shows it after a reload', async () => {
    const { daemon, list, user } = await openAccounts();
    const work = row(list, 'Work');
    await user.click(within(work).getByRole('button', { name: 'Rename' }));
    const input = within(work).getByRole('textbox', { name: 'Nickname for Work' });
    expect(input).toHaveFocus();
    await user.clear(input);
    await user.type(input, '  Team plan  {Enter}');

    await waitFor(() => expect(within(list).getByText('Team plan', { selector: 'strong' })).toBeInTheDocument());
    const put = daemon.requests.filter((request) => request.method === 'PUT').pop();
    expect(put?.path).toBe('/api/profiles/claude/work');
    expect(put?.body).toEqual({ label: 'Team plan' });
    // Persisted on the daemon, under the same account ID and sign-in.
    const stored = daemon.machines[0]!.profiles!.find((account) => account.name === 'work')!;
    expect(stored.label).toBe('Team plan');
    expect(stored.identity?.email).toBe('work@example.test');
    expect(daemon.created).toHaveLength(0);
  });

  it('clears a nickname back to the verified email, and Escape leaves it alone', async () => {
    const { daemon, list, user } = await openAccounts();
    await user.click(within(row(list, 'Work')).getByRole('button', { name: 'Rename' }));
    await user.keyboard('{Escape}');
    expect(within(list).getByText('Work', { selector: 'strong' })).toBeInTheDocument();
    expect(daemon.requests.some((request) => request.method === 'PUT')).toBe(false);

    await user.click(within(row(list, 'Work')).getByRole('button', { name: 'Rename' }));
    await user.clear(screen.getByRole('textbox', { name: 'Nickname for Work' }));
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(within(list).getByText('work@example.test', { selector: 'strong' })).toBeInTheDocument());
    expect(daemon.machines[0]!.profiles!.find((account) => account.name === 'work')!.label).toBeUndefined();
  });

  it('says why when the computer refuses, and keeps the old nickname', async () => {
    const fleet = machines();
    fleet[0]!.accountRenameForbidden = true;
    const { daemon, list, user } = await openAccounts(fleet);
    const work = row(list, 'Work');
    await user.click(within(work).getByRole('button', { name: 'Rename' }));
    await user.type(within(work).getByRole('textbox', { name: 'Nickname for Work' }), ' two{Enter}');
    expect(await within(work).findByRole('alert')).toHaveTextContent(/local or paired Sessions client/);
    // Still editing, so the person can cancel; nothing changed on the daemon.
    expect(within(work).getByRole('textbox', { name: 'Nickname for Work' })).toHaveValue('Work two');
    expect(daemon.machines[0]!.profiles!.find((account) => account.name === 'work')!.label).toBe('Work');
  });
});

describe('capability: adding an account is one flow at a time', () => {
  it('closes a finished sign-in before showing the next add form', async () => {
    const { list, user } = await openAccounts();
    await user.click(screen.getByRole('button', { name: 'Add account' }));
    await user.type(screen.getByLabelText('Account label'), 'Second');
    await user.click(screen.getByRole('button', { name: 'Continue' }));

    // While the provider sign-in is open there is no second form to start.
    const card = await screen.findByRole('region', { name: 'Sign in to account' });
    expect(screen.queryByRole('form', { name: 'Add an account' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Add account' })).not.toBeInTheDocument();
    expect(within(row(list, 'Work')).getByRole('button', { name: 'Rename' })).toBeDisabled();

    await user.type(within(card).getByLabelText('Claude confirmation code'), 'fixture-code{Enter}');
    expect(await within(card).findByText('second@example.test')).toBeVisible();
    await waitFor(() => expect(within(list).getByText('Second', { selector: 'strong' })).toBeInTheDocument());

    // Adding again replaces the completion card rather than stacking under it.
    await user.click(screen.getByRole('button', { name: 'Add account' }));
    expect(screen.getByRole('form', { name: 'Add an account' })).toBeInTheDocument();
    expect(screen.queryByRole('region', { name: 'Sign in to account' })).not.toBeInTheDocument();
    expect(screen.queryByText('Account connected')).not.toBeInTheDocument();
  });

  it('closes the add form when a row sign-in starts', async () => {
    const { list, user } = await openAccounts();
    await user.click(screen.getByRole('button', { name: 'Add account' }));
    expect(screen.getByRole('form', { name: 'Add an account' })).toBeInTheDocument();
    await user.click(within(row(list, 'Claude account')).getByRole('button', { name: 'Sign in' }));
    await screen.findByRole('region', { name: 'Sign in to account' });
    expect(screen.queryByRole('form', { name: 'Add an account' })).not.toBeInTheDocument();
  });
});
