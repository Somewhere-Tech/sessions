// CAPABILITY: delegated work recedes under its manager without becoming
// inaccessible. A person can open a helper or promote it to the main list from
// the manager's Subagents panel.
import { describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { SubagentsPanel } from '../../src/components/SubagentsPanel';
import { makeSession } from './fake-daemon';

describe('capability: manage delegated work from its manager', () => {
  it('opens a helper and can promote it to a main session', async () => {
    const user = userEvent.setup();
    const onOpen = vi.fn();
    const onMakeMain = vi.fn().mockResolvedValue(undefined);

    render(
      <SubagentsPanel
        manager={makeSession({ id: 'manager', name: 'Platform review' })}
        subagents={[
          makeSession({
            id: 'helper',
            name: 'Audit API routes',
            description: 'Map the public routes and authentication boundaries.',
            parentSessionId: 'manager',
            delegationKind: 'agent',
            working: true
          })
        ]}
        onClose={() => {}}
        onOpen={onOpen}
        onMakeMain={onMakeMain}
      />
    );

    expect(screen.getByText('Work delegated by')).toHaveTextContent('Platform review');
    expect(screen.getByText('Map the public routes and authentication boundaries.')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Open' }));
    expect(onOpen).toHaveBeenCalledWith('helper');

    await user.click(screen.getByRole('button', { name: 'Make main session' }));
    expect(onMakeMain).toHaveBeenCalledWith('helper');
  });
});
