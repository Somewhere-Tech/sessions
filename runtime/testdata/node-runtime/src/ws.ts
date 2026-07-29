import type { IncomingMessage } from 'node:http';
import type { Duplex } from 'node:stream';
import crypto from 'node:crypto';
import { WebSocketServer, type WebSocket } from 'ws';
import { getSession, writeInput, resize, snapshot, type OutputEvent, type SessionHandle } from './sessions.js';
import type { ClaudeSessionEvent } from './sessionFileWatcher.js';
import { PROTOCOL_VERSION, type ClientMsg, type ServerMsg } from './types.js';
import { config, getAuthToken, isAllowedOrigin, isAuthOpen } from './config.js';

// Cap inbound frames at 256 KB — plenty for any realistic input (keystrokes,
// JSON control messages, paste). A client that exceeds it is either broken or
// malicious; ws closes the socket with code 1009 automatically.
const wss = new WebSocketServer({ noServer: true, maxPayload: 256 * 1024 });

// Locally extend ClientMsg to include ping. The types stream adds this to
// types.ts; the local union keeps ws.ts compiling in the interim.
type AnyClientMsg = ClientMsg | { type: 'ping' };

// 1 MB backpressure threshold. When the WS send buffer exceeds this the
// replay loop yields until the socket drains, preventing the daemon from
// blasting a multi-MB replay (e.g. the 156 MB output-history case) into
// the TCP send buffer all at once and stalling the Node event loop.
const MAX_BUFFERED = 1 * 1024 * 1024;

// Pause until ws.bufferedAmount drops below MAX_BUFFERED, with a 5-second
// safety timeout so a hung client can't block the loop indefinitely.
async function drainIfNeeded(ws: WebSocket): Promise<void> {
  if (ws.readyState !== ws.OPEN || ws.bufferedAmount <= MAX_BUFFERED) return;
  await new Promise<void>((resolve) => {
    const onDrain = (): void => { clearTimeout(timer); resolve(); };
    // If drain never fires (client died without closing), give up after 5 s.
    const timer = setTimeout(() => { ws.off('drain', onDrain); resolve(); }, 5_000);
    ws.once('drain', onDrain);
  });
}

function send(ws: WebSocket, msg: ServerMsg): void {
  if (ws.readyState === ws.OPEN) ws.send(JSON.stringify(msg));
}

// Attach one session's stream to a socket: replay (raw output from
// lastSeq, claude events from claudeEventsSince) then live events. Both
// connection modes go through here; mux mode tags every message with the
// sessionId so the client can route. Returns a detach() that removes the
// listeners, or null if the session already exited (terminal state was
// sent; nothing live to subscribe to).
//
// Made async so the replay loops can yield to the event loop via
// drainIfNeeded() when the WS send buffer is full — prevents the daemon
// from synchronously blasting a huge replay into one giant send-buffer
// burst (the "156 MB backlog" that caused 5-second typing lag).
async function attachSessionStream(
  ws: WebSocket,
  session: SessionHandle,
  opts: {
    lastSeq: number;
    claudeEventsSince: number;
    tagSessionId?: string;
    onExited?: () => void;
    // When false, raw PTY bytes are suppressed for this attach — no
    // replay AND no live output frames. Structured claudeEvents, exit,
    // and hello still flow. See the attach message in types.ts.
    includeOutput?: boolean;
    // When false, the on-attach replay of Claude JSONL *history* is
    // suppressed. Hidden sessions
    // attach this way so page load doesn't replay every session's
    // conversation at once. See the attach message in types.ts.
    includeClaudeReplay?: boolean;
    // When false, live structured claudeEvent frames are suppressed too.
    // Hidden sessions backfill with HTTP tail pages on activation, so
    // sending frames they immediately drop is pure background cost.
    includeClaudeLive?: boolean;
  }
): Promise<(() => void) | null> {
  const tag = opts.tagSessionId !== undefined ? { sessionId: opts.tagSessionId } : {};
  const includeOutput = opts.includeOutput !== false;
  const includeClaudeReplay = opts.includeClaudeReplay !== false;
  const includeClaudeLive = opts.includeClaudeLive !== false;
  const replay = session.log.since(opts.lastSeq);

  // Claude-event replay window. Indices are ABSOLUTE (claudeEventBase +
  // array index) so they survive the 5000-cap front-trim; map the
  // client's absolute resume point into a local offset. Fresh attaches
  // (since=0) cap the initial replay to the tail — long sessions hold
  // thousands of events and RemoteView renders ~50 anyway; older history
  // stays reachable via HTTP /events.
  const INITIAL_REPLAY_CAP = 300;
  const base = session.claudeEventBase;
  const len = session.claudeEventLog.length;
  const claudeEventsCount = base + len;
  // When history replay is suppressed, start the window at the current end
  // (localStart === len) so nothing historical is sent and the client's
  // counter is reported as already caught up. If live events are enabled,
  // they take it forward from there.
  const localStart = !includeClaudeReplay
    ? len
    : opts.claudeEventsSince > 0
      ? Math.max(0, Math.min(opts.claudeEventsSince - base, len))
      : Math.max(0, len - INITIAL_REPLAY_CAP);
  const claudeReplayStart = base + localStart;

  send(ws, {
    type: 'hello',
    protocol: PROTOCOL_VERSION,
    session: session.info,
    currentSeq: replay.current,
    resumedFromSeq: opts.lastSeq > 0 ? opts.lastSeq : null,
    claudeEventsCount,
    claudeReplayStart,
    ...tag
  });

  if (includeOutput) {
    if (replay.gap) {
      send(ws, { type: 'gap', oldestAvailableSeq: replay.oldest, currentSeq: replay.current, ...tag });
    }
    for (const ev of replay.events) {
      send(ws, { type: 'output', seq: ev.seq, data: ev.data, ...tag });
      // Yield when the send buffer fills up so we don't blast a
      // multi-MB output history into a single synchronous write burst.
      await drainIfNeeded(ws);
    }
  }
  for (let i = localStart; i < len; i++) {
    send(ws, { type: 'claudeEvent', event: session.claudeEventLog[i]!, ...tag });
    await drainIfNeeded(ws);
  }

  // Already exited: deliver the terminal state and skip live wiring.
  if (session.exited) {
    send(ws, {
      type: 'exit',
      code: session.exitCode,
      signal: session.exitSignal,
      seq: session.exitSeq ?? replay.current,
      ...tag
    });
    opts.onExited?.();
    return null;
  }

  const onOutput = (ev: OutputEvent): void => {
    send(ws, { type: 'output', seq: ev.seq, data: ev.data, ...tag });
  };
  const onExit = (info: { code: number | null; signal: string | null; seq: number }): void => {
    send(ws, { type: 'exit', code: info.code, signal: info.signal, seq: info.seq, ...tag });
    opts.onExited?.();
  };
  const onClaudeEvent = (ev: ClaudeSessionEvent): void => {
    send(ws, { type: 'claudeEvent', event: ev, ...tag });
  };
  // Contract #4: when the runner process disconnects while the session is
  // still alive (e.g. launchd killed the runner), emit a synthetic exit
  // frame so the browser's session view unfreezes rather than spinning
  // forever. code=null + reason='runner-lost' distinguishes this from a
  // real PTY exit; the client's existing exit handling path fires for both.
  const onRunnerLost = (): void => {
    if (ws.readyState === ws.OPEN) {
      ws.send(JSON.stringify({
        type: 'exit',
        code: null,
        signal: null,
        seq: replay.current,
        reason: 'runner-lost',
        ...tag
      }));
    }
  };
  if (includeOutput) session.emitter.on('output', onOutput);
  session.emitter.on('exit', onExit);
  if (includeClaudeLive) session.emitter.on('claudeEvent', onClaudeEvent);
  session.emitter.on('runner-lost', onRunnerLost);

  return () => {
    if (includeOutput) session.emitter.off('output', onOutput);
    session.emitter.off('exit', onExit);
    if (includeClaudeLive) session.emitter.off('claudeEvent', onClaudeEvent);
    session.emitter.off('runner-lost', onRunnerLost);
  };
}

// Clamp client-requested PTY dimensions. Last-resize-wins across clients:
// the most recently active viewport dictates the PTY size; the TUI
// redraws at the new size automatically.
function clampedResize(sessionId: string, cols: number, rows: number): void {
  resize(sessionId, Math.max(40, Math.min(500, cols | 0)), Math.max(10, Math.min(200, rows | 0)));
}

function sendRpcError(
  ws: WebSocket,
  requestId: string,
  message: string,
  opts?: { code?: string; sessionId?: string }
): void {
  send(ws, {
    type: 'rpcError',
    requestId,
    message,
    code: opts?.code,
    sessionId: opts?.sessionId
  });
}

function sendEventsWindow(
  ws: WebSocket,
  requestId: string,
  sessionId: string,
  opts: { since?: number; tail?: number }
): void {
  const sess = getSession(sessionId);
  if (!sess) {
    sendRpcError(ws, requestId, `unknown session ${sessionId}`, {
      code: 'not_found',
      sessionId
    });
    return;
  }

  const log = sess.claudeEventLog;
  const base = sess.claudeEventBase;
  const len = log.length;
  const total = base + len;
  let start = 0;
  if (opts.since != null && Number.isFinite(opts.since) && opts.since >= 0) {
    start = Math.max(0, Math.min(opts.since - base, len));
  }
  if (opts.tail != null && Number.isFinite(opts.tail) && opts.tail > 0) {
    start = Math.max(start, Math.max(0, len - Math.floor(opts.tail)));
  }
  const events = start === 0 ? log : log.slice(start);
  send(ws, {
    type: 'events',
    requestId,
    sessionId,
    events,
    nextIndex: total,
    totalCount: total
  });
}

// Multiplexed mode (`/ws?mux=1`): one socket, many sessions (tmux-style).
// The client attaches/detaches sessions with tagged frames; every
// server→client message carries sessionId. N sessions cost ONE connection
// — the per-session-socket shape fell over once orchestrators pushed the
// session count past ~50 (dozens of parallel sockets, reconnect herds).
function handleMuxConnection(ws: WebSocket): void {
  const attached = new Map<string, () => void>(); // sessionId → detach
  // Guards against duplicate attach frames arriving while an async replay
  // is in progress (the await in attachSessionStream yields to the event
  // loop, so a second 'attach' for the same session can race through).
  const pendingAttach = new Set<string>();

  const detach = (sessionId: string): void => {
    const d = attached.get(sessionId);
    if (d) d();
    attached.delete(sessionId);
  };

  ws.on('message', (raw) => {
    // Frame-size enforcement is handled by maxPayload on the WebSocketServer
    // (code 1009 on overflow). This handler only fires for cleared frames.
    let parsed: AnyClientMsg | null = null;
    try {
      parsed = JSON.parse(raw.toString()) as AnyClientMsg;
    } catch {
      return; // mux mode is JSON-only; raw bytes have no session routing
    }
    // App-level keepalive ping (contract ws.d): JSON {type:'ping'} →
    // {type:'pong'}. Distinct from WebSocket's binary ping/pong opcodes;
    // this layer is what browser JS can actually send and receive.
    if (parsed.type === 'ping') {
      if (ws.readyState === ws.OPEN) ws.send(JSON.stringify({ type: 'pong' }));
      return;
    }
    if (parsed.type === 'attach') {
      const attachMsg = parsed; // capture narrowed type for the async closure
      const id = attachMsg.sessionId;
      if (!id || attached.has(id) || pendingAttach.has(id)) return;
      const session = getSession(id);
      if (!session) {
        send(ws, { type: 'error', message: `unknown session ${id}`, sessionId: id });
        return;
      }
      pendingAttach.add(id);
      void (async (): Promise<void> => {
        const det = await attachSessionStream(ws, session, {
          lastSeq: Math.max(0, (attachMsg.lastSeq ?? 0) | 0),
          claudeEventsSince: Math.max(0, (attachMsg.claudeEventsSince ?? 0) | 0),
          tagSessionId: id,
          includeOutput: attachMsg.outputReplay !== false,
          includeClaudeReplay: attachMsg.claudeReplay !== false,
          includeClaudeLive: attachMsg.claudeLive !== false,
          // On PTY exit only this session detaches — the socket stays up
          // for every other attached session.
          onExited: () => detach(id)
        });
        pendingAttach.delete(id);
        if (det) attached.set(id, det);
      })();
      return;
    }
    if (parsed.type === 'detach') {
      if (parsed.sessionId) detach(parsed.sessionId);
      return;
    }
    if (parsed.type === 'snapshot') {
      const { requestId, sessionId } = parsed;
      if (!requestId || !sessionId) return;
      void (async (): Promise<void> => {
        try {
          const cols = parsed.cols && parsed.cols > 0 ? { cols: Math.floor(parsed.cols) } : undefined;
          const result = await snapshot(sessionId, cols);
          if (!result) {
            sendRpcError(ws, requestId, `unknown session ${sessionId}`, {
              code: 'not_found',
              sessionId
            });
            return;
          }
          send(ws, {
            type: 'snapshot',
            requestId,
            sessionId,
            text: result.text,
            seq: result.seq
          });
        } catch (err) {
          sendRpcError(ws, requestId, (err as Error).message, { sessionId });
        }
      })();
      return;
    }
    if (parsed.type === 'events') {
      const { requestId, sessionId } = parsed;
      if (!requestId || !sessionId) return;
      sendEventsWindow(ws, requestId, sessionId, {
        since: parsed.since,
        tail: parsed.tail
      });
      return;
    }
    if (parsed.type === 'input') {
      if (!parsed.sessionId) return;
      const ok = writeInput(parsed.sessionId, parsed.data);
      if (parsed.requestId) {
        send(ws, {
          type: 'inputAck',
          requestId: parsed.requestId,
          sessionId: parsed.sessionId,
          ok
        });
      }
      return;
    }
    if (parsed.type === 'resize') {
      if (parsed.sessionId) clampedResize(parsed.sessionId, parsed.cols, parsed.rows);
      return;
    }
  });

  ws.on('close', () => {
    for (const d of attached.values()) d();
    attached.clear();
  });
}

// Single-session mode (`/ws?sessionId=…`): the original protocol. Still
// used by the CLI (`pretty attach`, `pretty tail -f`) and any client
// that hasn't moved to mux.
//
// Message and close handlers are registered BEFORE awaiting the async
// replay so input arriving during a slow drain (large session history)
// is not silently discarded.
async function handleSingleConnection(ws: WebSocket, url: URL, sessionId: string): Promise<void> {
  const session = getSession(sessionId);
  if (!session) {
    send(ws, { type: 'error', message: `unknown session ${sessionId}` });
    ws.close(1008, 'unknown session');
    return;
  }

  const lastSeqParam = url.searchParams.get('lastSeq');
  const lastSeq = lastSeqParam ? Math.max(0, Number(lastSeqParam) | 0) : 0;
  const claudeSinceParam = url.searchParams.get('claudeEventsSince');
  const claudeEventsSince = claudeSinceParam ? Math.max(0, Number(claudeSinceParam) | 0) : 0;

  // detachFn is set after the async replay resolves; the close handler
  // holds a reference so it can clean up regardless of timing.
  let detachFn: (() => void) | null = null;

  ws.on('message', (raw, isBinary) => {
    if (isBinary) {
      writeInput(sessionId, (raw as Buffer).toString('utf8'));
      return;
    }
    const text = raw.toString();
    let parsed: AnyClientMsg | null = null;
    try {
      parsed = JSON.parse(text) as AnyClientMsg;
    } catch {
      writeInput(sessionId, text);
      return;
    }
    // App-level keepalive — see handleMuxConnection for rationale.
    if (parsed.type === 'ping') {
      if (ws.readyState === ws.OPEN) ws.send(JSON.stringify({ type: 'pong' }));
      return;
    }
    if (parsed.type === 'input') {
      writeInput(sessionId, parsed.data);
    } else if (parsed.type === 'resize') {
      clampedResize(sessionId, parsed.cols, parsed.rows);
    }
  });

  ws.on('close', () => {
    detachFn?.();
  });

  detachFn = await attachSessionStream(ws, session, {
    lastSeq,
    claudeEventsSince,
    onExited: () => ws.close(1000, 'pty exited')
  });
  // null means the session was already exited when we attached — the exit
  // frame and onExited callback were handled inside attachSessionStream.
}

wss.on('connection', (ws, req) => {
  const url = new URL(req.url ?? '/', 'http://localhost');
  if (url.searchParams.get('mux') === '1') {
    handleMuxConnection(ws);
    return;
  }
  const sessionId = url.searchParams.get('sessionId');
  if (!sessionId) {
    send(ws, { type: 'error', message: 'missing sessionId' });
    ws.close(1008, 'missing sessionId');
    return;
  }
  void handleSingleConnection(ws, url, sessionId);
});

export function handleUpgrade(req: IncomingMessage, socket: Duplex, head: Buffer): void {
  const url = new URL(req.url ?? '/', 'http://localhost');
  if (url.pathname !== '/ws') {
    socket.destroy();
    return;
  }

  // Reject cross-origin browser requests BEFORE completing the WebSocket
  // handshake — CSRF mitigation. Non-browser clients (CLI, curl) send no
  // Origin and are always allowed.
  if (!isAllowedOrigin(req.headers.origin, config.host)) {
    socket.write('HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n');
    socket.destroy();
    return;
  }

  // Validate the auth token from the ?token= query param. Browsers cannot
  // set custom WebSocket headers, so the token travels in the URL per
  // contract #5. Use timingSafeEqual to avoid timing-oracle attacks.
  const qToken = url.searchParams.get('token') ?? '';
  const authToken = getAuthToken();
  const authBuf = Buffer.from(authToken, 'utf8');
  const qBuf = Buffer.from(qToken, 'utf8');
  // Length check short-circuits before timingSafeEqual (equal-length
  // requirement) but still leaks no timing info about the token value.
  const tokenOk = qBuf.length > 0 &&
    qBuf.length === authBuf.length &&
    crypto.timingSafeEqual(qBuf, authBuf);
  if (!tokenOk && !isAuthOpen()) {
    // Respond with HTTP 401 before destroying the socket. Using close
    // code 4401 post-upgrade would require completing the WS handshake
    // first, which we avoid so the socket is never treated as live.
    socket.write('HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n');
    socket.destroy();
    return;
  }

  wss.handleUpgrade(req, socket, head, (ws) => {
    wss.emit('connection', ws, req);
  });
}
