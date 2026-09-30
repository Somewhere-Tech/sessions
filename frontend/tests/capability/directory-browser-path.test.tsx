import { describe, expect, it } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DirectoryBrowser } from '../../src/components/DirectoryBrowser';
import { makeSession, useFakeMachines, type FakeMachine } from './fake-daemon';

function machine(): FakeMachine {
  return {
    id: 'local', name: 'Fixture Mac', host: 'localhost', port: 8787, isDefault: true,
    sessions: [makeSession({ id: 'existing' })]
  };
}

describe('capability: directory browser confirms typed paths', () => {
  it('retains the whole draft and loads that exact path before selection', async () => {
    useFakeMachines([machine()]);
    const requested: string[] = [];
    const originalFetch = globalThis.fetch;
    globalThis.fetch = async (input) => {
      const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url);
      if (url.pathname === '/api/fs/list') {
        const path = url.searchParams.get('path') || '/Users/example';
        requested.push(path);
        return new Response(JSON.stringify({ path, parent: '/', entries: [] }), {
          status: 200, headers: { 'content-type': 'application/json' }
        });
      }
      return originalFetch(input);
    };
    const changes: Array<{ path: string; confirmed?: boolean }> = [];
    try {
      const user = userEvent.setup();
      render(<DirectoryBrowser value="/Users/example" serverId="local" onChange={(path, confirmed) => changes.push({ path, confirmed })} />);
      const field = await screen.findByRole('textbox');
      await waitFor(() => expect(field).toHaveValue('/Users/example'));

      await user.clear(field);
      await user.type(field, '/tmp/full-path');
      expect(field).toHaveValue('/tmp/full-path');
      expect(screen.getByRole('button', { name: 'Select' })).toBeDisabled();

      await user.keyboard('{Enter}');
      await waitFor(() => expect(requested.at(-1)).toBe('/tmp/full-path'));
      await waitFor(() => expect(screen.getByRole('button', { name: 'Select' })).toBeEnabled());
      await user.click(screen.getByRole('button', { name: 'Select' }));
      expect(changes.at(-1)).toEqual({ path: '/tmp/full-path', confirmed: true });
    } finally {
      globalThis.fetch = originalFetch;
    }
  });
});
