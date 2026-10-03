// CAPABILITY: starting delegated work can fail halfway without being started
// twice, and a start that stalls says where and what the safe next step is.
//
// Creating a session and delivering its first request are two operations. A
// lost create response used to leave the person pressing Start again and
// getting a second session; an auth failure read as an idle session. These
// tests hold the outcomes a person sees.
import { useState } from 'react';
import { describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { NewSessionDialog } from '../../src/components/NewSessionDialog';
import { ProviderFaultCard } from '../../src/components/ProviderFaultCard';
import { startNotice } from '../../src/lib/startReceipt';
import { recordedPromptOperationId, startFailureMessage, startOperationIds, withStartOperation } from '../../src/lib/startOperation';
import { DaemonResponseError } from '../../src/api/sessionsd/core';
import type { StartReceipt } from '../../src/types';
import { Workbench } from './harness';
import { InputBar } from '../../src/components/InputBar';
import { NEW_SESSION_DEFAULTS_KEY } from '../../src/lib/newSessionDefaults';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from './fake-daemon';

function machineWithFolder(): FakeMachine {
  return {
    id: 'local', name: 'Fixture Mac', host: 'localhost', port: 8787, isDefault: true,
    sessions: [makeSession({ id: 'existing', name: 'Already running' })],
    directories: [{ path: '/Users/example/project', label: 'project', kind: 'project' }]
  };
}

function Launcher({ conversation = false }: { conversation?: boolean }): JSX.Element {
  const [open, setOpen] = useState(true);
  const [sessionId, setSessionId] = useState<string | null>(null);
  return <Workbench>{open ? <NewSessionDialog onClose={() => setOpen(false)} onStarted={setSessionId} embedded /> : null}
    {conversation && sessionId ? <InputBar sessionId={sessionId} draftMachineId="local" connected send={async () => {}} submitMessage={async () => {}} /> : null}
  </Workbench>;
}

describe('capability: a start that fails halfway is not started twice', () => {
  it('defaults to YOLO even when an old client saved its implicit Ask me choice', async () => {
    window.localStorage.setItem(NEW_SESSION_DEFAULTS_KEY, JSON.stringify({ skipPerms: false }));
    const machine = machineWithFolder();
    const daemon = installFakeDaemon([machine]); useFakeMachines([machine]);
    const user = userEvent.setup(); render(<Launcher />);
    const start = await screen.findByRole('button', { name: 'Start session' });
    await waitFor(() => expect(start).toBeEnabled());
    expect(screen.getByRole('combobox', { name: 'Access' })).toHaveValue('full');
    await user.click(start);
    await waitFor(() => expect(daemon.created).toHaveLength(1));
    const body = daemon.requests.find((request) => request.method === 'POST' && request.path === '/api/sessions')!.body as { args: string[]; claude: { permissionMode: string } };
    expect(body.claude.permissionMode).toBe('bypassPermissions');
  });

  it('opens the chat before a slow first delivery finishes and settles its unchanged draft once', async () => {
    const machine = { ...machineWithFolder(), submitDelayMS: 1500 };
    const daemon = installFakeDaemon([machine]); useFakeMachines([machine]);
    const user = userEvent.setup(); render(<Launcher conversation />);
    const start = await screen.findByRole('button', { name: 'Start session' });
    await waitFor(() => expect(start).toBeEnabled());
    await user.type(screen.getByRole('textbox', { name: 'First request (optional)' }), 'My one delegation');
    await user.click(start);
    await screen.findByText('Sending your first message…');
    expect(screen.queryByRole('button', { name: 'Start session' })).toBeNull();
    expect(screen.getByPlaceholderText(/Message Claude/)).toHaveValue('My one delegation');
    expect(screen.getByRole('button', { name: 'Send' })).toBeDisabled();
    await waitFor(() => expect(screen.getByPlaceholderText(/Message Claude/)).toHaveValue(''), { timeout: 3000 });
    expect(daemon.delivered[daemon.created[0]!.id]).toEqual(['My one delegation']);
    expect(daemon.requests.filter((request) => request.method === 'POST' && request.path.endsWith('/submit'))).toHaveLength(1);
  });
  it('sends the first request under the operation id recorded at create', async () => {
    const machine = machineWithFolder();
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine]);
    const user = userEvent.setup();
    render(<Launcher />);
    const start = await screen.findByRole('button', { name: 'Start session' });
    await waitFor(() => expect(start).toBeEnabled());
    await user.type(screen.getByRole('textbox', { name: 'First request (optional)' }), 'Fix the flaky test');
    await user.click(start);

    await waitFor(() => expect(daemon.delivered[daemon.created[0]!.id]).toEqual(['Fix the flaky test']));
    const create = daemon.requests.find((request) => request.method === 'POST' && request.path === '/api/sessions');
    const submit = daemon.requests.find((request) => request.method === 'POST' && request.path.endsWith('/submit'));
    const createBody = create?.body as { operation_id?: string; prompt_operation_id?: string };
    expect(createBody.operation_id).toMatch(/^[0-9a-f-]{36}$/);
    expect(createBody.prompt_operation_id).toMatch(/^[0-9a-f-]{36}$/);
    expect((submit?.body as { operation_id?: string }).operation_id).toBe(createBody.prompt_operation_id);
  });

  it('pressing Start again after a lost create response returns the same session', async () => {
    const machine = { ...machineWithFolder(), loseNextCreateResponse: true };
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine]);
    const user = userEvent.setup();
    render(<Launcher />);
    const start = await screen.findByRole('button', { name: 'Start session' });
    await waitFor(() => expect(start).toBeEnabled());
    await user.type(screen.getByRole('textbox', { name: 'First request (optional)' }), 'Fix the flaky test');
    await user.click(start);
    await waitFor(() => expect(daemon.created).toHaveLength(1));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Start session' })).toBeEnabled());
    expect(daemon.delivered[daemon.created[0]!.id]).toBeUndefined();

    await user.click(screen.getByRole('button', { name: 'Start session' }));
    await waitFor(() => expect(daemon.delivered[daemon.created[0]!.id]).toEqual(['Fix the flaky test']));
    expect(daemon.created).toHaveLength(1);
    const creates = daemon.requests.filter((request) => request.method === 'POST' && request.path === '/api/sessions');
    expect(creates).toHaveLength(2);
    expect((creates[1]!.body as { operation_id: string }).operation_id).toBe((creates[0]!.body as { operation_id: string }).operation_id);
  });

  it('a changed request after a lost response is new work, not a replay of the old', async () => {
    const machine = { ...machineWithFolder(), loseNextCreateResponse: true };
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine]);
    const user = userEvent.setup();
    render(<Launcher />);
    const start = await screen.findByRole('button', { name: 'Start session' });
    await waitFor(() => expect(start).toBeEnabled());
    await user.type(screen.getByRole('textbox', { name: 'First request (optional)' }), 'Fix the flaky test');
    await user.click(start);
    await waitFor(() => expect(daemon.created).toHaveLength(1));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Start session' })).toBeEnabled());

    await user.selectOptions(screen.getByRole('combobox', { name: 'Access' }), 'plan');
    await user.click(screen.getByRole('button', { name: 'Start session' }));
    await waitFor(() => expect(daemon.created).toHaveLength(2));
    const creates = daemon.requests.filter((request) => request.method === 'POST' && request.path === '/api/sessions');
    expect((creates[1]!.body as { operation_id: string }).operation_id).not.toBe((creates[0]!.body as { operation_id: string }).operation_id);
  });
});

describe('capability: start operation ids', () => {
  it('keeps a failed launch identifiable without promising old runtimes deduplicate retries', () => {
    const id = '90000000-0000-4000-8000-000000000001';
    const known = new DaemonResponseError(500, JSON.stringify({ session_id: id, error: 'Runner did not start' }), '');
    expect(startFailureMessage(known)).toContain(`sessions status ${id}`);
    expect(startFailureMessage(known)).toContain('Runner did not start');
    expect(startFailureMessage(new Error('Connection lost'))).toContain('may already exist');
    expect(startFailureMessage(new Error('Connection lost'))).toContain('older runtime may create a duplicate');
    expect(startFailureMessage(new DaemonResponseError(400, 'runner failed', ''))).toContain('may already exist');
  });

  it('reuse ids only for the identical request, and prefer the recorded first-request id on replay', () => {
    const first = startOperationIds(null, 'a');
    expect(startOperationIds(first, 'a')).toBe(first);
    expect(startOperationIds(first, 'b').create).not.toBe(first.create);
    expect(withStartOperation({ cmd: 'claude' }, first, false)).toEqual({ cmd: 'claude', operation_id: first.create, prompt_operation_id: undefined });
    expect(recordedPromptOperationId({ start: { phase: 'created', evidence: '', evidence_source: '', replayed: true, prompt_operation_id: 'recorded' } }, 'fresh')).toBe('recorded');
    expect(recordedPromptOperationId({ start: { phase: 'created', evidence: '', evidence_source: '', prompt_operation_id: 'recorded' } }, 'fresh')).toBe('fresh');
  });
});

describe('capability: a blocked login names Connect account', () => {
  it('offers Connect account on a Rich session, where no terminal exists', async () => {
    const connect = vi.fn();
    const user = userEvent.setup();
    render(
      <ProviderFaultCard sessionId="s" failureKind="auth" detail="Claude is not signed in" rich onOpenTerminal={() => {}} onConnectAccount={connect} />
    );
    expect(screen.getByText('Connect the account, then retry this turn')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Open Terminal' })).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Connect account' }));
    expect(connect).toHaveBeenCalledTimes(1);
  });

  it('keeps the terminal login beside Connect account for a terminal session', () => {
    render(
      <ProviderFaultCard sessionId="s" failureKind="auth" detail="Claude is not signed in" rich={false} onOpenTerminal={() => {}} onConnectAccount={() => {}} />
    );
    expect(screen.getByRole('button', { name: 'Connect account' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Open Terminal' })).toBeInTheDocument();
  });
});

function receipt(overrides: Partial<StartReceipt>): StartReceipt {
  return { phase: 'created', evidence: 'x', evidence_source: 'delivery-receipt', ...overrides };
}

describe('capability: the header says how far a start got', () => {
  it('distinguishes sending, not sent, refused, uncertain, delivered and blocked', () => {
    expect(startNotice({ exited: false, start: receipt({ prompt: { status: 'sending', retry: false } }) })?.tone).toBe('progress');
    expect(startNotice({ exited: false, start: receipt({ prompt: { status: 'not-sent', retry: true } }) })?.text).toMatch(/not sent/);
    const starting = startNotice({ exited: false, launching: true, start: receipt({ prompt: { status: 'not-sent', retry: true } }) });
    expect(starting?.tone).toBe('progress');
    expect(starting?.text).toMatch(/Wait for runner readiness/);
    expect(starting?.text).not.toMatch(/Send it from the composer/);
    expect(startNotice({ exited: false, start: receipt({ phase: 'prompt-not-delivered', prompt: { status: 'not-delivered', retry: true } }) })?.text).toMatch(/safe/);
    expect(startNotice({ exited: false, start: receipt({ phase: 'prompt-unknown', prompt: { status: 'unknown', retry: false } }) })?.text).toMatch(/did not resend/);
    expect(startNotice({ exited: false, start: receipt({ phase: 'prompt-delivered' }) })?.tone).toBe('progress');
    expect(startNotice({ exited: false, start: receipt({ phase: 'blocked', blocked_by: 'auth' }) })?.connectAccount).toBe(true);
    expect(startNotice({ exited: false, start: receipt({ phase: 'working' }) })).toBeNull();
    expect(startNotice({ exited: false })).toBeNull();
  });
});
