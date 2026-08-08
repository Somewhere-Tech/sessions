// CAPABILITY: a person can find something they said, in a conversation they
// half-remember.
//
// The outcome is the result on screen — the conversation named, the sentence
// quoted back, and the machine it is on — not that /api/search was requested.
import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { SearchView } from '../../src/components/SearchView';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from './fake-daemon';

function localMachine(): FakeMachine {
  return {
    id: 'local',
    name: 'Fixture Mac',
    host: 'localhost',
    port: 8787,
    isDefault: true,
    sessions: [makeSession({ id: 'drafts', name: 'Drafts rollout' })],
    searchCorpus: [
      {
        sessionId: 'drafts',
        name: 'Drafts rollout',
        tool: 'claude',
        role: 'user',
        text: 'the drafts rollout should ship behind a flag first'
      },
      {
        sessionId: 'drafts',
        name: 'Drafts rollout',
        tool: 'claude',
        role: 'assistant',
        text: 'Noted — flag first, then a staged rollout.'
      }
    ]
  };
}

describe('capability: search', () => {
  it('finds the conversation and quotes back the sentence that matched', async () => {
    const machine = localMachine();
    installFakeDaemon([machine]);
    useFakeMachines([machine]);
    const user = userEvent.setup();

    render(<SearchView onResumeConversation={async () => {}} />);

    await user.type(
      await screen.findByPlaceholderText(/Search a chat title/),
      'rollout'
    );
    await user.click(screen.getByRole('button', { name: 'Search' }));

    // The conversation the person was thinking of.
    expect(await screen.findByText('Drafts rollout')).toBeInTheDocument();
    // The sentence, in their own words, so they can tell it is the right one.
    expect(
      await screen.findByText(/the drafts rollout should ship behind a flag first/)
    ).toBeInTheDocument();
    // And a way in.
    expect(await screen.findByRole('button', { name: /Open conversation/ })).toBeInTheDocument();
  });

  it('says so plainly when nothing matches', async () => {
    const machine = localMachine();
    installFakeDaemon([machine]);
    useFakeMachines([machine]);
    const user = userEvent.setup();

    render(<SearchView onResumeConversation={async () => {}} />);

    await user.type(
      await screen.findByPlaceholderText(/Search a chat title/),
      'quarterly badger census'
    );
    await user.click(screen.getByRole('button', { name: 'Search' }));

    expect(await screen.findByText('No matching conversations.')).toBeInTheDocument();
  });
});
