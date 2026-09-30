import React, { useState } from 'react';
import { createRoot } from 'react-dom/client';
import '../src/styles/globals.css';
import { AccountsPanel } from '../src/components/AccountsPanel';
import type { AccountProfile } from '../src/api/sessionsd';
import { installFakeDaemon, useFakeMachines, type FakeMachine } from '../tests/capability/fake-daemon';

const machines: FakeMachine[] = [{ id: 'local', name: 'This Mac', host: 'localhost', port: 8787, isDefault: true, sessions: [] }];
const daemon = installFakeDaemon(machines);
useFakeMachines(machines);
Object.assign(window, { accountDaemon: daemon });
const theme = new URLSearchParams(location.search).get('theme') === 'light' ? 'light' : 'dark';
document.documentElement.dataset.theme = theme;
function Fixture(): JSX.Element {
  const [profiles, setProfiles] = useState<AccountProfile[]>([]);
  // The app's own theme scope, so light mode uses the product's light tokens.
  return <div className="operations-shell text-size-s" data-theme={theme} style={{ height: 'auto', minHeight: '100dvh', display: 'block' }}>
    <AccountsPanel profiles={profiles} machineName="This Mac" onReload={setProfiles} />
  </div>;
}
createRoot(document.getElementById('root')!).render(<Fixture />);
