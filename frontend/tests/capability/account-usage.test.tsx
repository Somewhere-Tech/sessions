// CAPABILITY: an account is one row with one allowance, however many computers
// it is signed into, and its usage is the provider's own reading.
//
// Accounts used to be a list per computer, and said nothing about how much of a
// plan was left. The same subscription signed in on two Macs read as two
// accounts, and there was no way to see its limits without opening the
// provider's own app.
import { StrictMode } from 'react';
import { describe, expect, it } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { AccountsView } from '../../src/components/AccountsView';
import { installFakeDaemon, useFakeMachines, type FakeMachine } from './fake-daemon';

const HOUR = 60 * 60_000;

function reading(percent: number, readAt: number, extra: Record<string, unknown> = {}) {
  return {
    state: 'available' as const, checked_at: readAt, read_at: readAt,
    buckets: [{ limit_id: 'codex', windows: [
      { kind: 'primary', used_percent: percent, window_minutes: 300, resets_at: Date.now() + 2 * HOUR },
      { kind: 'secondary', used_percent: 40, window_minutes: 10_080 }
    ] }],
    ...extra
  };
}

function fleet(): FakeMachine[] {
  const verified = (checked: number) => ({ account_id: 'ws-team', email: 'team@example.test', plan: 'team', checked_at: checked });
  return [
    {
      id: 'alpha', name: 'Alpha', host: '10.0.0.5', port: 8787, isDefault: true, sessions: [],
      profiles: [
        { tool: 'codex', name: 'team', label: 'Team plan', signed_in: true, identity: verified(Date.now() - HOUR) },
        { tool: 'codex', name: 'mail', label: 'Personal', signed_in: true, identity: { email: 'me@example.test', checked_at: Date.now() - HOUR } },
        { tool: 'claude', name: 'max', label: 'Claude Max', signed_in: true, identity: { email: 'me@example.test', checked_at: Date.now() - HOUR } }
      ],
      accountUsage: {
        'codex/team': { ...reading(12, Date.now() - 30 * 60_000), identity: verified(Date.now() - 30 * 60_000) },
        'codex/mail': { state: 'signed_out', checked_at: Date.now(), message: 'Codex reports no sign-in for this account. Sign in to see its usage.' }
      }
    },
    {
      id: 'beta', name: 'Beta', host: '10.0.0.6', port: 8787, sessions: [],
      profiles: [
        { tool: 'codex', name: 'team', signed_in: true, identity: verified(Date.now() - 2 * HOUR) },
        // Same email as Alpha's Personal, but no provider account ID: it cannot
        // be proven to be the same workspace, so it stays its own row.
        { tool: 'codex', name: 'other', label: 'Beta personal', signed_in: true, identity: { email: 'me@example.test', checked_at: Date.now() - HOUR } }
      ],
      accountUsage: {
        'codex/team': { ...reading(55, Date.now() - 60_000), identity: verified(Date.now() - 60_000) },
        'codex/other': {
          state: 'unavailable', checked_at: Date.now(), message: 'Codex did not start. Refresh to try again.',
          stale: true, read_at: Date.now() - 3 * HOUR,
          buckets: [{ limit_id: 'codex', windows: [{ kind: 'primary', used_percent: 70, window_minutes: 300 }] }]
        }
      }
    }
  ];
}

async function openAccounts(machines = fleet()) {
  const daemon = installFakeDaemon(machines);
  useFakeMachines(machines, 'alpha');
  render(<AccountsView hostName="Alpha" serverId="alpha" />);
  const list = await screen.findByRole('list', { name: 'Accounts' });
  return { daemon, list, user: userEvent.setup() };
}

function rowFor(list: HTMLElement, title: string): HTMLElement {
  return within(list).getByText(title, { selector: 'strong' }).closest('li') as HTMLElement;
}

describe('capability: one account, one allowance, on every computer it is signed into', () => {
  it('merges homes only when the provider reported the same account ID, and shows the freshest reading', async () => {
    const { list } = await openAccounts();
    const computers = await screen.findByRole('list', { name: 'Computers for Team plan' });
    await waitFor(() => expect(within(computers).getByText('Beta')).toBeVisible());
    expect(within(computers).getByText('Alpha')).toBeVisible();
    const team = rowFor(list, 'Team plan');
    expect(within(team).getByText('One allowance, signed in on 2 computers.')).toBeVisible();
    // Beta read it a minute ago, Alpha half an hour ago: the fresher reading
    // wins, and the two are never added together.
    const usage = await within(team).findByRole('group', { name: 'Usage for Team plan' });
    await waitFor(() => expect(within(usage).getByRole('meter', { name: /5-hour limit used/ })).toHaveAttribute('aria-valuenow', '55'));
    expect(within(usage).getByRole('meter', { name: /weekly limit used/ })).toHaveAttribute('aria-valuenow', '40');
    expect(within(usage).queryByText(/67%/)).not.toBeInTheDocument();
    expect(within(usage).getByText(/Read 1 min ago on Beta/)).toBeVisible();
    expect(within(usage).getByText(/55% used · resets /)).toBeVisible();
    // Beta's copy has no nickname of its own and is not renamed by the merge.
    expect(within(computers).getAllByRole('button', { name: 'Rename' })).toHaveLength(2);
  });

  it('keeps email-only accounts apart and says why', async () => {
    const { list } = await openAccounts();
    await screen.findByText('Beta personal', { selector: 'strong' });
    const personal = rowFor(list, 'Personal');
    expect(within(personal).getByText(/same email is also listed on .*Beta.*cannot confirm it is the same workspace/)).toBeVisible();
    expect(within(screen.getByRole('list', { name: 'Computers for Personal' })).queryByText('Beta')).not.toBeInTheDocument();
  });

  it('keeps a failed read visible as partial, stale coverage — not signed out, not zero', async () => {
    const { list } = await openAccounts();
    const other = rowFor(list, await screen.findByText('Beta personal', { selector: 'strong' }).then((node) => node.textContent!));
    const usage = within(other).getByRole('group', { name: 'Usage for Beta personal' });
    expect(within(usage).getByRole('meter', { name: /5-hour limit used/ })).toHaveAttribute('aria-valuenow', '70');
    expect(within(usage).getByText(/Last known · Read 3 h ago on Beta · Codex did not start/)).toBeVisible();
    expect(within(other).queryByText('Signed out')).not.toBeInTheDocument();
    // A provider that reports no sign-in says so, and shows no usage.
    const personal = rowFor(list, 'Personal');
    expect(within(personal).getByText('Signed out')).toBeVisible();
    expect(within(personal).getByText(/Signed out, so there is no usage to show/)).toBeVisible();
    expect(within(personal).getByRole('button', { name: 'Sign in' })).toBeEnabled();
    expect(within(personal).queryByRole('meter')).not.toBeInTheDocument();
  });

  it('says plainly when a provider or a computer cannot report usage', async () => {
    const machines = fleet();
    machines[1]!.accountUsage = false;
    const { list } = await openAccounts(machines);
    const claude = rowFor(list, 'Claude Max');
    expect(await within(claude).findByText('Claude usage is not connected in Sessions yet. Check Claude for your current limits.')).toBeVisible();
    expect(within(claude).queryByRole('meter')).not.toBeInTheDocument();
    expect(await screen.findByText('Update Sessions on Beta to see usage from it.')).toBeVisible();
    // Alpha still reads the shared account; Beta's missing reading is coverage.
    const team = rowFor(list, 'Team plan');
    await waitFor(() => expect(within(team).getByRole('meter', { name: /5-hour limit used/ })).toHaveAttribute('aria-valuenow', '12'));
    expect(within(team).getByText(/No reading from Beta/)).toBeVisible();
  });

  it('keeps the reachable computers when one does not answer', async () => {
    const machines = fleet();
    machines[1]!.reachable = false;
    const { list } = await openAccounts(machines);
    expect(await screen.findByText('Beta did not answer, so its accounts are not shown.')).toBeVisible();
    expect(rowFor(list, 'Team plan')).toBeVisible();
    expect(within(screen.getByRole('list', { name: 'Computers for Team plan' })).queryByText('Beta')).not.toBeInTheDocument();
  });

  it('refreshes usage on every computer on request', async () => {
    const { daemon, user } = await openAccounts();
    await screen.findByText('Beta personal', { selector: 'strong' });
    await user.click(screen.getByRole('button', { name: 'Refresh usage' }));
    await waitFor(() => {
      const refreshed = daemon.requests.filter((request) => request.path === '/api/account-usage' && request.url.includes('refresh=1'));
      expect(new Set(refreshed.map((request) => request.origin))).toEqual(new Set(['http://10.0.0.5:8787', 'http://10.0.0.6:8787']));
    });
  });

  it('falls back to the latest reading when this client may not make providers answer again', async () => {
    const machines = fleet();
    machines[1]!.accountUsageRefreshForbidden = true;
    const { daemon, list, user } = await openAccounts(machines);
    await screen.findByText('Beta personal', { selector: 'strong' });
    await user.click(screen.getByRole('button', { name: 'Refresh usage' }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Refresh usage' })).toBeEnabled());
    const beta = daemon.requests.filter((request) => request.path === '/api/account-usage' && request.origin === 'http://10.0.0.6:8787');
    expect(beta.at(-2)?.url).toContain('refresh=1');
    expect(beta.at(-1)?.url).not.toContain('refresh=1');
    expect(screen.queryByText(/Beta did not answer/)).not.toBeInTheDocument();
    const team = rowFor(list, 'Team plan');
    expect(within(team).getByRole('meter', { name: /5-hour limit used/ })).toHaveAttribute('aria-valuenow', '55');
  });

  // The app mounts under React.StrictMode, which sets effects up, cleans them
  // up and sets them up again. Answers that arrive after that must still land.
  it('reads other computers and usage under StrictMode, as the app mounts it', async () => {
    const machines = fleet();
    installFakeDaemon(machines);
    useFakeMachines(machines, 'alpha');
    render(<StrictMode><AccountsView hostName="Alpha" serverId="alpha" /></StrictMode>);
    const list = await screen.findByRole('list', { name: 'Accounts' });
    expect(await within(list).findByText('Beta personal', { selector: 'strong' })).toBeVisible();
    const team = rowFor(list, 'Team plan');
    await waitFor(() => expect(within(team).getByRole('meter', { name: /5-hour limit used/ })).toHaveAttribute('aria-valuenow', '55'));
  });

  it('asks a computer that did not answer for its accounts again on refresh', async () => {
    const machines = fleet();
    machines[1]!.reachable = false;
    const { list, user } = await openAccounts(machines);
    expect(await screen.findByText('Beta did not answer, so its accounts are not shown.')).toBeVisible();
    machines[1]!.reachable = true;
    await user.click(screen.getByRole('button', { name: 'Refresh usage' }));
    expect(await within(list).findByText('Beta personal', { selector: 'strong' })).toBeVisible();
    expect(screen.queryByText('Beta did not answer, so its accounts are not shown.')).not.toBeInTheDocument();
    await waitFor(() => expect(within(screen.getByRole('list', { name: 'Computers for Team plan' })).getByText('Beta')).toBeVisible());
  });
});
