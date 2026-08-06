// One fixture session list, every surface that renders status, on one screen.
//
// The smoke suites around this one bundle lib/ modules and assert on them, or
// drive puppeteer over static markup. Neither can catch the class of bug this
// program actually found: two real components, mounted at the same time,
// describing the same session differently. That needs the components
// themselves, rendered from real props against real stores — which is what
// this fixture is. There is no daemon: window.fetch and window.WebSocket are
// replaced below so every surface takes its normal code path and gets
// deterministic answers.
import React from 'react';
import { createRoot } from 'react-dom/client';
import '../src/styles/globals.css';
import { SessionNavigator } from '../src/components/SessionNavigator';
import { FleetView } from '../src/components/FleetView';
import { HomeView } from '../src/components/HomeView';
import { GridView } from '../src/components/GridView';
import { useSessions } from '../src/store/sessions';
import type { SessionInfo } from '../src/types';
import type { TabStatus } from '../src/components/SessionTabs';

const MACHINE = 'Fixture Mac (this machine)';
const now = Date.now();

// The daemon refuses the end request. docs/PRINCIPLES.md: a failed cleanup is
// an unresolved decision and must stay on screen, in the user's words.
const END_FAILURE = 'The runner for this session is not responding.';

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
  provenanceStatus: 'rooted'
};

// Five sessions, one per interesting classifier state. Two of them are the
// exact records that used to make two surfaces disagree.
export const sessions: SessionInfo[] = [
  {
    // THE bug: exited, but its last recorded idleReason is still the provider
    // question it was waiting on. The navigator said "Ended"; Fleet said
    // "Needs you"; both were on screen.
    ...base,
    id: 'ended-needs-input',
    name: 'Ended while asking',
    createdAt: now - 3_600_000,
    lastDataAt: now - 600_000,
    idleReason: 'needs-input',
    idleDetail: 'Claude asked whether to continue.',
    exited: true,
    exitCode: 0,
    exitReason: 'ended-by-user',
    exitedAt: now - 600_000,
    endedByClient: 'sessions-desktop'
  },
  {
    ...base,
    id: 'live-needs-input',
    name: 'Waiting on you',
    createdAt: now - 1_800_000,
    lastDataAt: now - 30_000,
    idleReason: 'needs-input',
    idleDetail: 'Claude is waiting for approval to run a command.'
  },
  {
    // The other bug: an MCP server did not start. The agent is alive and
    // answering. This is not a question, so it must never inflate a
    // "needs you" count on any surface.
    ...base,
    id: 'degraded-mcp',
    name: 'Missing one MCP server',
    tool: 'codex' as const,
    createdAt: now - 2_400_000,
    lastDataAt: now - 120_000,
    idleReason: 'failed',
    idleDetail: '⚠ MCP startup incomplete (failed: somewhere)'
  },
  {
    ...base,
    id: 'busy-runtime',
    name: 'Running the build',
    createdAt: now - 900_000,
    lastDataAt: now - 5_000,
    working: true
  },
  {
    ...base,
    id: 'finished-runtime',
    name: 'Finished its turn',
    createdAt: now - 1_200_000,
    lastDataAt: now - 240_000,
    idleReason: 'completed'
  }
];

// Exactly App.tsx's mapping, so the grid's live-activity override agrees with
// the daemon flag every other surface reads.
const statusBySession: Record<string, TabStatus> = {};
const iconBySession: Record<string, string> = {};
for (const session of sessions) {
  statusBySession[session.id] = session.working ? 'working' : 'idle';
  iconBySession[session.id] = session.tool === 'codex' ? '🟢' : '🟠';
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' }
  });
}

// No daemon, no live sockets — but every surface still runs its real request
// code. Fleet and the navigator's all-machines scope read /api/sessions; the
// end request is answered the way a wedged runner answers it.
window.fetch = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
  const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
  const method = (init?.method ?? 'GET').toUpperCase();
  if (method === 'DELETE' && /\/api\/sessions\//.test(url)) {
    return new Response(END_FAILURE, { status: 503 });
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
  if (/\/api\/sessions/.test(url)) return jsonResponse({ sessions });
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

// The navigator ends sessions through the store, which needs to know which
// machine's rows it holds before it will dial anything.
useSessions.setState({ serverId: 'fixture', sessions, hydrated: true });

const noop = (): void => { /* the fixture never navigates */ };

createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <div className="app-shell operations-shell" data-theme="dark" style={{ display: 'block' }}>
      <div id="surface-navigator" style={{ height: '760px', display: 'flex' }}>
        <SessionNavigator
          sessions={sessions}
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
      <div id="surface-home">
        <HomeView sessions={sessions} machine={MACHINE} onOpen={noop} onNew={noop} onNavigate={noop} />
      </div>
      <div id="surface-fleet">
        <FleetView onOpenSession={noop} onOpenMachine={noop} />
      </div>
      <div id="surface-grid" style={{ height: '620px' }}>
        <GridView
          sessions={sessions}
          statusBySession={statusBySession}
          iconBySession={iconBySession}
          onEnd={noop}
          onExpand={noop}
        />
      </div>
    </div>
  </React.StrictMode>
);
