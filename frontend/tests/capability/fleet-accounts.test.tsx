// CAPABILITY: a second subscription is a choice on every computer, not only on
// the one this window is connected to.
//
// Accounts landed as a per-machine fact and a Settings page for the active
// machine. What was left undone: Fleet's machine cards said nothing about
// accounts beyond a count in the meta row, so "this Mac has the team plan and
// the mini does not" was invisible, and there was no way to add the missing one
// where it was missing without first switching the whole app to that machine.
import { describe, expect, it } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { FleetView } from '../../src/components/FleetView';
import { AccountsPanel } from '../../src/components/AccountsPanel';
import { installFakeDaemon, useFakeMachines, type FakeDaemon, type FakeMachine } from './fake-daemon';

const ALPHA_ORIGIN = 'http://10.0.0.5:8787';
const BETA_ORIGIN = 'http://10.0.0.6:8787';

function fleet(): FakeMachine[] {
  return [
    {
      id: 'alpha', name: 'Alpha', host: '10.0.0.5', port: 8787, machineId: 'machine-alpha', sessions: [],
      profiles: [
        { tool: 'claude', name: 'work', label: 'Work — team plan', signed_in: true },
        { tool: 'codex', name: 'work-gpt', label: 'Work ChatGPT', signed_in: false }
      ]
    },
    {
      id: 'beta', name: 'Beta', host: '10.0.0.6', port: 8787, machineId: 'machine-beta', sessions: [],
      profiles: [{ tool: 'claude', name: 'shared', label: 'Shared build box', signed_in: true }]
    }
  ];
}

function cardFor(name: string): HTMLElement {
  const heading = screen.getByRole('heading', { name, level: 2 });
  const card = heading.closest('section');
  if (!card) throw new Error(`no machine card around the heading "${name}"`);
  return card;
}

function lastRequest(daemon: FakeDaemon, path: string): { origin: string; body: unknown } | undefined {
  return daemon.requests.filter((request) => request.method === 'POST' && request.path === path).pop();
}

describe('capability: each computer says which accounts it has', () => {
  it('names the accounts on a machine card, with what Sessions can actually see', async () => {
    const machines = fleet();
    installFakeDaemon(machines);
    useFakeMachines(machines, 'alpha');
    render(<FleetView onOpenSession={() => {}} onOpenMachine={() => {}} />);

    const alpha = await waitFor(() => {
      const card = cardFor('Alpha');
      expect(within(card).getByText('Work — team plan')).toBeVisible();
      return card;
    });
    // The label the owner typed, the provider, and the only account fact
    // Sessions has ever checked: whether a login file is there.
    expect(within(alpha).getByText('Work ChatGPT')).toBeVisible();
    expect(within(alpha).getAllByText('Identity not checked').length).toBe(2);
    expect(within(alpha).queryByText('Signed in')).not.toBeInTheDocument();

    // Beta has its own account, and does not borrow Alpha's.
    const beta = cardFor('Beta');
    expect(within(beta).getByText('Shared build box')).toBeVisible();
    expect(within(beta).getByText('Identity not checked')).toBeVisible();
    // Beta's account appears on Alpha only as the offer to log in there too —
    // never as an account Alpha has.
    expect(within(alpha).getByText('Shared build box').closest('.fleet-account')?.className)
      .toContain('is-missing');
    expect(within(beta).getByText('Shared build box').closest('.fleet-account')?.className)
      .not.toContain('is-missing');
  }, 20_000);

  it('offers the missing account where it is missing, and nowhere else', async () => {
    const machines = fleet();
    installFakeDaemon(machines);
    useFakeMachines(machines, 'alpha');
    render(<FleetView onOpenSession={() => {}} onOpenMachine={() => {}} />);

    const beta = await waitFor(() => {
      const card = cardFor('Beta');
      expect(within(card).getAllByRole('button', { name: 'Log in here too' }).length).toBe(2);
      return card;
    });
    // The offer carries the account's own label, and never claims Beta has it.
    expect(within(beta).getByText('Work — team plan')).toBeVisible();
    expect(within(beta).getAllByText('Identity not checked').length).toBe(1);
    // Alpha is missing Beta's account, so exactly one offer appears there.
    const alpha = cardFor('Alpha');
    expect(within(alpha).getAllByRole('button', { name: 'Log in here too' }).length).toBe(1);
    expect(within(alpha).getByText('Shared build box')).toBeVisible();
  }, 20_000);

  it('logs in on that computer: the account is registered there and the sign-in opens there', async () => {
    const machines = fleet();
    const daemon = installFakeDaemon(machines);
    useFakeMachines(machines, 'alpha');
    const user = userEvent.setup();
    const opened: Array<{ serverId: string; sessionId: string }> = [];
    render(<FleetView onOpenSession={(serverId, sessionId) => opened.push({ serverId, sessionId })} onOpenMachine={() => {}} />);

    const beta = await waitFor(() => {
      const card = cardFor('Beta');
      expect(within(card).getAllByRole('button', { name: 'Log in here too' }).length).toBe(2);
      return card;
    });
    const offer = within(beta).getByText('Work — team plan').closest('span');
    await user.click(within(offer as HTMLElement).getByRole('button', { name: 'Log in here too' }));

    // Registered on Beta, under the name and label its owner already chose.
    await waitFor(() => expect(machines[1]!.profiles?.some(
      (account) => account.name === 'work' && account.label === 'Work — team plan'
    )).toBe(true));
    expect(lastRequest(daemon, '/api/profiles')?.origin).toBe(BETA_ORIGIN);

    // And the provider's own sign-in was opened on Beta, in that account's home.
    const created = lastRequest(daemon, '/api/account-logins');
    expect(created?.origin).toBe(BETA_ORIGIN);
    expect((created?.body as { profile?: string; cmd?: string }).profile).toBe('work');
    expect((created?.body as { tool?: string }).tool).toBe('claude');
    expect(opened).toEqual([]);

    // The same instruction Settings gives, naming the computer it happened on.
    expect(await within(beta).findByText(/Check that you choose the account/i)).toBeInTheDocument();
    expect(within(beta).getByRole('heading', { name: /Sign in to Claude on Beta/ })).toBeInTheDocument();
    // Nothing was created on the machine this window is connected to.
    expect(daemon.requests.filter(
      (request) => request.method === 'POST' && request.origin === ALPHA_ORIGIN && request.path === '/api/profiles'
    )).toHaveLength(0);
  }, 20_000);
});

describe('capability: Accounts manages sign-ins on any computer', () => {
  it('switches computers and reads that machine’s accounts', async () => {
    const machines = fleet();
    installFakeDaemon(machines);
    useFakeMachines(machines, 'alpha');
    const user = userEvent.setup();
    render(
      <AccountsPanel
        profiles={[{ tool: 'claude', name: 'work', label: 'Work — team plan', path: '/state/profiles/claude/work', signed_in: true, sessions: [], last_used: 1 }]}
        machineName="Alpha"
        onReload={() => {}}
      />
    );
    expect(screen.getByText('Work — team plan')).toBeVisible();

    await user.selectOptions(screen.getByLabelText('Computer'), 'beta');
    await waitFor(() => expect(screen.getByText('Shared build box')).toBeVisible());
    // Alpha's account is not shown as if it were Beta's.
    expect(screen.queryByText('Work — team plan')).not.toBeInTheDocument();
    expect(screen.getByLabelText('Computer')).toHaveDisplayValue('Beta');
    expect(screen.getByRole('list', { name: 'Accounts on Beta' })).toBeVisible();
  }, 20_000);

  it('adds an account on the computer that was chosen, not the connected one', async () => {
    const machines = fleet();
    const daemon = installFakeDaemon(machines);
    useFakeMachines(machines, 'alpha');
    const user = userEvent.setup();
    render(<AccountsPanel profiles={[]} machineName="Alpha" onReload={() => {}} />);

    await user.selectOptions(screen.getByLabelText('Computer'), 'beta');
    await waitFor(() => expect(screen.getByText('Shared build box')).toBeVisible());
    await user.click(screen.getByRole('button', { name: 'Add account' }));
    await user.type(screen.getByLabelText('Account label'), 'Second plan');
    await user.click(screen.getByRole('button', { name: 'Continue' }));

    await waitFor(() => expect(machines[1]!.profiles?.some((account) => account.name === 'acct-fixture')).toBe(true));
    expect(lastRequest(daemon, '/api/profiles')?.origin).toBe(BETA_ORIGIN);
    expect(lastRequest(daemon, '/api/account-logins')?.origin).toBe(BETA_ORIGIN);
    expect(machines[0]!.profiles?.some((account) => account.name === 'acct-fixture')).toBe(false);
  }, 20_000);
});
