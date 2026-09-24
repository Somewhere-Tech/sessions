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
  it('signs in from the launcher without creating a login chat or losing the first request', async () => {
    const machines = fleet();
    const daemon = installFakeDaemon(machines);
    useFakeMachines(machines, 'local');
    const user = userEvent.setup();
    render(<NewSessionDialog onClose={() => {}} onStarted={() => {}}
      projectSeed={{ serverId: 'local', cwd: '/project', tags: {} }} />);
    const account = await screen.findByLabelText('Account');
    const add = within(account).getByRole('option', { name: /Add an account/ }) as HTMLOptionElement;
    await user.selectOptions(account, add.value);
    await user.type(screen.getByLabelText('First request (optional)'), 'Keep this request for the real chat');
    await user.click(screen.getByRole('button', { name: 'Sign in to Claude' }));
    await screen.findByLabelText('Claude confirmation code');
    expect(screen.getByRole('button', { name: 'Start session' })).toBeDisabled();
    await user.type(screen.getByLabelText('Claude confirmation code'), 'fixture-code{Enter}');
    expect(await within(screen.getByRole('region', { name: 'Sign in to account' })).findByText('second@example.test')).toBeVisible();
    expect(daemon.created).toHaveLength(0);
    await user.click(screen.getByRole('button', { name: 'Done' }));
    await waitFor(() => expect((account as HTMLSelectElement).value).toBe('acct-fixture'));
    expect(screen.getByRole('button', { name: 'Start session' })).toBeEnabled();
    expect(screen.getByLabelText('First request (optional)')).toHaveValue('Keep this request for the real chat');
    expect(daemon.created).toHaveLength(0);
  });
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
    expect(screen.queryByLabelText('Account name')).not.toBeInTheDocument();
    await user.type(screen.getByLabelText('Account label'), 'Second plan');
    expect(screen.getByText(/No API key needed/i)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Continue' }));

    // The account was registered on this computer…
    await waitFor(() => expect(daemon.machines[0]!.profiles?.some(
      (account) => account.name === 'acct-fixture' && account.label === 'Second plan'
    )).toBe(true));
    // …and the provider's own login was opened in that account's home.
    await screen.findByRole('button', { name: 'Continue to Claude' });
    expect(opened).toHaveLength(0);
    // The request itself, not the fixture's summary of it: the login session
    // has to be created inside that account's home.
    const create = daemon.requests.filter(
      (request) => request.method === 'POST' && request.path === '/api/account-logins'
    ).pop();
    const createBody = create?.body as { profile?: string; tool?: string } | undefined;
    expect(createBody?.profile).toBe('acct-fixture');
    expect(createBody?.tool).toBe('claude');
    expect(daemon.created).toHaveLength(0);

    // The person is told to check which account they are signing in as.
    expect(await screen.findByText(/Check that you choose the account/i)).toBeInTheDocument();
    await user.type(screen.getByLabelText('Claude confirmation code'), 'fixture-code');
    await user.click(screen.getByRole('button', { name: 'Connect account' }));
    expect(await screen.findByText('second@example.test')).toBeVisible();
  }, 20_000);

  it('says only what it can see: whether a login file is there', async () => {
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
    // Presence of a provider's sign-in file is the whole fact. "Signed in"
    // would claim a working login Sessions has never checked.
    expect(screen.getByText('Identity not checked')).toBeInTheDocument();
    expect(screen.queryByText(/Not signed in/)).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Sign in' })).toBeEnabled();
  }, 20_000);

  it('removing an account explains that saved chats and sign-in are unchanged', async () => {
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
    const note = await screen.findByText(/was removed from this list/);
    expect(note.textContent).toContain('Work — team plan');
    expect(note.textContent).toMatch(/saved chats and sign-in are unchanged/);
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

  it('reports a login file it can see without calling it a working login', () => {
    const machines = fleet();
    installFakeDaemon(machines);
    useFakeMachines(machines, 'local');
    render(
      <AccountsPanel
        profiles={[{ tool: 'claude', name: 'work', label: 'Work', path: '/state/profiles/claude/work', signed_in: true, sessions: [], last_used: 1 }]}
        machineName="This Mac"
        onReload={() => {}}
      />
    );
    const state = screen.getByText('Identity not checked');
    expect(state).toBeInTheDocument();
    expect(state.title).toMatch(/confirm who is signed in/);
    expect(screen.queryByText('Signed in')).not.toBeInTheDocument();
  }, 20_000);

  // A sign-in that cannot be opened must say so, once, and leave the panel
  // where it was — not stranded on "finish signing in" for a session that was
  // never created.
  it('says when the provider sign-in could not be opened, and opens one session per click', async () => {
    const machines = fleet();
    installFakeDaemon(machines);
    useFakeMachines(machines, 'local');
    const realFetch = globalThis.fetch;
    let creates = 0;
    globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input).endsWith('/api/account-logins') && (init?.method ?? 'GET') === 'POST') {
        creates += 1;
        return new Response(JSON.stringify({ error: 'no runner is available' }), {
          status: 503, headers: { 'content-type': 'application/json' }
        });
      }
      return realFetch(input, init);
    }) as typeof fetch;
    const user = userEvent.setup();
    const opened: string[] = [];
    render(
      <AccountsPanel
        profiles={[{ tool: 'claude', name: 'pending', label: 'Pending', path: '/state/profiles/claude/pending', signed_in: false, sessions: [], last_used: 0 }]}
        machineName="This Mac"
        onOpenSession={(id) => opened.push(id)}
        onReload={() => {}}
      />
    );

    const signIn = screen.getByRole('button', { name: 'Sign in' });
    await user.click(signIn);
    await waitFor(() => expect(screen.getByRole('status').textContent).toMatch(/no runner is available|could not open/));
    // Not stranded on the signing-in step, and no session was reported.
    expect(screen.queryByText(/Finish signing in/)).not.toBeInTheDocument();
    expect(opened).toEqual([]);
    expect(screen.getByRole('button', { name: 'Add account' })).toBeInTheDocument();
    expect(creates).toBe(1);

    // And a second click is a second attempt, not a second session per click.
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    await waitFor(() => expect(creates).toBe(2));
    globalThis.fetch = realFetch;
  }, 20_000);

  // Two clicks while the first one is still in flight are one login session.
  it('opens one login session even when the button is hit twice', async () => {
    const machines = fleet();
    installFakeDaemon(machines);
    useFakeMachines(machines, 'local');
    const realFetch = globalThis.fetch;
    let creates = 0;
    globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input).endsWith('/api/account-logins') && (init?.method ?? 'GET') === 'POST') {
        creates += 1;
        // Slow enough that a second click lands while the first is unfinished.
        await new Promise((resolve) => setTimeout(resolve, 400));
      }
      return realFetch(input, init);
    }) as typeof fetch;
    const user = userEvent.setup();
    const opened: string[] = [];
    render(
      <AccountsPanel
        profiles={[{ tool: 'claude', name: 'pending', label: 'Pending', path: '/state/profiles/claude/pending', signed_in: false, sessions: [], last_used: 0 }]}
        machineName="This Mac"
        onOpenSession={(id) => opened.push(id)}
        onReload={() => {}}
      />
    );
    const signIn = screen.getByRole('button', { name: 'Sign in' });
    await user.click(signIn);
    await user.click(signIn);
    await screen.findByRole('button', { name: 'Continue to Claude' });
    expect(opened).toHaveLength(0);
    await new Promise((resolve) => setTimeout(resolve, 600));
    expect(creates).toBe(1);
    globalThis.fetch = realFetch;
  }, 20_000);
});
