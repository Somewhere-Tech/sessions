import { describe, expect, it } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { ConversationBrowser } from '../../src/components/ConversationBrowser';
import { DEFAULT_BROWSE_FILTERS } from '../../src/lib/conversationBrowser';
import { installFakeDaemon, useFakeMachines, type FakeMachine } from './fake-daemon';

describe('capability: browse history while another computer is slow', () => {
  it('shows local conversations before a remote history request finishes', async () => {
    const local: FakeMachine = {
      id: 'local', name: 'MacBook', host: 'localhost', port: 8787, isDefault: true, sessions: [],
      history: [{ id: 'plan', name: 'Launch planning', tool: 'claude', cwd: '/project', machine: 'MacBook',
        created_at: Date.now(), last_activity_at: Date.now(), message_count: 12, conversation_available: true }]
    };
    const mini: FakeMachine = { id: 'mini', name: 'Studio Mini', host: '10.0.0.8', port: 8787, sessions: [] };
    installFakeDaemon([local, mini]);
    useFakeMachines([local, mini]);
    const request = globalThis.fetch;
    let finishRemote: (() => void) | undefined;
    globalThis.fetch = async (input, init) => {
      const url = new URL(String(input));
      if (url.hostname === mini.host && url.pathname === '/api/history') {
        await new Promise<void>((resolve) => { finishRemote = resolve; init?.signal?.addEventListener('abort', () => resolve(), { once: true }); });
      }
      return request(input, init);
    };
    const view = render(<ConversationBrowser filters={DEFAULT_BROWSE_FILTERS} filtered={false} onOpen={() => {}} onResume={() => {}} />);
    try {
      expect(await screen.findByText('Launch planning')).toBeVisible();
      expect(screen.getByText(/Reading available computers/)).toBeVisible();
      expect(finishRemote).toBeDefined();
      finishRemote?.();
      await waitFor(() => expect(screen.queryByText(/Reading available computers/)).not.toBeInTheDocument());
      expect(screen.getByText('Launch planning')).toBeVisible();
    } finally {
      view.unmount();
      finishRemote?.();
    }
  });
});
