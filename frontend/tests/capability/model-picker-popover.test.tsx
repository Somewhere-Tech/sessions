import { useState } from 'react';
import { describe, expect, it } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ModelPicker } from '../../src/components/ModelPicker';

function Picker(): JSX.Element {
  const [value, setValue] = useState('');
  return (
    <div data-testid="clipping-parent" style={{ overflow: 'hidden', width: 180, height: 40 }}>
      <ModelPicker
        provider="codex"
        value={value}
        options={[{ id: 'gpt-6-astra', label: 'Astra' }]}
        onChange={setValue}
      />
    </div>
  );
}

describe('capability: model picker stays visible outside the composer', () => {
  it('portals the popover beyond clipping ancestors and keeps it in the viewport', async () => {
    const user = userEvent.setup();
    render(<Picker />);

    await user.click(screen.getByRole('button', { name: /Default/ }));
    const popover = await screen.findByRole('region', { name: 'Codex model picker' });

    expect(popover.parentElement).toBe(document.body);
    expect(popover.closest('[data-testid="clipping-parent"]')).toBeNull();
    expect(popover).toHaveStyle({ position: 'fixed' });
    expect(Number.parseFloat(popover.style.left)).toBeGreaterThanOrEqual(0);
    expect(Number.parseFloat(popover.style.top)).toBeGreaterThanOrEqual(0);
  });

  it('contains Escape and popover clicks instead of dismissing the enclosing dialog', async () => {
    const user = userEvent.setup();
    let enclosingDismissals = 0;
    const dismiss = (event: KeyboardEvent): void => {
      if (event.key === 'Escape' && !event.defaultPrevented) enclosingDismissals++;
    };
    window.addEventListener('keydown', dismiss);
    try {
      render(<Picker />);
      await user.click(screen.getByRole('button', { name: /Default/ }));
      await screen.findByRole('combobox', { name: 'Search Codex models' });
      await user.click(screen.getByRole('option', { name: /Astra/ }));
      expect(enclosingDismissals).toBe(0);

      await user.click(screen.getByRole('button', { name: /Astra/ }));
      fireEvent.keyDown(await screen.findByRole('combobox', { name: 'Search Codex models' }), { key: 'Escape' });
      expect(screen.queryByRole('region', { name: 'Codex model picker' })).not.toBeInTheDocument();
      expect(enclosingDismissals).toBe(0);
    } finally {
      window.removeEventListener('keydown', dismiss);
    }
  });

  it('caps the popover to the real space available in a short viewport', async () => {
    const originalHeight = window.innerHeight;
    Object.defineProperty(window, 'innerHeight', { configurable: true, value: 160 });
    try {
      const user = userEvent.setup();
      render(<Picker />);
      const trigger = screen.getByRole('button', { name: /Default/ });
      trigger.getBoundingClientRect = () => ({
        x: 100, y: 80, top: 80, right: 280, bottom: 112, left: 100,
        width: 180, height: 32, toJSON: () => ({})
      });

      await user.click(trigger);
      const popover = await screen.findByRole('region', { name: 'Codex model picker' });
      const top = Number.parseFloat(popover.style.top);
      const maxHeight = Number.parseFloat(popover.style.maxHeight);
      expect(top).toBeGreaterThanOrEqual(0);
      expect(top + maxHeight).toBeLessThanOrEqual(window.innerHeight);
      expect(maxHeight).toBeLessThan(180);
    } finally {
      Object.defineProperty(window, 'innerHeight', { configurable: true, value: originalHeight });
    }
  });
});
