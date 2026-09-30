import { render, screen } from '@testing-library/react';
import { expect, it } from 'vitest';
import { ConnectionsView } from '../../src/components/ConnectionsView';
import { useServers } from '../../src/lib/servers';
import { installFakeDaemon, useFakeMachines } from './fake-daemon';

function show(protection: 'protected' | 'local'): void {
  const machine = { id: 'local', name: 'Fixture host', host: 'localhost', port: 8787, isDefault: true, sessions: [] };
  installFakeDaemon([machine]);
  useFakeMachines([machine]);
  useServers.setState({ credentialProtection: protection });
  render(<ConnectionsView clientOnly hostName="Fixture host" />);
}

it('does not describe legacy browser storage as an OS-protected vault', async () => {
  show('local');
  expect(await screen.findByRole('note')).toHaveTextContent('OS credential protection is not available on this device yet');
  expect(screen.getByRole('note')).not.toHaveTextContent('protected by this device');
});

it('reports protected storage only after the protected bootstrap has verified it', async () => {
  show('protected');
  expect(await screen.findByRole('note')).toHaveTextContent('protected by this device’s credential store');
});
