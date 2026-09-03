import React from 'react';
import { createRoot } from 'react-dom/client';
import '../src/styles/globals.css';
import { SessionNavigator } from '../src/components/SessionNavigator';
import { SessionView } from '../src/components/SessionView';
import { preferNextSessionView } from '../src/lib/sessionViewPreference';
import { useSessions } from '../src/store/sessions';
import { installFakeDaemon, makeSession, useFakeMachines, type FakeMachine } from '../tests/capability/fake-daemon';
import type { StructuredSessionEvent } from '../src/types';

const now = Date.now();
const codexFault = makeSession({
  id: 'codex-fault',
  name: 'Codex outage',
  tool: 'codex',
  cmd: 'codex',
  kind: 'codex-app-server',
  failureKind: 'provider-unavailable',
  failureDetail: 'Codex API unavailable (503, overloaded)',
  failureProvider: 'codex',
  failureAt: now - 3 * 60_000
});
const retrying = makeSession({
  id: 'codex-retrying',
  name: 'Retrying release check',
  tool: 'codex',
  cmd: 'codex',
  kind: 'codex-app-server',
  failureKind: 'provider-unavailable',
  failureDetail: 'Codex API unavailable (503, overloaded)',
  failureProvider: 'codex',
  failureAt: now - 2 * 60_000,
  retry: { attempt: 2, max: 5, nextAt: now + 42_000, kind: 'provider-unavailable' }
});
const ptyFault = makeSession({
  id: 'codex-pty-fault',
  name: 'Terminal rate limit',
  tool: 'codex',
  cmd: 'codex',
  failureKind: 'rate-limited',
  failureDetail: 'Codex rate limit reached (429)',
  failureProvider: 'codex',
  failureAt: now - 60_000
});
const claudeFault = makeSession({
  id: 'claude-fault',
  name: 'Claude outage',
  kind: 'claude-structured',
  failureKind: 'provider-unavailable',
  failureDetail: 'Claude API overloaded (529)',
  failureProvider: 'claude',
  failureAt: now - 30_000
});

const events: StructuredSessionEvent[] = [
  {
    type: 'user', source: 'codex-app-server', uuid: 'fault-user',
    timestamp: new Date(now - 4_000).toISOString(),
    message: { role: 'user', content: 'Check the release.' }
  },
  {
    type: 'system', subtype: 'provider_fault', uuid: 'fault-event',
    timestamp: new Date(now - 3_000).toISOString(), provider: 'codex',
    kind: 'provider-unavailable', detail: retrying.failureDetail, status: 503
  },
  {
    type: 'system', subtype: 'provider_retry', uuid: 'retry-event',
    timestamp: new Date(now - 2_000).toISOString(), provider: 'codex',
    attempt: 2, max: 5, nextAt: retrying.retry!.nextAt
  }
];

const machine: FakeMachine = {
  id: 'fixture',
  name: 'Fixture Mac',
  host: '127.0.0.1',
  port: 8787,
  isDefault: true,
  sessions: [codexFault, retrying, ptyFault, claudeFault],
  events: { [retrying.id]: events }
};
const daemon = installFakeDaemon([machine]);
useFakeMachines([machine]);
preferNextSessionView(ptyFault.id, 'terminal');

Object.assign(window, {
  __providerFaultFixture: {
    daemon,
    clearFaults: async () => {
      for (const session of machine.sessions) {
        session.failureKind = undefined;
        session.failureDetail = undefined;
        session.failureProvider = undefined;
        session.failureAt = undefined;
        session.retry = undefined;
      }
      await useSessions.getState().refresh();
    }
  }
});
const noop = (): void => {};

function Fixture(): JSX.Element {
  const sessions = useSessions((state) => state.sessions);
  return (
    <div className="provider-fault-fixture" data-theme="dark">
      <section id="fault-navigator">
        <SessionNavigator
          sessions={sessions}
          activeId={null}
          machine={machine.name}
          onOpen={noop}
          onOpenMachineSession={noop}
          onNew={noop}
          onContinue={noop}
          onResumeSession={noop}
          onForkSession={async () => {}}
          onStartLinked={noop}
          openSessionIds={[]}
          onCloseView={noop}
          onReparent={async () => {}}
        />
      </section>
      <section id="retrying-view"><SessionView sessionId={retrying.id} isActive /></section>
      <section id="pty-view"><SessionView sessionId={ptyFault.id} isActive={false} preferFullTerminal /></section>
    </div>
  );
}

createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <Fixture />
  </React.StrictMode>
);
