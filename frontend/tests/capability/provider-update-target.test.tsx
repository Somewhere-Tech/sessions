// CAPABILITY: an explicitly paired client can update a provider on another
// machine without mistaking the destination. The button names the host before
// the click, and the request is sent to that host rather than localhost.
import { describe, expect, it } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ProductSidebar } from '../../src/components/ProductSidebar';
import { installFakeDaemon, useFakeMachines, type FakeMachine } from './fake-daemon';

describe('capability: update an agent tool on a paired machine', () => {
  it('names the destination and runs the update there', async () => {
    const mini: FakeMachine = {
      id: 'mini',
      name: 'Studio Mini',
      host: '10.0.0.8',
      port: 8787,
      sessions: [],
      providers: [{
        id: 'codex',
        name: 'Codex',
        installed: true,
        version: '0.146.1',
        latestVersion: '0.151.0',
        updateAvailable: true
      }]
    };
    const daemon = installFakeDaemon([mini]);
    useFakeMachines([mini], mini.id);
    const user = userEvent.setup();

    render(
      <ProductSidebar
        active="tabs"
        theme="dark"
        onNavigate={() => {}}
        onNewSession={() => {}}
        onOpenCommandPalette={() => {}}
        onToggleTheme={() => {}}
      />
    );

    const update = await screen.findByRole('button', {
      name: 'Update Codex on Studio Mini'
    }, { timeout: 3_000 });
    expect(screen.getByText('Update available')).toBeInTheDocument();
    expect(screen.getByText('Studio Mini · Running work keeps going.')).toBeInTheDocument();
    await user.click(update);

    await waitFor(() => {
      expect(daemon.requests.some((request) =>
        request.origin === 'http://10.0.0.8:8787'
        && request.method === 'POST'
        && request.path === '/api/providers/codex/update'
      )).toBe(true);
    });
  });
});
