import { afterEach, describe, expect, it } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { FleetView } from '../../src/components/FleetView';
import { installFakeDaemon, useFakeMachines, type FakeMachine } from './fake-daemon';

afterEach(() => Reflect.deleteProperty(window, '__TAURI_INTERNALS__'));

describe('capability: discovery does not take over Fleet', () => {
  it('discovers quietly; shows plain guidance and optional diagnostics when requested', async () => {
    const machine: FakeMachine = { id: 'local', name: 'MacBook', host: 'localhost', port: 8787, isDefault: true, sessions: [] };
    installFakeDaemon([machine]);
    useFakeMachines([machine]);
    let discoveries = 0;
    const rawError = '{"error":"sendto: no route to host"}';
    Object.defineProperty(window, '__TAURI_INTERNALS__', { configurable: true, value: {
      invoke: async (command: string) => {
        if (command === 'native_tailnet_discover') { discoveries++; return []; }
        if (command === 'native_nearby_discover') throw new Error(rawError);
        throw new Error(`Unexpected native request: ${command}`);
      }
    } });
    render(<FleetView onOpenSession={() => {}} onOpenMachine={() => {}} />);
    await waitFor(() => expect(discoveries).toBe(1));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Find machines' })).toBeEnabled());
    expect(screen.queryByRole('heading', { name: 'Machines you can connect to' })).not.toBeInTheDocument();
    expect(screen.queryByText(/sendto/)).not.toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole('button', { name: 'Find machines' }));
    expect(await screen.findByText(/Some discovery routes did not answer/)).toBeVisible();
    expect(screen.getByText(/sendto/)).not.toBeVisible();
    await userEvent.setup().click(screen.getByText('Technical details'));
    expect(screen.getByText(/sendto/)).toBeVisible();
  });
});
