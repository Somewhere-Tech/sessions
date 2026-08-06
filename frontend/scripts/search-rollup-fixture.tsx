// The real SearchView, mounted over four fixture daemons.
//
// Search's whole claim is reach: on this machine the index holds ~288 sessions
// where the session list holds 26. That reach only exists for the user if the
// screen says which sessions matched — including the ones whose messages never
// fit on the returned page. Asserting that by grepping SearchView.tsx would
// prove nothing about what renders, so this fixture mounts the component and
// replaces window.fetch with a daemon that answers four different ways:
//
//   "rollout"  a current daemon: rollup, totals, one session with hits but no
//              messages on the page.
//   "how should the drafts rollout work"  the strict reading found nothing and
//              the daemon relaxed the query.
//   "partial"  the rollup did not finish; its counts are lower bounds.
//   "legacy"   a daemon older than the rollup: matches and total, nothing else.
//
// The smoke driver types each query into the same view, so every scenario goes
// through the component's real request path.
import React from 'react';
import { createRoot } from 'react-dom/client';
import '../src/styles/globals.css';
import { SearchView } from '../src/components/SearchView';

const MACHINE = 'Fixture Mac';
const CWD = '/Users/example/somewhere/tech';

const message = (
  sessionID: string,
  index: number,
  role: 'user' | 'assistant',
  snippet: string,
  timestamp: string,
  name: string,
  tool: 'claude' | 'codex' = 'claude'
) => ({
  session_id: sessionID,
  provider_session_id: `provider-${sessionID}`,
  name,
  tool,
  role,
  timestamp,
  message_index: index,
  message_id: `${sessionID}:${index}`,
  snippet,
  match_start: 0,
  match_end: 7,
  score: 0.9,
  cwd: CWD,
  machine: MACHINE
});

// A current daemon. Note sess-lost: five hits, zero messages on the page.
// Before the rollup this conversation was simply not on the screen.
const CURRENT = {
  matches: [
    message('sess-a', 12, 'user', 'how the drafts [[rollout]] should work for the beta', '2026-07-09T17:04:00Z', 'Drafts rollout plan'),
    message('sess-a', 40, 'assistant', 'the [[rollout]] gates on the migration finishing', '2026-07-09T17:22:00Z', 'Drafts rollout plan'),
    message('sess-b', 3, 'user', 'staged [[rollout]] for the mobile client', '2026-07-11T09:10:00Z', 'Mobile staged release')
  ],
  total: 3,
  sessions: [
    {
      session_id: 'sess-a',
      name: 'Drafts rollout plan',
      cwd: CWD,
      tool: 'claude',
      machine: MACHINE,
      hits: 9,
      score: 0.94,
      first_hit_at: '2026-07-02T11:00:00Z',
      last_hit_at: '2026-07-09T17:22:00Z'
    },
    {
      session_id: 'sess-b',
      name: 'Mobile staged release',
      cwd: CWD,
      tool: 'claude',
      machine: MACHINE,
      hits: 1,
      score: 0.71,
      first_hit_at: '2026-07-11T09:10:00Z',
      last_hit_at: '2026-07-11T09:10:00Z'
    },
    {
      session_id: 'sess-lost',
      name: 'Rollout retro with Codex',
      cwd: '/Users/example/somewhere/retro',
      tool: 'codex',
      machine: MACHINE,
      hits: 5,
      score: 0.66,
      first_hit_at: '2026-05-04T08:00:00Z',
      last_hit_at: '2026-05-06T19:30:00Z'
    }
  ],
  effective_query: 'rollout',
  match_mode: 'strict',
  total_hits: 288,
  total_sessions: 26,
  rollup_partial: false
};

const RELAXED = {
  matches: [
    message('sess-c', 5, 'user', 'the [[drafts]] work happened in the migration thread', '2026-06-01T10:00:00Z', 'Migration thread')
  ],
  total: 1,
  sessions: [
    {
      session_id: 'sess-c',
      name: 'Migration thread',
      cwd: CWD,
      tool: 'claude',
      machine: MACHINE,
      hits: 4,
      score: 0.4,
      first_hit_at: '2026-06-01T10:00:00Z',
      last_hit_at: '2026-06-01T12:00:00Z'
    }
  ],
  effective_query: 'drafts OR rollout',
  match_mode: 'broad',
  total_hits: 4,
  total_sessions: 1,
  rollup_partial: false
};

const PARTIAL = {
  matches: [
    message('sess-d', 2, 'user', 'the [[partial]] index rebuild', '2026-04-01T10:00:00Z', 'Index rebuild')
  ],
  total: 1,
  sessions: [
    {
      session_id: 'sess-d',
      name: 'Index rebuild',
      cwd: CWD,
      tool: 'claude',
      machine: MACHINE,
      hits: 6,
      score: 0.8,
      first_hit_at: '2026-04-01T10:00:00Z',
      last_hit_at: '2026-04-01T10:40:00Z'
    }
  ],
  effective_query: 'partial',
  match_mode: 'strict',
  total_hits: 31,
  total_sessions: 12,
  rollup_partial: true
};

// Everything the rollup added, absent. This is the shape the frontend must
// keep working against, and the reason every new field is optional.
const LEGACY = {
  matches: [
    message('sess-e', 1, 'user', 'the [[legacy]] daemon answer', '2026-03-01T10:00:00Z', 'Legacy daemon chat'),
    message('sess-e', 8, 'assistant', 'still the [[legacy]] shape', '2026-03-01T10:30:00Z', 'Legacy daemon chat')
  ],
  total: 2
};

const TRANSCRIPT = {
  schemaVersion: 1,
  session: {
    id: 'sess-lost',
    name: 'Rollout retro with Codex',
    tool: 'codex',
    cwd: '/Users/example/somewhere/retro',
    machine: MACHINE,
    created_at: Date.parse('2026-05-04T08:00:00Z'),
    last_activity_at: Date.parse('2026-05-06T19:30:00Z'),
    message_count: 42,
    conversation_available: true
  },
  messages: [
    { index: 0, id: 'm0', role: 'user', text: 'Kicking off the retro.', timestamp: '2026-05-04T08:00:00Z' },
    { index: 1, id: 'm1', role: 'assistant', text: 'Here is what shipped.', timestamp: '2026-05-04T08:01:00Z' },
    { index: 2, id: 'm2', role: 'user', text: 'And what slipped?', timestamp: '2026-05-04T08:02:00Z' }
  ],
  has_more: true,
  next_index: 3
};

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } });
}

// Every request the search path makes is answered here; nothing reaches a
// daemon. The search response is chosen by the query the component sent, so
// each scenario is reached the way a user reaches it — by typing.
const fetchLog: string[] = [];
(window as unknown as { __fetchLog: string[] }).__fetchLog = fetchLog;

window.fetch = async (input: RequestInfo | URL): Promise<Response> => {
  const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, window.location.href);
  fetchLog.push(`${url.pathname}${url.search}`);
  if (url.pathname === '/api/search') {
    const query = (url.searchParams.get('q') ?? '').toLowerCase();
    if (query.includes('legacy')) return jsonResponse(LEGACY);
    if (query.includes('partial')) return jsonResponse(PARTIAL);
    if (query.includes('how should')) return jsonResponse(RELAXED);
    if (query.includes('rollout')) return jsonResponse(CURRENT);
    return jsonResponse({ matches: [], total: 0, sessions: [], total_hits: 0, total_sessions: 0 });
  }
  if (url.pathname.startsWith('/api/history/')) return jsonResponse(TRANSCRIPT);
  if (url.pathname === '/api/history') return jsonResponse({ sessions: [] });
  if (url.pathname === '/api/sessions') return jsonResponse({ sessions: [] });
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
      <div id="surface-search" style={{ height: '1200px' }}>
        <SearchView onResumeConversation={async () => { /* resume is not exercised here */ }} />
      </div>
    </div>
  </React.StrictMode>
);
