import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import StatusSidebar, { type SidebarProps } from '../../src/components/StatusSidebar';

const status: SidebarProps = {
  parserName: 'Codex', parserIcon: '✻', isWorking: true, timer: '7s', tokens: '70.3k',
  context: '27% of 258.4k', finalElapsed: '', currentTask: '', checklist: [], statusLabel: 'Ready'
};

describe('capability: provider-neutral activity footer', () => {
  it('uses the Codex icon, not a Claude star, while retaining readable metrics', () => {
    const { container } = render(<StatusSidebar {...status} />);
    expect(screen.getByLabelText('Agent working')).toHaveTextContent('Working');
    expect(container.querySelector('.provider-mark.is-codex img')).toHaveAttribute('src', '/openai-icon.svg');
    expect(screen.queryByText('✻')).not.toBeInTheDocument();
    expect(screen.getByText('70.3k')).toBeInTheDocument();
    expect(screen.getByText('27% of 258.4k')).toBeInTheDocument();
  });

  it('keeps provider identity separate from working and completed states', () => {
    const view = render(<StatusSidebar {...status} parserName="Claude" />);
    expect(view.container.querySelector('.provider-mark.is-claude img')).toHaveAttribute('src', '/claude-icon.svg');
    view.rerender(<StatusSidebar {...status} isWorking={false} finalElapsed="12s" />);
    expect(screen.getByLabelText('Ready')).toHaveTextContent('Ready · 12s');
    expect(screen.queryByText('elapsed')).not.toBeInTheDocument();
    expect(view.container.querySelector('.provider-mark.is-codex')).toBeInTheDocument();
  });
});
