import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { TeamEvidence } from '../../src/components/TeamEvidence';
import type { TeamMember } from '../../src/api/sessionsd';
import { makeSession } from './fake-daemon';

const member: TeamMember = {
  id: 'worker', tool: 'codex', relation: 'child', depth: 1,
  state: 'idle', needs_you: false, working: false, exited: false
};

describe('delegation evidence', () => {
  it('distinguishes a finished turn from a missing handoff', async () => {
    const user = userEvent.setup();
    render(<TeamEvidence session={makeSession({ id: 'worker', idleReason: 'completed' })} member={{
      ...member, handoff: { source: 'not-reported', push: 'unknown', workspace: '/work' }
    }} />);
    await user.click(screen.getByText('Handoff · not reported'));
    expect(screen.getByText(/A finished turn does not prove/)).toBeVisible();
    expect(screen.getAllByText('Not reported')).toHaveLength(5);
    expect(screen.queryByText('Reported pushed')).not.toBeInTheDocument();
  });

  it('shows shared-checkout risk without opening a details disclosure', () => {
    render(<TeamEvidence session={makeSession({ id: 'worker' })} member={{ ...member, checkout_warning: {
      dirty: true, sessions: ['a', 'b'], detail: 'Multiple sessions share uncommitted changes. Coordinate writes.'
    } }} />);
    expect(screen.getByRole('status')).toBeVisible();
    expect(screen.getByRole('status')).toHaveTextContent('Coordinate writes');
  });

  it('labels tests, artifacts and commits as agent-reported', async () => {
    const user = userEvent.setup();
    render(<TeamEvidence session={makeSession({ id: 'worker' })} member={{ ...member, handoff: {
      source: 'agent-reported', outcome: 'done', push: 'not-pushed', summary: 'Fixed delivery',
      commits: ['abc123'], tests: ['go test passed'], artifacts: ['report.md'], remaining: ['native check']
    } }} />);
    await user.click(screen.getByText('Handoff · done'));
    expect(screen.getByText(/not independently verified/)).toBeVisible();
    for (const value of ['abc123', 'go test passed', 'report.md', 'native check', 'Not pushed']) {
      expect(screen.getByText(value)).toBeVisible();
    }
  });
});
