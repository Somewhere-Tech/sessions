// CAPABILITY: a person can pick up a conversation they had before.
//
// The outcome is not "adopt returned ok". It is that the conversation they
// chose is now a session they can open, under the name they recognised it by.
import { useState } from 'react';
import { describe, expect, it } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ResumeDialog } from '../../src/components/ResumeDialog';
import { SessionHistoryView } from '../../src/components/SessionHistoryView';
import { resumeExactSession } from '../../src/lib/resumeExactSession';
import { Workbench } from './harness';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from './fake-daemon';

const PROVIDER_UUID = '0f5c2f7a-1b8e-4a52-9c31-6d0a2b3c4d5e';

function localMachine(): FakeMachine {
  return {
    id: 'local',
    name: 'Fixture Mac',
    host: 'localhost',
    port: 8787,
    isDefault: true,
    sessions: [],
    resumable: [{
      sessionId: PROVIDER_UUID,
      tool: 'claude',
      origin: 'Claude Code',
      title: 'Thursday migration plan',
      cwd: '/Users/example/project',
      modifiedAt: Date.UTC(2026, 0, 15, 9, 0, 0),
      firstUserMessage: 'Walk me through the migration plan',
      sizeBytes: 48_000
    }]
  };
}

// The dialog closes itself on success, exactly as App.tsx lets it.
function ResumeFlow({ onResumed }: { onResumed: (laneId: string) => void }): JSX.Element {
  const [open, setOpen] = useState(true);
  return (
    <Workbench>
      {open ? (
        <ResumeDialog
          onClose={() => setOpen(false)}
          onResumed={onResumed}
          onStartNew={() => {}}
        />
      ) : null}
    </Workbench>
  );
}

describe('capability: resume a conversation', () => {
  it('reopens the chosen conversation as a session in the list', async () => {
    const machine = localMachine();
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine]);
    const user = userEvent.setup();

    let resumedLaneId: string | null = null;
    render(<ResumeFlow onResumed={(laneId) => { resumedLaneId = laneId; }} />);

    await user.click(await screen.findByRole('button', { name: /Thursday migration plan/ }));
    await user.click(await screen.findByRole('button', { name: 'Resume with Claude' }));

    // The daemon bound the same provider conversation the person picked.
    await waitFor(() => expect(daemon.adopted).toEqual([PROVIDER_UUID]));
    expect(resumedLaneId).toBeTruthy();

    // And it is a session they can now open. The row is identified by the lane
    // the daemon returned rather than by its label: what the resumed lane ends
    // up called is the daemon's decision, and asserting it here would only be
    // asserting against this file's own fake.
    const rows = await screen.findAllByRole('treeitem');
    expect(rows).toHaveLength(1);
    expect(rows[0]).toHaveAttribute('data-session-id', resumedLaneId);
  });

  it('resumes an ended row directly without asking which conversation again', async () => {
    const ended = makeSession({
      id: 'ended-runtime',
      name: 'Quarterly plan',
      conversationId: PROVIDER_UUID,
      exited: true,
      exitCode: 0,
      exitedAt: Date.now() - 60_000
    });
    const machine = localMachine();
    machine.sessions = [ended];
    machine.history = [{
      id: ended.id,
      name: ended.name ?? 'Quarterly plan',
      tool: 'claude',
      provider_session_id: PROVIDER_UUID,
      cwd: ended.cwd,
      machine: machine.name,
      created_at: ended.createdAt,
      last_activity_at: ended.lastDataAt,
      message_count: 1,
      conversation_available: true
    }];
    machine.transcripts = {
      [ended.id]: Array.from({ length: 90 }, (_, index) => ({
        index,
        id: `message-${index}`,
        role: 'user' as const,
        text: `Plan message ${index}`,
        timestamp: null
      }))
    };
    const daemon = installFakeDaemon([machine]);
    useFakeMachines([machine]);
    const user = userEvent.setup();

    render(
      <Workbench>
        <SessionHistoryView
          session={ended}
          onResume={async (session) => { await resumeExactSession(session); }}
        />
      </Workbench>
    );

    // A version-skewed daemon may ignore the additive preview limit. The
    // client still keeps the initial DOM bounded and shows the newest work.
    expect(await screen.findByText('Plan message 89')).toBeInTheDocument();
    expect(screen.queryByText('Plan message 0')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Show earlier messages' })).toBeInTheDocument();

    await user.click(await screen.findByRole('button', { name: /Resume conversation/ }));
    await waitFor(() => expect(daemon.adopted).toEqual([PROVIDER_UUID]));

    // The generic picker never appeared. One click acted on the row already
    // in view instead of asking the person to identify it a second time.
    expect(screen.queryByText('Continue an earlier chat')).not.toBeInTheDocument();
  });
});
