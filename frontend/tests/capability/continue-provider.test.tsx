// CAPABILITY: a person can see and control exactly what another agent receives.
import { useState } from 'react';
import { describe, expect, it } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ResumeDialog } from '../../src/components/ResumeDialog';
import type { ContinuationJob } from '../../src/api/sessionsd';
import { Workbench } from './harness';
import { installFakeDaemon, useFakeMachines, type FakeMachine } from './fake-daemon';

const HISTORY_ID = 'codex-history-1';

function continuationMachine(): FakeMachine {
  return {
    id: 'local', name: 'Fixture Mac', host: 'localhost', port: 8787, isDefault: true, sessions: [],
    resumable: [{
      sessionId: HISTORY_ID, historyId: HISTORY_ID, tool: 'codex', origin: 'Codex',
      title: 'Frozen release plan', cwd: '/Users/example/project', modifiedAt: Date.now(),
      firstUserMessage: 'Finish the release plan', sizeBytes: 248_000
    }],
    providers: [{
      id: 'claude', name: 'Claude Code', installed: true, updateAvailable: false,
      models: [
        { id: 'sonnet-5', displayName: 'Sonnet 5', description: 'Balanced', hidden: false, isDefault: true, defaultReasoningEffort: 'medium', supportedReasoningEfforts: [{ reasoningEffort: 'medium', description: 'Medium' }, { reasoningEffort: 'high', description: 'High' }] },
        { id: 'opus-5', displayName: 'Opus 5', description: 'Most capable', hidden: false, isDefault: false, defaultReasoningEffort: 'high', supportedReasoningEfforts: [{ reasoningEffort: 'high', description: 'High' }] }
      ]
    }, { id: 'codex', name: 'Codex', installed: true, updateAvailable: false, models: [] }],
    continuationPreview: {
      conversation: 'Frozen release plan', sourceProvider: 'codex', destinationProvider: 'claude',
      totalMessageCount: 84, messageCount: 84, characterCount: 248_000,
      estimatedTokens: 62_000, thresholdTokens: 60_000, limited: false, sourceUntouched: true
    }
  };
}

function ResumeFlow({ machine, onResumed = () => {} }: { machine: FakeMachine; onResumed?: (id: string) => void }): JSX.Element {
  const [open, setOpen] = useState(true);
  return <Workbench>{open ? <ResumeDialog
    preferredProviderId={HISTORY_ID}
    preferredHistoryId={HISTORY_ID}
    preferredDestinationProvider="claude"
    onClose={() => setOpen(false)}
    onResumed={onResumed}
    onStartNew={() => {}}
  /> : null}<span data-testid="machine-name">{machine.name}</span></Workbench>;
}

function job(stage: ContinuationJob['stage'], events: ContinuationJob['events'], extra: Partial<ContinuationJob> = {}): ContinuationJob {
  return {
    id: 'continuation-1', status: 'running', stage, stageText: events.at(-1)?.text ?? '',
    provider: 'claude', model: 'sonnet-5', modelDisplayName: 'Sonnet 5', effort: 'medium',
    laneId: 'continued-1', events, ...extra
  };
}

describe('capability: continue with another provider', () => {
  it('shows the estimate, model choices, safety threshold, and a live last-message estimate', async () => {
    const machine = continuationMachine();
    installFakeDaemon([machine]);
    useFakeMachines([machine]);
    const user = userEvent.setup();
    render(<ResumeFlow machine={machine} />);

    expect(await screen.findByText('84 messages, about 62k tokens')).toBeInTheDocument();
    expect(screen.getByText('Your Codex conversation is not changed')).toBeInTheDocument();
    expect(screen.getByText('Nothing is sent until you press Start')).toBeInTheDocument();
    const start = screen.getByRole('button', { name: 'Start Claude (Sonnet 5) with this history · ~62k tokens' });
    expect(start).toBeDisabled();

    await user.click(screen.getByRole('button', { name: 'Model' }));
    expect(screen.getByRole('option', { name: /Opus 5/ })).toBeInTheDocument();
    await user.click(screen.getByRole('checkbox', { name: /Send the whole history anyway/ }));
    expect(start).toBeEnabled();
    await user.click(screen.getByRole('radio', { name: /Only the last/ }));
    expect(await screen.findByText('40 messages, about 30k tokens')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /Start Claude \(Sonnet 5\).*~30k tokens/ })).toBeEnabled();
  });

  it('shows every stage and provider fault, then ends only the new session on Cancel', async () => {
    const machine = continuationMachine();
    machine.continuationPreview = { ...machine.continuationPreview!, characterCount: 48_000, estimatedTokens: 12_000 };
    const exporting = [{ stage: 'exporting-history' as const, text: 'Exporting conversation history', at: 1 }];
    const creating = [...exporting, { stage: 'creating-session' as const, text: 'Creating the new Claude session', at: 2 }];
    const starting = [...creating, { stage: 'provider-starting' as const, text: 'Claude is starting', at: 3 }];
    const replying = [...starting, { stage: 'first-reply' as const, text: 'Waiting for Claude to answer', at: 4 }];
    machine.continuationJobs = [
      job('exporting-history', exporting), job('creating-session', creating),
      job('provider-starting', starting),
      job('first-reply', replying, { failureKind: 'auth', failureDetail: 'Claude needs you to sign in.' })
    ];
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine]);
    const user = userEvent.setup();
    render(<ResumeFlow machine={machine} />);

    await user.click(await screen.findByRole('button', { name: /Start Claude \(Sonnet 5\)/ }));
    expect(await screen.findByText('Exporting conversation history')).toBeInTheDocument();
    expect(await screen.findByRole('group', { name: 'Provider trouble' }, { timeout: 3_000 })).toHaveTextContent('Claude needs you to sign in.');
    expect(screen.getByText('Creating the new session')).toBeInTheDocument();
    expect(screen.getByText('Starting the agent')).toBeInTheDocument();
    expect(screen.getByText('Waiting for the first reply')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(await screen.findByText('Continuation canceled')).toBeInTheDocument();
    expect(daemon.continuationCanceled).toEqual(['continuation-1']);
    expect(daemon.ended).toEqual(['continued-1']);
  });

  it('opens the new conversation after its first reply', async () => {
    const machine = continuationMachine();
    machine.continuationPreview = { ...machine.continuationPreview!, characterCount: 48_000, estimatedTokens: 12_000 };
    const events = [{ stage: 'exporting-history' as const, text: 'Exporting conversation history', at: 1 }];
    machine.continuationJobs = [job('exporting-history', events), job('first-reply', events), job('first-reply', events, { status: 'succeeded', stageText: 'Claude answered' })];
    installFakeDaemon([machine]);
    useFakeMachines([machine]);
    let opened = '';
    const user = userEvent.setup();
    render(<ResumeFlow machine={machine} onResumed={(id) => { opened = id; }} />);

    await user.click(await screen.findByRole('button', { name: /Start Claude \(Sonnet 5\)/ }));
    await waitFor(() => expect(opened).toBe('continued-1'), { timeout: 3_000 });
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });
});
