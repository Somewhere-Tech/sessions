// The real SearchView, mounted over two fixture daemons — one that answers and
// one that does not.
//
// What is being proved is the browse state: the screen a person reaches when
// they cannot quote a word from the conversation they lost, only place it in a
// folder and a day. The rows below cover every verdict lib/conversationBrowser
// can reach, because each one is a different way this screen can lie:
//
//   live-by-id / live-by-provider  a conversation a session is holding right
//       now. The daemon refuses to resume it (ConversationLiveError), so a
//       Resume button here is a button that fails. The second one is bound
//       only through the provider UUID, which is what the daemon's guard keys
//       on and what a naive id comparison misses.
//   sessions-copy  the provider deleted its transcript; Sessions kept one.
//       Still resumable, and only through the history id.
//   native         the ordinary case, resumable through the provider handle.
//   moved          continued on another machine. Resuming here forks it.
//   gone           neither side still holds it. Must say so, not offer.
//   unreadable     could not be read on this pass. Still listed, still named.
//   stale-record   a Sessions record touched by a shutdown sweep long after
//       the conversation itself went quiet. Ordering by the record's activity
//       stamp puts it at the top of the screen, above conversations the user
//       was actually in yesterday.
//   empty / shell  the rows a person scrolls past.
//
// The second machine fails, so every count on the screen is a lower bound.
import React from 'react';
import { createRoot } from 'react-dom/client';
import '../src/styles/globals.css';
import { SearchView } from '../src/components/SearchView';

const HOUR = 3_600_000;
const DAY = 24 * HOUR;
const NOW = Date.now();
const CWD = '/Users/example/somewhere/tech';

interface FixtureSession {
  id: string;
  name: string;
  tool: 'claude' | 'codex' | 'shell';
  provider_session_id?: string;
  cwd: string;
  machine: string;
  created_at: number;
  last_activity_at: number;
  conversation_updated_at?: number;
  message_count: number;
  conversation_available: boolean;
  external?: boolean;
  prompt_history_only?: boolean;
  moved_to_endpoint?: string;
  unreadable?: boolean;
  unreadable_reason?: string;
}

const MACHINE = 'Fixture Mac';

const filler: FixtureSession[] = Array.from({ length: 20 }, (_, index) => ({
  id: `filler-${String(index + 1).padStart(2, '0')}`,
  name: `Filler conversation ${String(index + 1).padStart(2, '0')}`,
  tool: 'claude',
  provider_session_id: `uuid-filler-${index + 1}`,
  cwd: `${CWD}/filler`,
  machine: MACHINE,
  created_at: NOW - (30 + index) * DAY,
  last_activity_at: NOW - (30 + index) * DAY,
  conversation_updated_at: NOW - (30 + index) * DAY,
  message_count: 12,
  conversation_available: true
}));

const SESSIONS: FixtureSession[] = [
  {
    id: 'live-by-id',
    name: 'Cutover rehearsal',
    tool: 'claude',
    provider_session_id: 'uuid-live-1',
    cwd: `${CWD}/cutover`,
    machine: MACHINE,
    created_at: NOW - 2 * HOUR,
    last_activity_at: NOW - 2 * HOUR,
    conversation_updated_at: NOW - 2 * HOUR,
    message_count: 84,
    conversation_available: true
  },
  {
    id: 'live-by-provider',
    name: 'Quota calculator with Codex',
    tool: 'codex',
    provider_session_id: 'uuid-live-2',
    cwd: `${CWD}/quota`,
    machine: MACHINE,
    created_at: NOW - 3 * HOUR,
    last_activity_at: NOW - 3 * HOUR,
    conversation_updated_at: NOW - 3 * HOUR,
    message_count: 51,
    conversation_available: true
  },
  {
    id: 'sessions-copy',
    name: 'Hardening sweep notes',
    tool: 'claude',
    cwd: `${CWD}/hardening`,
    machine: MACHINE,
    created_at: NOW - 5 * HOUR,
    last_activity_at: NOW - 5 * HOUR,
    conversation_updated_at: NOW - 5 * HOUR,
    message_count: 233,
    conversation_available: true
  },
  {
    id: 'native',
    name: 'Release notes for 0.2.16',
    tool: 'codex',
    provider_session_id: 'uuid-native',
    cwd: `${CWD}/release`,
    machine: MACHINE,
    created_at: NOW - 26 * HOUR,
    last_activity_at: NOW - 26 * HOUR,
    conversation_updated_at: NOW - 26 * HOUR,
    message_count: 19,
    conversation_available: true,
    external: true
  },
  {
    id: 'moved',
    name: 'Fleet migration plan',
    tool: 'claude',
    provider_session_id: 'uuid-moved',
    cwd: `${CWD}/fleet`,
    machine: MACHINE,
    created_at: NOW - 2 * DAY,
    last_activity_at: NOW - 2 * DAY,
    conversation_updated_at: NOW - 2 * DAY,
    message_count: 40,
    conversation_available: true,
    moved_to_endpoint: 'studio.tail-scale.ts'
  },
  {
    // Touched by a shutdown sweep an hour ago; last spoken in three weeks ago.
    id: 'stale-record',
    name: 'Old drafts thread',
    tool: 'claude',
    provider_session_id: 'uuid-stale',
    cwd: `${CWD}/drafts`,
    machine: MACHINE,
    created_at: NOW - 21 * DAY,
    last_activity_at: NOW - HOUR,
    conversation_updated_at: NOW - 21 * DAY,
    message_count: 7,
    conversation_available: true
  },
  {
    id: 'gone',
    name: 'Deleted experiment',
    tool: 'claude',
    provider_session_id: 'uuid-gone',
    cwd: `${CWD}/experiment`,
    machine: MACHINE,
    created_at: NOW - 4 * DAY,
    last_activity_at: NOW - 4 * DAY,
    conversation_updated_at: NOW - 4 * DAY,
    message_count: 0,
    conversation_available: false
  },
  {
    id: 'unreadable',
    name: 'Torn transcript',
    tool: 'codex',
    provider_session_id: 'uuid-torn',
    cwd: `${CWD}/torn`,
    machine: MACHINE,
    created_at: NOW - 6 * DAY,
    last_activity_at: NOW - 6 * DAY,
    conversation_updated_at: NOW - 6 * DAY,
    message_count: 3,
    conversation_available: true,
    unreadable: true,
    unreadable_reason: 'permission denied reading the Codex rollout file'
  },
  {
    id: 'empty-shell',
    name: 'Never used',
    tool: 'claude',
    provider_session_id: 'uuid-empty',
    cwd: CWD,
    machine: MACHINE,
    created_at: NOW - 7 * DAY,
    last_activity_at: NOW - 7 * DAY,
    conversation_updated_at: NOW - 7 * DAY,
    message_count: 0,
    conversation_available: true
  },
  {
    id: 'shell-lane',
    name: 'build logs',
    tool: 'shell',
    cwd: CWD,
    machine: MACHINE,
    created_at: NOW - 8 * DAY,
    last_activity_at: NOW - 8 * DAY,
    conversation_updated_at: NOW - 8 * DAY,
    message_count: 64,
    conversation_available: true
  },
  ...filler
];

// What the answering daemon currently has running. `live-by-provider` is bound
// only through conversationId: its runtime id is nothing like the history id.
const RUNNING = [
  {
    id: 'live-by-id',
    cmd: 'claude',
    args: ['--resume', 'uuid-live-1'],
    cwd: `${CWD}/cutover`,
    cols: 80,
    rows: 24,
    createdAt: NOW - 2 * HOUR,
    pid: 4242,
    tool: 'claude-code',
    working: true,
    lastDataAt: NOW - 60_000,
    lastUserMessageAt: NOW - 120_000,
    exited: false,
    exitCode: null,
    exitSignal: null,
    exitedAt: null
  },
  {
    id: 'lane-99',
    cmd: 'codex',
    args: [],
    conversationId: 'uuid-live-2',
    cwd: `${CWD}/quota`,
    cols: 80,
    rows: 24,
    createdAt: NOW - 3 * HOUR,
    pid: 4243,
    tool: 'codex',
    working: false,
    lastDataAt: NOW - 300_000,
    lastUserMessageAt: NOW - 300_000,
    exited: false,
    exitCode: null,
    exitSignal: null,
    exitedAt: null
  },
  {
    // Ended, and holding the same provider UUID as a browsable row. An exited
    // runtime does not make a conversation live; treating it as live would
    // hide the Resume button on a perfectly resumable conversation.
    id: 'ended-lane',
    cmd: 'claude',
    args: ['--resume', 'uuid-native'],
    cwd: `${CWD}/release`,
    cols: 80,
    rows: 24,
    createdAt: NOW - 30 * HOUR,
    pid: 0,
    tool: 'claude-code',
    working: false,
    lastDataAt: NOW - 26 * HOUR,
    lastUserMessageAt: NOW - 26 * HOUR,
    exited: true,
    exitCode: 0,
    exitSignal: null,
    exitedAt: NOW - 26 * HOUR
  }
];

const PREVIEW = {
  schemaVersion: 1,
  session: SESSIONS[2],
  messages: [
    { index: 0, id: 'p0', role: 'user', text: 'Start the hardening sweep.', timestamp: new Date(NOW - 9 * HOUR).toISOString() },
    { index: 1, id: 'p1', role: 'tool', text: 'Read(frontend/src/App.tsx)', timestamp: new Date(NOW - 9 * HOUR).toISOString() },
    { index: 2, id: 'p2', role: 'assistant', text: 'Found three surfaces that disagree.', timestamp: new Date(NOW - 8 * HOUR).toISOString() },
    { index: 3, id: 'p3', role: 'user', text: 'Fix the navigator first.', timestamp: new Date(NOW - 7 * HOUR).toISOString() },
    { index: 4, id: 'p4', role: 'assistant', text: 'Navigator and Fleet now read the same classifier.', timestamp: new Date(NOW - 6 * HOUR).toISOString() },
    { index: 5, id: 'p5', role: 'user', text: 'Ship it after the smoke suite passes.', timestamp: new Date(NOW - 5 * HOUR).toISOString() }
  ]
};

const TRANSCRIPT = {
  schemaVersion: 1,
  session: SESSIONS[2],
  messages: PREVIEW.messages,
  has_more: false,
  next_index: 6
};

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } });
}

const fetchLog: string[] = [];
const actions: string[] = [];
(window as unknown as { __fetchLog: string[]; __actions: string[] }).__fetchLog = fetchLog;
(window as unknown as { __fetchLog: string[]; __actions: string[] }).__actions = actions;

window.fetch = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
  const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, window.location.href);
  fetchLog.push(`${init?.method ?? 'GET'} ${url.port}${url.pathname}${url.search}`);
  // The second machine is unreachable. Nothing it holds may be counted, and
  // its absence may not be silent.
  if (url.port === '8788') return jsonResponse({ error: 'unreachable' }, 503);
  if (url.pathname === '/api/history') return jsonResponse({ schemaVersion: 1, sessions: SESSIONS, unreadable_sessions: 1 });
  if (url.pathname.endsWith('/preview')) return jsonResponse(PREVIEW);
  if (url.pathname.startsWith('/api/history/')) return jsonResponse(TRANSCRIPT);
  if (url.pathname === '/api/sessions') return jsonResponse({ sessions: RUNNING });
  if (url.pathname === '/api/search') return jsonResponse({ matches: [], total: 0, sessions: [], total_hits: 0, total_sessions: 0 });
  if (url.pathname === '/api/resumable-conversations') return jsonResponse({ sessions: [] });
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

createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <div className="app-shell operations-shell" data-theme="dark" style={{ display: 'block' }}>
      <div id="surface-search" style={{ height: '1400px' }}>
        <SearchView
          onResumeConversation={async (serverId, providerSessionId, sourceSessionId, historyId) => {
            actions.push(`resume ${serverId} provider=${providerSessionId} source=${sourceSessionId ?? '-'} history=${historyId ?? '-'}`);
          }}
          onOpenLiveSession={(serverId, sessionId) => { actions.push(`attach ${serverId} ${sessionId}`); }}
        />
      </div>
    </div>
  </React.StrictMode>
);
