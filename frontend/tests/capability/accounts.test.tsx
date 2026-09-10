// CAPABILITY: a second subscription is a choice a person makes, not a setting
// they find.
//
// The owner has a second Claude and a second ChatGPT plan. The machinery for
// running a session on one has existed since profiles landed, but the only way
// to reach it was New Session → Advanced → Account → "Add another login…", and
// nothing anywhere said which accounts a computer had or which account a
// session was using.
import { describe, expect, it } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { NewSessionDialog } from '../../src/components/NewSessionDialog';
import { AccountsPanel } from '../../src/components/AccountsPanel';
import { SubagentsPanel } from '../../src/components/SubagentsPanel';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeDaemon, type FakeMachine } from './fake-daemon';

function fleet(): FakeMachine[] {
  return [
    {
      id: 'local', name: 'This Mac', host: 'localhost', port: 8787, isDefault: true,
      sessions: [],
      profiles: [
        { tool: 'claude', name: 'work', label: 'Work — team plan', signed_in: true },
        { tool: 'claude', name: 'personal', label: 'Personal', signed_in: true },
        { tool: 'codex', name: 'work-gpt', label: 'Work ChatGPT', signed_in: true }
      ]
    },
    {
      id: 'mini', name: 'Mac mini', host: '10.0.0.9', port: 8787,
      sessions: [],
      profiles: [{ tool: 'claude', name: 'shared', label: 'Shared build box', signed_in: true }]
    }
  ];
}

async function openLauncher(): Promise<HTMLSelectElement> {
  render(<NewSessionDialog onClose={() => {}} onStarted={() => {}} />);
  return await screen.findByLabelText('Account') as HTMLSelectElement;
}

describe('capability: accounts are a first-class choice', () => {
  it('offers the accounts on the selected computer beside Agent and Computer', async () => {
    const machines = fleet();
    installFakeDaemon(machines);
    useFakeMachines(machines, 'local');
    const account = await openLauncher();

    // Not behind Advanced: the control is in the setup row with the others.
    expect(account.closest('.launcher-setup')).not.toBeNull();
    expect(account.closest('details')).toBeNull();

    await waitFor(() => expect(within(account).getByRole('option', { name: /Work — team plan/ })).toBeInTheDocument());
    // The label the person typed, not the folder name behind it.
    expect(within(account).getByRole('option', { name: /Personal/ })).toBeInTheDocument();
    expect(within(account).getByRole('option', { name: 'Default' })).toBeInTheDocument();
    expect(within(account).getByRole('option', { name: /Add an account/ })).toBeInTheDocument();
    // Claude's accounts, because the agent is Claude. Codex's are not offered.
    expect(within(account).queryByRole('option', { name: /Work ChatGPT/ })).not.toBeInTheDocument();
  }, 20_000);

  it('follows the computer: another Mac has its own accounts', async () => {
    const machines = fleet();
    installFakeDaemon(machines);
    useFakeMachines(machines, 'local');
    const user = userEvent.setup();
    const account = await openLauncher();
    await waitFor(() => expect(within(account).getByRole('option', { name: /Work — team plan/ })).toBeInTheDocument());

    await user.selectOptions(screen.getByLabelText('Computer'), 'mini');
    await waitFor(() => expect(within(account).getByRole('option', { name: /Shared build box/ })).toBeInTheDocument());
    expect(within(account).queryByRole('option', { name: /Work — team plan/ })).not.toBeInTheDocument();
  }, 20_000);

  it('starts a delegate on its manager account and lets that be changed', async () => {
    const machines = fleet();
    const manager = makeSession({ id: 'manager', name: 'Manager', tool: 'claude-code' });
    manager.profile = 'work';
    machines[0]!.sessions = [manager];
    installFakeDaemon(machines);
    useFakeMachines(machines, 'local');
    const user = userEvent.setup();

    render(<NewSessionDialog onClose={() => {}} onStarted={() => {}} parentSession={manager} />);
    const account = await screen.findByLabelText('Account') as HTMLSelectElement;
    await waitFor(() => expect(account.value).toBe('work'));
    // And it says where that came from rather than looking like a free choice.
    expect(within(account).getByRole('option', { name: /from this session/ })).toBeInTheDocument();

    await user.selectOptions(account, 'personal');
    expect(account.value).toBe('personal');
  }, 20_000);

  it('remembers the account a project used last', async () => {
    const machines = fleet();
    installFakeDaemon(machines);
    useFakeMachines(machines, 'local');
    window.localStorage.setItem(
      'sessions:account-choice',
      JSON.stringify({ 'local claude /Users/somebody/project': 'personal' })
    );
    render(
      <NewSessionDialog
        onClose={() => {}}
        onStarted={() => {}}
        projectSeed={{ serverId: 'local', cwd: '/Users/somebody/project', tags: {} }}
      />
    );
    const account = await screen.findByLabelText('Account') as HTMLSelectElement;
    await waitFor(() => expect(account.value).toBe('personal'));
    window.localStorage.removeItem('sessions:account-choice');
  }, 20_000);
});

describe('capability: adding an account is a guided login', () => {
  it('registers the home, opens the provider sign-in, and never offers an API key', async () => {
    const machines = fleet();
    const daemon: FakeDaemon = installFakeDaemon(machines);
    useFakeMachines(machines, 'local');
    const user = userEvent.setup();
    const opened: string[] = [];
    render(
      <AccountsPanel
        profiles={[]}
        machineName="This Mac"
        onOpenSession={(id) => opened.push(id)}
        onReload={() => {}}
      />
    );

    await user.click(screen.getByRole('button', { name: 'Add account' }));
    await user.type(screen.getByLabelText('Account name'), 'second');
    await user.type(screen.getByLabelText('Account label'), 'Second plan');
    expect(screen.getByText(/no API-key path/i)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Add and sign in' }));

    // The account was registered on this computer…
    await waitFor(() => expect(daemon.machines[0]!.profiles?.some(
      (account) => account.name === 'second' && account.label === 'Second plan'
    )).toBe(true));
    // …and the provider's own login was opened in that account's home.
    await waitFor(() => expect(opened).toHaveLength(1));
    // The request itself, not the fixture's summary of it: the login session
    // has to be created inside that account's home.
    const create = daemon.requests.filter(
      (request) => request.method === 'POST' && request.path === '/api/sessions'
    ).pop();
    const createBody = create?.body as { profile?: string; cmd?: string } | undefined;
    expect(createBody?.profile).toBe('second');
    expect(createBody?.cmd).toBe('claude');

    // The person is told to check which account they are signing in as.
    expect(await screen.findByText(/check which account you are signing in as/i)).toBeInTheDocument();
  }, 20_000);

  it('says an account is not signed in until the provider says otherwise', async () => {
    const machines = fleet();
    installFakeDaemon(machines);
    useFakeMachines(machines, 'local');
    render(
      <AccountsPanel
        profiles={[{ tool: 'claude', name: 'pending', label: 'Pending', path: '/state/profiles/claude/pending', signed_in: false, sessions: [], last_used: 0 }]}
        machineName="This Mac"
        onReload={() => {}}
      />
    );
    expect(screen.getByText('Not signed in yet')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Sign in' })).toBeEnabled();
  }, 20_000);

  it('removing an account says the provider home was left behind', async () => {
    const machines = fleet();
    installFakeDaemon(machines);
    useFakeMachines(machines, 'local');
    const user = userEvent.setup();
    render(
      <AccountsPanel
        profiles={[{ tool: 'claude', name: 'work', label: 'Work — team plan', path: '/state/profiles/claude/work', signed_in: true, sessions: [], last_used: 1 }]}
        machineName="This Mac"
        onReload={() => {}}
      />
    );
    await user.click(screen.getByRole('button', { name: 'Remove' }));
    const note = await screen.findByText(/no longer listed/);
    expect(note.textContent).toContain('/state/profiles/claude/work');
    expect(note.textContent).toMatch(/review/);
  }, 20_000);
});

describe('capability: a session says which account it is on', () => {
  it('badges a delegated lane that is not on the default account', () => {
    const onDefault = makeSession({ id: 'plain', name: 'Plain lane', tool: 'claude-code' });
    const onSecond = makeSession({ id: 'second', name: 'Second lane', tool: 'claude-code' });
    onSecond.profile = 'work';
    render(
      <SubagentsPanel
        manager={makeSession({ id: 'manager', name: 'Manager', tool: 'claude-code' })}
        subagents={[onDefault, onSecond]}
        onClose={() => {}}
        onOpen={() => {}}
        onMakeMain={async () => {}}
        onEnd={async () => {}}
        onHandBack={async () => {}}
        onApprove={async () => {}}
      />
    );
    const badges = screen.getAllByTitle(/uses the work account/);
    expect(badges).toHaveLength(1);
    expect(badges[0]!.textContent).toBe('work');
  }, 20_000);
});
