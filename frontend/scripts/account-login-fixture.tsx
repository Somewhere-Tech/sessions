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
document.documentElement.dataset.theme = 'dark';
function Fixture(): JSX.Element {
  const [profiles, setProfiles] = useState<AccountProfile[]>([]);
  return <main style={{ maxWidth: 760, margin: '0 auto', padding: 16 }}>
    <AccountsPanel profiles={profiles} machineName="This Mac" onReload={setProfiles} />
  </main>;
}
createRoot(document.getElementById('root')!).render(<Fixture />);
