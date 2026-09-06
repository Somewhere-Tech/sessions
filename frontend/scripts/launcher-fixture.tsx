import React from 'react';
import { createRoot } from 'react-dom/client';
import '../src/styles/globals.css';
import { NewSessionDialog } from '../src/components/NewSessionDialog';
import { installFakeDaemon, useFakeMachines, type FakeMachine } from '../tests/capability/fake-daemon';

const machines: FakeMachine[] = [
  { id: 'local', name: 'MacBook Pro', host: 'localhost', port: 8787, isDefault: true, sessions: [],
    directories: [{ path: '/Users/example/Projects/Sessions', label: 'Sessions', kind: 'project' }] },
  { id: 'mini', name: 'Mac-mini-313.local', host: 'fixture-mini.invalid', port: 8787, sessions: [],
    directories: [{ path: '/Users/example/Projects/platform', label: 'platform', kind: 'project' }] }
];
for (const machine of machines) {
  machine.codexModels = [{ id: 'gpt-6-astra', displayName: 'Astra', hidden: false, isDefault: true,
    defaultReasoningEffort: 'high', supportedReasoningEfforts: [{ reasoningEffort: 'high', description: 'High' }] }];
}
// Real components and store; no request can reach a real host or provider.
const daemon = installFakeDaemon(machines);
useFakeMachines(machines);
document.documentElement.dataset.theme = 'dark';
Object.assign(window, { launcherDaemon: daemon });
createRoot(document.getElementById('root')!).render(
  <NewSessionDialog embedded onClose={() => {}} onStarted={() => {}} onOpenResume={() => {}} />
);
