// The real SessionNavigator, over a pin-shaped fixture list, with a fake
// daemon that answers PUT /api/sessions/<id>/pin.
//
// The rest of the pin suite bundles lib/workingSet.ts and asserts on the
// grouping function. That cannot see the thing the owner actually reported:
// the grouping was fine and the ACTION was missing, so a shipped feature had
// no way in from the row a person reaches for. Only a mounted navigator can
// answer "does the ••• menu offer Pin?", and only a real write path can answer
// "does pinning move the row into the Pinned group and out of Live?".
//
// No daemon: window.fetch and window.WebSocket are replaced below, so the
// navigator, the store and src/api/sessionsd.ts all run their normal code.
import React from 'react';
import { createRoot } from 'react-dom/client';
import '../src/styles/globals.css';
import { SessionNavigator } from '../src/components/SessionNavigator';
import { useSessions } from '../src/store/sessions';
import type { SessionInfo } from '../src/types';

const MACHINE = 'Fixture Mac (this machine)';
const now = Date.now();

const base = {
  cmd: 'claude',
  args: [] as string[],
  cwd: '/Users/example/sessions',
  cols: 120,
  rows: 40,
  pid: 4242,
  tool: 'claude-code' as const,
  working: false,
  lastUserMessageAt: now - 90_000,
  exited: false,
  exitCode: null,
  exitSignal: null,
  exitedAt: null,
  provenanceStatus: 'rooted',
  pinned: false
};

// Four records, one per question the suite asks:
//   pinned-manager  — already pinned, with delegated work that must stay out
//                     of the main navigator;
//   pinned-child    — the agent-created helper shown only in Subagents;
//   plain-manager   — starts in Live; the suite pins it, then unpins it;
//   ended-record    — the daemon refuses a pin on this one (409), so the menu
//                     item must be present, disabled, and say why.
export const sessions: SessionInfo[] = [
  {
    ...base,
    id: 'pinned-manager',
    name: 'Pinned workbench',
    createdAt: now - 3_600_000,
    lastDataAt: now - 120_000,
    pinned: true
  },
  {
    ...base,
    id: 'pinned-child',
    name: 'Child of the pin',
    parentSessionId: 'pinned-manager',
    creatorKind: 'session',
    delegationKind: 'agent',
    kind: 'lane',
    createdAt: now - 3_000_000,
    lastDataAt: now - 60_000
  },
  {
    ...base,
    id: 'plain-manager',
    name: 'Plain manager',
    createdAt: now - 1_800_000,
    lastDataAt: now - 30_000
  },
  {
    ...base,
    id: 'ended-record',
    name: 'Ended record',
    createdAt: now - 7_200_000,
    lastDataAt: now - 5_400_000,
    exited: true,
    exitCode: 0,
    exitReason: 'ended-by-user',
    exitedAt: now - 5_400_000
  }
];

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' }
  });
}

// The daemon side of the pin, including its refusal. `UpdatePinned` in
// runtime/internal/session/manager.go checks Exited before it reads the
// requested value, so an ended record is 409 in both directions — the fixture
// answers the same way, and the UI must never send it one.
window.fetch = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
  const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
  const method = (init?.method ?? 'GET').toUpperCase();
  const pin = /\/api\/sessions\/([^/]+)\/pin$/.exec(url);
  if (method === 'PUT' && pin) {
    const id = decodeURIComponent(pin[1]);
    const record = useSessions.getState().sessions.find((session) => session.id === id);
    if (!record) return jsonResponse({ error: `session not found: ${id}` }, 404);
    if (record.exited) {
      return jsonResponse({
        error: 'session has ended; a pin exempts a live session from automatic '
          + 'termination and cannot protect one that already ended, so use '
          + 'archive to hide the record instead'
      }, 409);
    }
    const requested = JSON.parse(String(init?.body ?? '{}')) as { pinned?: boolean };
    return jsonResponse({ pinned: requested.pinned === true });
  }
  if (/\/api\/health/.test(url)) {
    return jsonResponse({
      ok: true,
      name: 'sessionsd',
      version: '0.2.16',
      listen: { host: '127.0.0.1', port: 8787 },
      lan: { enabled: false, url: null },
      discovering: false,
      sessionsLoaded: sessions.length
    });
  }
  if (/\/api\/sessions/.test(url)) return jsonResponse({ sessions: useSessions.getState().sessions });
  if (/\/api\/profiles/.test(url)) return jsonResponse({ profiles: [] });
  return jsonResponse({}, 404);
};

class InertSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;
  readonly readyState = 3;
  close(): void { /* nothing to close */ }
  send(): void { /* nothing to send */ }
  addEventListener(): void { /* never fires */ }
  removeEventListener(): void { /* never registered */ }
}
window.WebSocket = InertSocket as unknown as typeof WebSocket;

useSessions.setState({ serverId: 'fixture', sessions, hydrated: true });

const noop = (): void => { /* the fixture never navigates */ };

// The navigator reads its list from a prop, exactly as App.tsx does, and
// App.tsx feeds it from the store. Subscribing here is what makes the pin a
// round trip rather than a local toggle: click → store action → api → the fake
// daemon → store → re-render → a different group.
function Harness(): JSX.Element {
  const live = useSessions((state) => state.sessions);
  return (
    <div id="surface-navigator" style={{ height: '900px', display: 'flex' }}>
      <SessionNavigator
        sessions={live}
        activeId={null}
        machine={MACHINE}
        onOpen={noop}
        onOpenMachineSession={noop}
        onNew={noop}
        onContinue={noop}
        onResumeSession={noop}
        onForkSession={async () => { /* not exercised here */ }}
        onStartLinked={noop}
        openSessionIds={[]}
        onCloseView={noop}
        onReparent={async () => { /* not exercised here */ }}
      />
    </div>
  );
}

createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <div className="app-shell operations-shell" data-theme="dark" style={{ display: 'block' }}>
      <Harness />
    </div>
  </React.StrictMode>
);
