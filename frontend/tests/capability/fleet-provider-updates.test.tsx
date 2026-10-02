import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { FleetProviderUpdates } from '../../src/components/FleetProviderUpdates';
import { useServers } from '../../src/lib/servers';

describe('capability: provider updates keep per-computer outcomes', () => {
  it('names the machine that answered, keeps partial failures, and never changes active scope', async () => {
    useServers.setState({ activeId: 'one', servers: [
      { id: 'one', name: 'MacBook', host: '127.0.0.1', port: 8891, isDefault: true },
      { id: 'two', name: 'Studio', host: '127.0.0.1', port: 8892, isDefault: false },
      { id: 'three', name: 'Unpaired', host: '127.0.0.1', port: 8893, isDefault: false, directoryOnly: true }
    ] });
    let finish!: () => void;
    const pending = new Promise<void>((resolve) => { finish = resolve; });
    const fetch = vi.fn(async (url: string, options: RequestInit) => {
      expect(options.method).toBe('POST');
      expect(url).toMatch(/\/api\/providers\/codex\/update$/);
      await pending;
      return String(url).includes('8891')
        ? new Response(JSON.stringify({ provider: { id: 'codex', installed: true, version: 'fixture-new' } }))
        : new Response(JSON.stringify({ error: 'Pair Studio again' }), { status: 403 });
    });
    vi.stubGlobal('fetch', fetch);
    render(<FleetProviderUpdates />);
    fireEvent.click(screen.getByRole('button', { name: 'Update Codex everywhere' }));
    expect(screen.getByRole('button', { name: 'Update Claude everywhere' })).toBeDisabled();
    await act(async () => { useServers.setState({ activeId: 'two' }); finish(); });
    await screen.findByText('fixture-new');
    await waitFor(() => expect(screen.getByText(/Pair Studio again/)).toBeInTheDocument());
    expect(screen.getByText('Needs checking')).toBeInTheDocument();
    expect(screen.getByText('Pair this computer first.')).toBeInTheDocument();
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(useServers.getState().activeId).toBe('two');
  });
});
