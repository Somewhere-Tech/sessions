import React, { useState } from 'react';
import { createRoot } from 'react-dom/client';
import '../src/styles/globals.css';
import { AccountsPanel } from '../src/components/AccountsPanel';
import type { AccountProfile } from '../src/api/sessionsd';
import { installFakeDaemon, useFakeMachines, type FakeMachine } from '../tests/capability/fake-daemon';

// Two computers: one ChatGPT team account both report under the same provider
// account ID (a contract fixture: no provider reports such an ID today), an
// email-only account on each, a failed read with a stale
// reading, a signed-out home, and a Claude account whose usage is not connected yet.
const HOUR = 60 * 60_000;
const now = Date.now();
const team = (checked: number) => ({ account_id: 'ws-team', email: 'team@example.test', plan: 'team', checked_at: checked });
const windows = (percent: number) => [
  { kind: 'primary', used_percent: percent, window_minutes: 300, resets_at: now + 2 * HOUR },
  { kind: 'secondary', used_percent: 41, window_minutes: 10_080, resets_at: now + 4 * 24 * HOUR }
];
const machines: FakeMachine[] = [
  {
    id: 'local', name: 'This Mac', host: 'localhost', port: 8787, isDefault: true, sessions: [],
    profiles: [
      { tool: 'codex', name: 'team', label: 'Team plan', signed_in: true, identity: team(now - HOUR), last_used: now - HOUR },
      { tool: 'claude', name: 'max', label: 'Claude Max', signed_in: true, identity: { email: 'me@example.test', plan: 'max', checked_at: now - 3 * HOUR } },
      { tool: 'codex', name: 'personal', label: 'Personal', signed_in: true, identity: { email: 'me@example.test', plan: 'plus', checked_at: now - HOUR } }
    ],
    accountUsage: {
      'codex/team': { state: 'available', checked_at: now - 20 * 60_000, read_at: now - 20 * 60_000, identity: team(now - 20 * 60_000), buckets: [{ limit_id: 'codex', windows: windows(18) }] },
      'codex/personal': { state: 'signed_out', checked_at: now, message: 'Codex reports no sign-in for this account. Sign in to see its usage.' }
    }
  },
  {
    id: 'mini', name: 'Mac mini', host: 'mini.test', port: 8787, sessions: [],
    profiles: [
      { tool: 'codex', name: 'team', signed_in: true, identity: team(now - 2 * HOUR) },
      { tool: 'codex', name: 'side', label: 'Side project', signed_in: true, identity: { email: 'me@example.test', plan: 'plus', checked_at: now - HOUR } }
    ],
    accountUsage: {
      'codex/team': {
        state: 'available', checked_at: now - 60_000, read_at: now - 60_000, identity: team(now - 60_000),
        buckets: [
          { limit_id: 'codex', windows: windows(93) },
          { limit_id: 'codex_review', limit_name: 'Code review', windows: [{ kind: 'primary', used_percent: 5, window_minutes: 10_080 }] }
        ]
      },
      'codex/side': {
        state: 'unavailable', checked_at: now, message: 'Codex did not start. Refresh to try again.', stale: true, read_at: now - 5 * HOUR,
        buckets: [{ limit_id: 'codex', windows: [{ kind: 'primary', used_percent: 64, window_minutes: 300 }] }]
      }
    }
  }
];
const daemon = installFakeDaemon(machines);
useFakeMachines(machines, 'local');
Object.assign(window, { accountDaemon: daemon });
const theme = new URLSearchParams(location.search).get('theme') === 'light' ? 'light' : 'dark';
document.documentElement.dataset.theme = theme;
const initial = machines[0]!.profiles!.map((account) => ({ path: '', sessions: [], last_used: 0, signed_in: false, ...account })) as AccountProfile[];
function Fixture(): JSX.Element {
  const [profiles, setProfiles] = useState<AccountProfile[]>(initial);
  return <div className="operations-shell text-size-s" data-theme={theme} style={{ height: 'auto', minHeight: '100dvh', display: 'block' }}>
    <AccountsPanel profiles={profiles} machineName="This Mac" serverId="local" onReload={setProfiles} />
  </div>;
}
createRoot(document.getElementById('root')!).render(<Fixture />);
