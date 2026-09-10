import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { LocalNetworkGuide } from '../../src/components/LocalNetworkGuide';
import { useFakeMachines } from './fake-daemon';

function fixture({ mobile = false, status = 'denied' } = {}) {
  useFakeMachines([
    { id: 'local', name: 'MacBook', host: 'localhost', port: 8787, isDefault: true, sessions: [] },
    { id: 'mini', name: 'Mac mini', host: '10.0.0.8', port: 8787, sessions: [] }
  ], 'mini');
  vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(mobile ? 'iPhone' : 'Macintosh');
  const invoke = vi.fn(async () => undefined);
  Object.defineProperty(window, '__TAURI_INTERNALS__', { configurable: true, value: { invoke } });
  // `discovered` is what the daemon actually reached. The daemon records
  // `granted` only from real contact, so a fixture that flips `status` without
  // finding anything is the shape of a stale, stored observation.
  const state = { status, fail: false, discovered: [] as unknown[], requests: [] as string[] };
  globalThis.fetch = async (input) => {
    const url = String(input);
    state.requests.push(url);
    if (state.fail) throw new Error('unreachable');
    const path = new URL(url).pathname;
    if (path === '/api/lan') return new Response(JSON.stringify({ enabled: true, permission: { status: state.status } }));
    if (path === '/api/lan/discover') return new Response(JSON.stringify({ machines: state.discovered }));
    throw new Error(`Unexpected request ${url}`);
  };
  return { state, invoke };
}

afterEach(() => { Reflect.deleteProperty(window, '__TAURI_INTERNALS__'); vi.restoreAllMocks(); });

describe('capability: permission recovery explains the next step on the right Mac', () => {
  it('shows numbered steps and opens only local settings while a remote chat is selected', async () => {
    const { state, invoke } = fixture();
    render(<LocalNetworkGuide />);
    expect(await screen.findByRole('region', { name: 'Nearby access on this Mac' })).toBeVisible();
    expect(screen.getAllByRole('listitem')).toHaveLength(3);
    await userEvent.setup().click(screen.getByRole('button', { name: 'Open System Settings' }));
    await waitFor(() => expect(invoke).toHaveBeenCalledWith('open_local_network_settings', {}, undefined));
    expect(state.requests.every((url) => new URL(url).hostname === 'localhost')).toBe(true);
  });

  it('checks access, does not equate an empty discovery with success, then confirms actual recovery', async () => {
    const { state } = fixture();
    render(<LocalNetworkGuide />);
    const check = await screen.findByRole('button', { name: 'Check again' });
    await userEvent.setup().click(check);
    expect(await screen.findByText(/If access is not confirmed yet/)).toBeVisible();
    expect(screen.queryByText(/Nearby access is working/)).not.toBeInTheDocument();
    state.status = 'granted';
    state.discovered = [{ name: 'Mac mini' }];
    await userEvent.setup().click(check);
    expect(await screen.findByRole('status')).toHaveTextContent('Nearby access is working on this Mac');
    expect(screen.queryByRole('button', { name: 'Open System Settings' })).not.toBeInTheDocument();
  });

  // The daemon's `granted` is its last observation, not a live reading. A check
  // that answers 200 with nothing found has contacted no one, so announcing a
  // restored connection off the back of it would be a claim about a network
  // this check never reached.
  it('does not announce a working connection from an empty discovery plus a stored success', async () => {
    const { state } = fixture();
    render(<LocalNetworkGuide />);
    const check = await screen.findByRole('button', { name: 'Check again' });
    state.status = 'granted';
    await userEvent.setup().click(check);
    expect(await screen.findByText(/If access is not confirmed yet/)).toBeVisible();
    expect(screen.queryByText(/Nearby access is working/)).not.toBeInTheDocument();
    expect(screen.getAllByRole('listitem')).toHaveLength(3);
  });

  // A stored success that arrives on its own is reported as history with a way
  // to test it, never as a live connection.
  it('reports an unverified stored success as the host\'s last observation', async () => {
    const { state } = fixture();
    render(<LocalNetworkGuide />);
    await screen.findByRole('heading');
    state.status = 'granted';
    fireEvent.focus(window);
    const banner = await screen.findByRole('status');
    expect(banner).toHaveTextContent('last reported nearby access working');
    expect(banner).toHaveTextContent('not a live test');
    expect(screen.queryByText(/Nearby access is working/)).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Check again' })).toBeEnabled();
  });

  // The failure is what just happened; the stored success is older. Returning
  // the resolved banner here used to drop the error message entirely.
  it('keeps a failed check visible even when the host still reports an earlier success', async () => {
    const { state } = fixture();
    render(<LocalNetworkGuide />);
    await screen.findByRole('heading');
    state.status = 'granted';
    fireEvent.focus(window);
    await screen.findByText(/last reported nearby access working/);
    state.fail = true;
    await userEvent.setup().click(screen.getByRole('button', { name: 'Check again' }));
    expect(await screen.findByText(/If Sessions is already on, leave it on/)).toBeVisible();
    expect(screen.queryByText(/Nearby access is working/)).not.toBeInTheDocument();
    expect(screen.getAllByRole('listitem')).toHaveLength(3);
  });

  it('never opens phone settings to fix a host Mac or asks the phone to trigger host discovery', async () => {
    const { state } = fixture({ mobile: true });
    render(<LocalNetworkGuide />);
    expect(await screen.findByRole('region', { name: 'Nearby access on Mac mini' })).toBeVisible();
    expect(screen.queryByRole('button', { name: 'Open System Settings' })).not.toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole('button', { name: 'Check again' }));
    expect(state.requests.every((url) => new URL(url).hostname === '10.0.0.8' && new URL(url).pathname === '/api/lan')).toBe(true);
  });

  it('keeps recovery instructions visible when the check fails', async () => {
    const { state } = fixture();
    render(<LocalNetworkGuide />);
    await screen.findByRole('heading');
    state.fail = true;
    await userEvent.setup().click(screen.getByRole('button', { name: 'Check again' }));
    expect(await screen.findByRole('status')).toHaveTextContent('If Sessions is already on, leave it on');
    expect(screen.getByRole('button', { name: 'Check again' })).toBeEnabled();
    expect(screen.getAllByRole('listitem')).toHaveLength(3);
  });

  it.each(['granted', 'not-required', 'unknown'])('does not invent a permission problem for %s', async (status) => {
    const { state } = fixture({ status });
    const { container } = render(<LocalNetworkGuide />);
    await waitFor(() => expect(state.requests).toHaveLength(1));
    expect(container).toBeEmptyDOMElement();
  });
});
