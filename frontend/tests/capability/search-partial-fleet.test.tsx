// CAPABILITY: a machine that is off does not hide the history of the machine
// in front of you, and its absence is said out loud rather than left as a gap.
//
// The reported failure: a fleet-wide read waited on a paired Mini that was
// mid-install, so the person's own local history took sixteen seconds to appear
// and nothing on screen said why.
import { describe, expect, it } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { SearchView } from '../../src/components/SearchView';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from './fake-daemon';

function fleet(): FakeMachine[] {
  return [
    {
      id: 'local', name: 'This Mac', host: 'localhost', port: 8787, isDefault: true,
      sessions: [makeSession({ id: 'drafts', name: 'Drafts rollout' })],
      searchCorpus: [{
        sessionId: 'drafts', name: 'Drafts rollout', tool: 'claude', role: 'user',
        text: 'the drafts rollout should ship behind a flag first'
      }]
    },
    {
      id: 'mini', name: 'Mac mini', host: '10.0.0.9', port: 8787,
      sessions: [makeSession({ id: 'mini-work', name: 'Index rebuild' })],
      searchCorpus: [{
        sessionId: 'mini-work', name: 'Index rebuild', tool: 'codex', role: 'user',
        text: 'the drafts rollout index needs rebuilding'
      }]
    }
  ];
}

// stallMini holds every request to the Mini until the test releases it, the way
// a machine mid-install holds a connection open and never answers.
function stallMini(): { release: () => void } {
  const realFetch = globalThis.fetch;
  let release!: () => void;
  const held = new Promise<void>((resolve) => { release = resolve; });
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    if (String(input).includes('10.0.0.9')) {
      await held;
      throw new DOMException('The operation timed out.', 'TimeoutError');
    }
    return realFetch(input, init);
  }) as typeof fetch;
  return { release };
}

describe('capability: one machine off does not hide the rest', () => {
  it('shows local results while a peer is still stalling', async () => {
    const machines = fleet();
    installFakeDaemon(machines);
    useFakeMachines(machines, 'local');
    const stalled = stallMini();
    const user = userEvent.setup();

    render(<SearchView onResumeConversation={async () => {}} />);
    await user.type(await screen.findByPlaceholderText(/Search a chat title/), 'drafts rollout');
    await user.click(screen.getByRole('button', { name: 'Search' }));

    // The local machine's answer is on screen while the Mini is still held.
    await waitFor(() => expect(screen.getByText(/Drafts rollout/)).toBeInTheDocument());
    expect(screen.queryByText(/Index rebuild/)).not.toBeInTheDocument();

    // Past its budget the Mini is reported, not omitted, and can be retried.
    stalled.release();
    await waitFor(() => expect(screen.getByText(/Mac mini did not answer in time/)).toBeInTheDocument());
    expect(screen.getByRole('button', { name: 'Try again' })).toBeEnabled();
    // Its absence is named as absence, never as an empty history.
    expect(screen.getByText(/Drafts rollout/)).toBeInTheDocument();
  });
});
