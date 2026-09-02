// An in-process sessionsd.
//
// This is the boundary the capability tests mock, and it is the ONLY thing they
// mock. Components are mounted for real, their own modules are never stubbed,
// and every request they make lands here — the same place a browser's request
// would land on a real daemon. Nothing in this file opens a socket; there is no
// listener, no port, and no path by which a test can reach localhost:8787 or
// any other machine.
//
// The routes below follow runtime/CONTRACT/http-api.md. Where the contract
// describes a refusal (archiving a session that is still running is "skipped"
// with reason "session is still running"), this fake refuses it the same way,
// because a capability test that passes against a more permissive fake than the
// real daemon is worth nothing.
//
// An unrecognised route answers 404 and is recorded in `unhandled`. A test that
// fails with a route in that list is telling you the product asked for
// something this fake does not model — fix the fake. A test that fails with an
// empty `unhandled` is telling you about the product.
import { useServers, type ServerConfig } from '../../src/lib/servers';
import { useSessions } from '../../src/store/sessions';
import type { DirectoryCandidate, SessionInfo } from '../../src/types';
import type {
  HistoryMessage,
  HistorySession,
  ProviderStatus,
  ResumableSession,
  SearchMatch,
  SearchResponse,
  TeamListing
} from '../../src/api/sessionsd';

export interface FakeMachine {
  /** Server id used by lib/servers.ts and the sessions store. */
  id: string;
  name: string;
  host: string;
  port: number;
  scheme?: 'http' | 'https';
  isDefault?: boolean;
  machineId?: string;
  /** When false every request to this machine fails the way an unreachable one does. */
  reachable?: boolean;
  sessions: SessionInfo[];
  history?: HistorySession[];
  transcripts?: Record<string, HistoryMessage[]>;
  resumable?: ResumableSession[];
  /** Message corpus the fake's /api/search scans. */
  searchCorpus?: Array<{
    sessionId: string;
    name: string;
    tool: 'claude' | 'codex' | 'shell';
    role: 'user' | 'assistant' | 'tool';
    text: string;
    cwd?: string;
  }>;
  profiles?: unknown[];
  directories?: DirectoryCandidate[];
  providers?: ProviderStatus[];
  team?: TeamListing;
  /** Optional latency used to expose same-tick duplicate-action races. */
  createDelayMS?: number;
  submitDelayMS?: number;
}

export interface RecordedRequest {
  method: string;
  url: string;
  path: string;
  origin: string;
  body: unknown;
}

export interface FakeDaemon {
  machines: FakeMachine[];
  requests: RecordedRequest[];
  unhandled: RecordedRequest[];
  /** Every message the fake accepted through /submit or /input, per session. */
  delivered: Record<string, string[]>;
  /** Sessions created through POST /api/sessions, newest last. */
  created: SessionInfo[];
  /** Sessions ended through DELETE /api/sessions/:id. */
  ended: string[];
  /** ids passed to POST /api/retention/archive, and what the fake answered. */
  archived: string[];
  /** Conversations adopted through POST /api/recovery/adopt. */
  adopted: string[];
  machineFor(origin: string): FakeMachine | undefined;
  session(id: string): SessionInfo | undefined;
  /** Route requests whose path/method this fake does not model, for the report. */
  restore(): void;
}

// Wall clock, not a fixed instant: the navigator's "recently ended" window
// and every relative timestamp are computed against Date.now(), so a frozen
// 2026 fixture would sort itself out of the list the tests are reading.
const NOW = Date.now();

export function makeSession(overrides: Partial<SessionInfo> & { id: string }): SessionInfo {
  return {
    name: overrides.id,
    cmd: 'claude',
    args: [],
    cwd: '/Users/example/project',
    cols: 120,
    rows: 40,
    createdAt: NOW - 3_600_000,
    pid: 4242,
    tool: 'claude-code',
    working: false,
    lastDataAt: NOW - 60_000,
    lastUserMessageAt: NOW - 90_000,
    exited: false,
    exitCode: null,
    exitSignal: null,
    exitedAt: null,
    provenanceStatus: 'rooted',
    pinned: false,
    ...overrides
  };
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' }
  });
}

function originOf(machine: FakeMachine): string {
  return `${machine.scheme ?? 'http'}://${machine.host}:${machine.port}`;
}

function healthFor(machine: FakeMachine): unknown {
  return {
    ok: true,
    name: 'sessionsd',
    version: '0.2.16',
    listen: { host: machine.host, port: machine.port },
    lan: { enabled: false, url: null },
    access: { open: true },
    system: { os: 'darwin', arch: 'arm64' },
    compatibility: {
      api: { current: 1, minimumClient: 1, maximumClient: 1 },
      runner: { current: 2, minimum: 1, maximum: 2 }
    },
    discovering: false,
    sessionsLoaded: machine.sessions.length
  };
}

function searchFor(machine: FakeMachine, query: string, limit: number): SearchResponse {
  const needle = query.trim().toLowerCase();
  const corpus = machine.searchCorpus ?? [];
  const matches: SearchMatch[] = [];
  for (const [index, entry] of corpus.entries()) {
    const at = entry.text.toLowerCase().indexOf(needle);
    if (needle === '' || at < 0) continue;
    matches.push({
      session_id: entry.sessionId,
      name: entry.name,
      tool: entry.tool,
      role: entry.role,
      timestamp: new Date(NOW - (corpus.length - index) * 60_000).toISOString(),
      message_index: index,
      message_id: `${entry.sessionId}:${index}`,
      snippet: entry.text,
      match_start: at,
      match_end: at + needle.length,
      score: 1,
      cwd: entry.cwd ?? '/Users/example/project',
      machine: machine.name
    });
  }
  const page = matches.slice(0, limit);
  const bySession = new Map<string, { name: string; hits: number; snippets: string[]; tool: SearchMatch['tool'] }>();
  for (const match of matches) {
    const row = bySession.get(match.session_id)
      ?? { name: match.name, hits: 0, snippets: [], tool: match.tool };
    row.hits += 1;
    if (page.includes(match)) row.snippets.push(match.snippet);
    bySession.set(match.session_id, row);
  }
  return {
    matches: page,
    total: matches.length,
    machines: [{ alias: machine.id, name: machine.name, endpoint: originOf(machine), status: 'ok' }],
    partial: false,
    sessions: [...bySession.entries()].map(([session_id, row]) => ({
      session_id,
      name: row.name,
      tool: row.tool,
      machine: machine.name,
      hits: row.hits,
      score: row.hits,
      snippets: row.snippets
    })),
    effective_query: query,
    match_mode: 'strict',
    total_hits: matches.length,
    total_sessions: bySession.size,
    rollup_partial: false
  };
}

/**
 * Replace window.fetch and window.WebSocket with the in-process daemon and seed
 * lib/servers + the sessions store so the mounted components address it.
 */
export function installFakeDaemon(machines: FakeMachine[]): FakeDaemon {
  const byOrigin = new Map<string, FakeMachine>();
  for (const machine of machines) byOrigin.set(originOf(machine), machine);

  const daemon: FakeDaemon = {
    machines,
    requests: [],
    unhandled: [],
    delivered: {},
    created: [],
    ended: [],
    archived: [],
    adopted: [],
    machineFor: (origin) => byOrigin.get(origin),
    session: (id) => machines.flatMap((m) => m.sessions).find((s) => s.id === id),
    restore: () => { /* setup.ts re-bans the network after every test */ }
  };

  const handle = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const href = typeof input === 'string'
      ? input
      : input instanceof URL
        ? input.href
        : input.url;
    const url = new URL(href, 'http://127.0.0.1:8787');
    const method = (init?.method ?? 'GET').toUpperCase();
    let body: unknown = undefined;
    if (typeof init?.body === 'string') {
      try { body = JSON.parse(init.body); } catch { body = init.body; }
    } else if (init?.body !== undefined) {
      body = init.body;
    }
    const record: RecordedRequest = { method, url: url.href, path: url.pathname, origin: url.origin, body };
    daemon.requests.push(record);

    const machine = byOrigin.get(url.origin);
    if (!machine) {
      // The product addressed a machine this fixture does not have. That is
      // never a network call: it fails the way an unreachable host fails.
      throw new TypeError(`Failed to fetch: no fake daemon at ${url.origin}`);
    }
    if (machine.reachable === false) {
      throw new TypeError(`Failed to fetch: ${machine.name} is unreachable`);
    }

    const path = url.pathname;
    const sessionRoute = /^\/api\/sessions\/([^/]+)(\/.*)?$/.exec(path);
    const sessionId = sessionRoute ? decodeURIComponent(sessionRoute[1]) : '';
    const sessionTail = sessionRoute?.[2] ?? '';
    const target = machine.sessions.find((s) => s.id === sessionId);

    // ── identity / health ───────────────────────────────────────────────
    if (path === '/api/health') return jsonResponse(healthFor(machine));
    if (path === '/api/machine') {
      return jsonResponse({ machineId: machine.machineId ?? machine.id, name: machine.name });
    }
    if (path === '/api/lan') return jsonResponse({ enabled: false, url: null });
    if (path === '/api/lanes/mine' && machine.team) return jsonResponse(machine.team);

    // ── session list & lifecycle ────────────────────────────────────────
    if (path === '/api/sessions' && method === 'GET') {
      const includeExited = url.searchParams.get('include_exited') === '1';
      return jsonResponse({
        sessions: includeExited ? machine.sessions : machine.sessions.filter((s) => !s.exited)
      });
    }
    if (path === '/api/sessions' && method === 'POST') {
      if (machine.createDelayMS) {
        await new Promise((resolve) => window.setTimeout(resolve, machine.createDelayMS));
      }
      const request = (body ?? {}) as Record<string, unknown>;
      const created = makeSession({
        id: `created-${machine.sessions.length + 1}`,
        name: typeof request.name === 'string' && request.name ? request.name : 'Untitled session',
        cmd: typeof request.cmd === 'string' ? request.cmd : 'claude',
        cwd: typeof request.cwd === 'string' ? request.cwd : '/Users/example/project',
        createdAt: NOW,
        lastDataAt: NOW,
        tool: request.cmd === 'codex' ? 'codex' : request.cmd === 'bash' || request.cmd === 'zsh' ? 'terminal' : 'claude-code'
      });
      machine.sessions.push(created);
      daemon.created.push(created);
      return jsonResponse(created);
    }
    if (sessionRoute && sessionTail === '' && method === 'DELETE') {
      if (!target) return jsonResponse({ ok: false }, 404);
      // The documented success path: the daemon accepts the end request and
      // leaves removal to the runner EXIT path, which the fake completes here.
      target.exited = true;
      target.exitCode = 0;
      target.exitedAt = NOW;
      target.exitReason = 'ended-by-user';
      target.working = false;
      daemon.ended.push(sessionId);
      return jsonResponse({ ok: true });
    }
    if (sessionRoute && sessionTail === '/name' && method === 'PUT') {
      if (!target) return jsonResponse({ error: 'session not found' }, 404);
      const name = String((body as { name?: unknown })?.name ?? '').trim();
      target.name = name;
      return jsonResponse({ name });
    }
    if (sessionRoute && sessionTail === '/pin' && method === 'PUT') {
      if (!target) return jsonResponse({ error: 'session not found' }, 404);
      const pinned = (body as { pinned?: unknown })?.pinned === true;
      target.pinned = pinned;
      return jsonResponse({ pinned });
    }
    if (sessionRoute && sessionTail === '/set-aside' && method === 'PUT') {
      if (!target) return jsonResponse({ error: 'session not found' }, 404);
      const setAside = (body as { setAside?: unknown })?.setAside === true;
      target.setAsideAt = setAside ? NOW : null;
      return jsonResponse({ setAsideAt: target.setAsideAt });
    }
    if (sessionRoute && sessionTail === '/display-parent' && method === 'PUT') {
      const parent = String((body as { parentSessionId?: unknown })?.parentSessionId ?? '');
      if (target) target.displayParentSessionId = parent;
      return jsonResponse({ displayParentSessionId: parent });
    }
    if (sessionRoute && sessionTail === '/tags' && method === 'PUT') {
      const tags = ((body as { tags?: Record<string, string> })?.tags) ?? {};
      if (target) target.tags = tags;
      return jsonResponse({ tags });
    }
    if (sessionRoute && (sessionTail === '/submit' || sessionTail === '/input') && method === 'POST') {
      if (!target) return jsonResponse({ ok: false }, 404);
      if (machine.submitDelayMS) {
        await new Promise((resolve) => window.setTimeout(resolve, machine.submitDelayMS));
      }
      const data = String((body as { data?: unknown })?.data ?? '');
      // The daemon owns text + Enter as one operation; InputBar wraps a
      // message in bracketed paste. Record what a person actually typed.
      const text = data.replace(/\x1b\[200~/g, '').replace(/\x1b\[201~/g, '');
      (daemon.delivered[sessionId] ??= []).push(text);
      target.lastUserMessageAt = NOW;
      return jsonResponse({ ok: true });
    }
    if (sessionRoute && sessionTail === '/snapshot') {
      return new Response('', { status: 200, headers: { 'X-Sessions-Seq': '0' } });
    }
    if (sessionRoute && sessionTail === '/events') {
      return jsonResponse({ events: [], nextIndex: 0, totalCount: 0, startIndex: 0, endIndex: 0 });
    }
    if (sessionRoute && sessionTail === '/model-options') return jsonResponse({ models: [] });

    // ── retention ───────────────────────────────────────────────────────
    if (path === '/api/retention/archive' && method === 'POST') {
      const ids = Array.isArray((body as { ids?: unknown })?.ids)
        ? ((body as { ids: unknown[] }).ids).map(String)
        : [];
      // ArchiveClosed hides a row without deleting its transcript or lineage.
      // A finished parent is therefore independently archivable even while a
      // finished child record remains visible.
      const items = ids.map((id) => {
        const record = machine.sessions.find((s) => s.id === id);
        if (!record) return { id, status: 'skipped' as const, reason: 'record not found' };
        if (!record.exited) return { id, name: record.name, status: 'skipped' as const, reason: 'session is still running' };
        daemon.archived.push(id);
        machine.sessions = machine.sessions.filter((s) => s.id !== id);
        return { id, name: record.name, status: 'archived' as const };
      });
      return jsonResponse({ dry_run: false, cutoff_ms: NOW, items });
    }

    // ── history / conversations ─────────────────────────────────────────
    if (path === '/api/history') {
      return jsonResponse({ schemaVersion: 1, sessions: machine.history ?? [] });
    }
    const historyRoute = /^\/api\/history\/([^/]+)(\/.*)?$/.exec(path);
    if (historyRoute) {
      const id = decodeURIComponent(historyRoute[1]);
      const record = (machine.history ?? []).find((s) => s.id === id);
      if (!record) return jsonResponse({ error: 'history session not found' }, 404);
      return jsonResponse({
        schemaVersion: 1,
        session: record,
        messages: machine.transcripts?.[id] ?? []
      });
    }
    if (path === '/api/resumable-conversations') {
      return jsonResponse({ sessions: machine.resumable ?? [] });
    }
    if (path === '/api/recovery/adopt' && method === 'POST') {
      const request = (body ?? {}) as { target?: string };
      const providerUuid = String(request.target ?? '');
      const resumed = makeSession({
        id: `resumed-${providerUuid}`,
        name: (machine.resumable ?? []).find((c) => c.sessionId === providerUuid)?.title ?? 'Resumed conversation',
        createdAt: NOW,
        lastDataAt: NOW,
        claudeSessionId: providerUuid
      });
      machine.sessions.push(resumed);
      daemon.adopted.push(providerUuid);
      return jsonResponse({
        ok: true,
        laneId: resumed.id,
        adoption: {
          path: '/Users/example/.claude/projects/x.jsonl',
          tool: 'claude',
          cwd: resumed.cwd,
          providerUuid,
          cmd: 'claude',
          args: ['--resume', providerUuid]
        }
      });
    }
    if (path === '/api/recovery/fork' && method === 'POST') {
      return jsonResponse({ ok: true, laneId: 'forked-1' });
    }

    // ── search ──────────────────────────────────────────────────────────
    if (path === '/api/search') {
      return jsonResponse(searchFor(
        machine,
        url.searchParams.get('q') ?? '',
        Number(url.searchParams.get('limit') ?? 100)
      ));
    }
    if (path === '/api/search/plan' && method === 'POST') {
      return jsonResponse({ provider: 'claude', query: String((body as { query?: string })?.query ?? '') });
    }

    // ── settings surfaces the views read on mount ───────────────────────
    if (path === '/api/profiles') return jsonResponse({ profiles: machine.profiles ?? [] });
    if (path === '/api/directories') return jsonResponse({ directories: machine.directories ?? [] });
    if (path === '/api/fs/list') {
      return jsonResponse({ path: '/Users/example', parent: '/Users', entries: [] });
    }
    if (path === '/api/models/codex') return jsonResponse({ models: [] });
    if (path === '/api/providers' && method === 'GET') {
      return jsonResponse({ providers: machine.providers ?? [] });
    }
    const providerUpdateRoute = /^\/api\/providers\/(claude|codex)\/update$/.exec(path);
    if (providerUpdateRoute && method === 'POST') {
      const provider = (machine.providers ?? []).find((item) => item.id === providerUpdateRoute[1]);
      if (!provider) return jsonResponse({ error: 'provider is not installed' }, 400);
      provider.version = provider.latestVersion ?? provider.version;
      provider.updateAvailable = false;
      return jsonResponse({ provider, output: 'updated' });
    }
    if (path === '/api/onboarding') {
      return jsonResponse({ version: 1, complete: true, remoteControl: 'enabled', delegatedAccess: 'inherit' });
    }
    if (path === '/api/claude/settings') {
      return jsonResponse({ remoteControl: 'inherit', permissionMode: 'inherit', model: '', somewhereMCP: 'inherit' });
    }
    if (path === '/api/ai/settings') return jsonResponse({ provider: 'claude' });
    if (path === '/api/recap/settings') return jsonResponse({ provider: 'off' });
    if (path === '/api/recap/dates') return jsonResponse({ dates: [] });
    if (path === '/api/access/requests') return jsonResponse({ requests: [] });
    if (path === '/api/usage') {
      return jsonResponse({
        schemaVersion: 1,
        machine: machine.name,
        generatedAt: new Date(NOW).toISOString(),
        group: 'daily',
        mode: 'auto',
        pricing: { source: 'fixture', revision: '1', url: '', note: '' },
        scan: { filesSeen: 0, filesRead: 0, linesRead: 0, entriesSeen: 0 },
        rows: [],
        totals: {
          key: 'total',
          models: [],
          tokens: { inputTokens: 0, outputTokens: 0, cacheCreationTokens: 0, cacheReadTokens: 0, reasoningTokens: 0 },
          costUSD: 0,
          recordedCostUSD: 0,
          calculatedCostUSD: 0,
          entries: 0,
          missingPricingEntries: 0
        }
      });
    }

    daemon.unhandled.push(record);
    return jsonResponse({ error: `fake daemon has no route for ${method} ${path}` }, 404);
  };

  globalThis.fetch = handle as unknown as typeof fetch;

  // Terminal streaming is out of scope for these capabilities; a socket that
  // never opens is exactly what a component sees before a daemon answers, and
  // no capability under test depends on frames arriving.
  class InertSocket {
    static readonly CONNECTING = 0;
    static readonly OPEN = 1;
    static readonly CLOSING = 2;
    static readonly CLOSED = 3;
    readonly readyState = 0;
    readonly url: string;
    constructor(url: string) { this.url = url; }
    close(): void {}
    send(): void {}
    addEventListener(): void {}
    removeEventListener(): void {}
  }
  globalThis.WebSocket = InertSocket as unknown as typeof WebSocket;

  return daemon;
}

export function serverConfigsFor(machines: FakeMachine[]): ServerConfig[] {
  return machines.map((m) => ({
    id: m.id,
    name: m.name,
    host: m.host,
    port: m.port,
    scheme: m.scheme ?? 'http',
    isDefault: m.isDefault ?? false,
    machineId: m.machineId ?? m.id,
    systemName: m.name
  }));
}

/**
 * Point lib/servers and the sessions store at the fake.
 *
 * Both modules compute their initial state from localStorage at import time,
 * which has already happened by the time a test body runs. Writing through the
 * stores is the same operation the running app performs when a machine is added
 * or selected, so nothing here is a stub of product behaviour.
 */
export function useFakeMachines(machines: FakeMachine[], activeId?: string): void {
  const servers = serverConfigsFor(machines);
  const active = activeId ?? servers.find((s) => s.isDefault)?.id ?? servers[0]?.id ?? null;
  window.localStorage.setItem('sessions:servers', JSON.stringify(servers));
  window.localStorage.setItem('sessions:active-server', active ?? '');
  useServers.setState({ servers, activeId: active });
  const activeMachine = machines.find((m) => m.id === active) ?? machines[0];
  useSessions.setState({
    serverId: active,
    // A real daemon response is deserialized into a client-owned array. Keep
    // the same ownership boundary here: lifecycle routes mutate the daemon's
    // records, while refresh/create must be what makes them visible to React.
    sessions: activeMachine?.sessions.map((session) => ({ ...session })) ?? [],
    activeId: null,
    hydrated: true,
    loading: false,
    error: null
  });
}
