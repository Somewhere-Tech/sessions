// Multiplexed WebSocket manager — ONE socket per (window, server) carrying
// every attached session's traffic as sessionId-tagged frames.
//
// Why: the previous shape opened one WebSocket per mounted SessionView.
// With orchestrators pushing the session count past 50, that meant 50+
// parallel sockets per window — reconnect herds on every daemon restart,
// and a dev-proxy/socket pile-up that left "open" sockets dead (input
// silently dropped). tmux solves the same problem with a single
// multiplexed client connection; this is that, for the browser.
//
// useTerminal attaches each session via attachSession() and receives the
// exact same ServerMsg stream it used to read off its private socket.
// The manager owns: connection lifecycle, exponential backoff, the
// visibilitychange reconnect nudge (phone unlock), and re-attaching every
// registered session after a reconnect (asking each for its current
// resume point so replays stay incremental).

import type { ServerMsg, MuxClientMsg, ClaudeSessionEvent } from '../types';

export type MuxStatus = 'connecting' | 'open' | 'reconnecting' | 'closed' | 'error';

export interface SessionHandlers {
  onMessage: (msg: ServerMsg) => void;
  onStatus: (status: MuxStatus) => void;
  // Called on (re)connect to build the attach frame — returns where this
  // session's client-side state is, so the server replays only deltas.
  // outputReplay=false means "no raw PTY bytes at all" (no terminal is
  // mounted for this session; only claudeEvents are consumed).
  // claudeReplay=false means "no Claude history replay". claudeLive=false
  // means "no live Claude event frames"; hidden sessions backfill when
  // activated instead of receiving frames they will drop.
  getResume: () => { lastSeq: number; claudeEventsSince: number; outputReplay: boolean; claudeReplay: boolean; claudeLive: boolean };
}

export interface SessionChannel {
  sendInput(data: string): void;
  sendResize(cols: number, rows: number): void;
  detach(): void;
}

const RECONNECT_BACKOFF_MS = [500, 1000, 2000, 4000, 8000] as const;

// Cap on input/resize frames buffered while the socket is down. Keystrokes
// are tiny; this is seconds of furious typing. Bounded so a socket that
// never comes back can't grow it without limit.
const OUTBOX_CAP = 2000;
const REQUEST_TIMEOUT_MS = 10_000;
const IDLE_SHUTDOWN_MS = 15_000;

// Application-level keepalive constants.  TCP RST from the peer is normally
// visible, but silently-dead NAT/VPN tunnels and some Wi-Fi stacks drop the
// TCP FIN and leave the socket open-but-dead — keystrokes then vanish.
// Every PING_INTERVAL_MS we send {type:'ping'}; if no message at all arrives
// for STALE_TIMEOUT_MS we force-close so the existing reconnect path fires.
const PING_INTERVAL_MS  = 20_000; // 20 s — send a ping this often
const STALE_TIMEOUT_MS  = 30_000; // 30 s — declare connection dead if silent

type RpcResponse = Extract<ServerMsg, { requestId: string }>;

interface PendingRequest {
  resolve: (msg: RpcResponse) => void;
  reject: (err: Error) => void;
  timer: number;
}

class MuxRpcError extends Error {
  readonly code: string | undefined;
  constructor(message: string, code?: string) {
    super(message);
    this.name = 'MuxRpcError';
    this.code = code;
  }
}

let nextRequestSeq = 1;
function newRequestId(): string {
  const seq = nextRequestSeq++;
  return `mux-${Date.now().toString(36)}-${seq.toString(36)}`;
}

function hasRequestId(msg: MuxClientMsg): msg is MuxClientMsg & { requestId: string } {
  return typeof (msg as { requestId?: unknown }).requestId === 'string';
}

function isRpcResponse(msg: ServerMsg): msg is RpcResponse {
  return typeof (msg as { requestId?: unknown }).requestId === 'string';
}

function isQueueableWhileClosed(msg: MuxClientMsg): boolean {
  return msg.type === 'input' ||
    msg.type === 'resize' ||
    msg.type === 'snapshot' ||
    msg.type === 'events';
}

class MuxManager {
  private ws: WebSocket | null = null;
  private status: MuxStatus = 'connecting';
  private attempt = 0;
  private reconnectTimer: number | null = null;
  private pingInterval: number | null = null;
  private lastMessageAt = 0;   // epoch ms of most recent server-sent frame
  private readonly sessions = new Map<string, SessionHandlers>();
  private readonly requests = new Map<string, PendingRequest>();
  private idleShutdownTimer: number | null = null;
  // Input/resize typed while the socket isn't OPEN (initial connect, or a
  // reconnect blip after phone-sleep / network handoff). Without this they
  // were silently dropped — you'd click a terminal that LOOKS ready (the
  // snapshot is already painted) and type into the void until the socket
  // happened to be OPEN. Flushed in order on reopen, after re-attach.
  private readonly outbox: MuxClientMsg[] = [];
  private readonly onVis = (): void => {
    // Phone unlock / tab foreground: if the socket died while backgrounded,
    // reconnect immediately instead of waiting out the backoff timer.
    if (document.visibilityState !== 'visible') return;
    if (this.sessions.size === 0 && this.requests.size === 0 && this.outbox.length === 0) return;
    if (!this.ws || this.ws.readyState >= WebSocket.CLOSING) {
      if (this.reconnectTimer !== null) {
        window.clearTimeout(this.reconnectTimer);
        this.reconnectTimer = null;
      }
      this.connect();
    }
  };

  constructor(private readonly url: string) {
    document.addEventListener('visibilitychange', this.onVis);
  }

  attach(sessionId: string, handlers: SessionHandlers): SessionChannel {
    this.cancelIdleShutdown();
    this.sessions.set(sessionId, handlers);
    if (this.ws && this.ws.readyState === WebSocket.OPEN) {
      handlers.onStatus('open');
      this.sendAttach(sessionId, handlers);
    } else {
      handlers.onStatus(this.status === 'open' ? 'connecting' : this.status);
      if (!this.ws || this.ws.readyState >= WebSocket.CLOSING) this.connect();
    }
    return {
      sendInput: (data: string) => this.send({ type: 'input', data, sessionId }),
      sendResize: (cols: number, rows: number) => this.send({ type: 'resize', cols, rows, sessionId }),
      detach: () => {
        this.sessions.delete(sessionId);
        this.send({ type: 'detach', sessionId });
        if (this.sessions.size === 0) this.scheduleIdleShutdown();
      }
    };
  }

  request(msg: MuxClientMsg & { requestId: string }, timeoutMs: number = REQUEST_TIMEOUT_MS): Promise<RpcResponse> {
    this.cancelIdleShutdown();
    return new Promise((resolve, reject) => {
      const timer = window.setTimeout(() => {
        this.requests.delete(msg.requestId);
        this.removeQueuedRequest(msg.requestId);
        reject(new Error(`sessionsd mux request timed out: ${msg.type}`));
        this.scheduleIdleShutdown();
      }, timeoutMs);
      this.requests.set(msg.requestId, { resolve, reject, timer });
      this.send(msg);
      if (!this.ws || this.ws.readyState >= WebSocket.CLOSING) {
        if (this.reconnectTimer === null) this.connect();
      }
    });
  }

  private send(msg: MuxClientMsg): void {
    if (this.ws && this.ws.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify(msg));
      return;
    }
    // Socket not OPEN. Queue input & resize so they're delivered on reopen
    // instead of silently lost. RPC frames queue the same way. attach/detach
    // are NOT queued — they're rebuilt from the live `sessions` map on
    // reconnect, so a stale queued attach/detach would only fight that.
    if (isQueueableWhileClosed(msg)) {
      this.outbox.push(msg);
      if (this.outbox.length > OUTBOX_CAP) {
        const dropped = this.outbox.splice(0, this.outbox.length - OUTBOX_CAP);
        this.rejectDroppedRequests(dropped);
      }
      // A queued keystroke means we WANT a live socket. If none is pending,
      // kick a connect now rather than waiting for the next backoff tick.
      if (!this.ws || this.ws.readyState >= WebSocket.CLOSING) {
        if (this.reconnectTimer === null) this.connect();
      }
    }
  }

  private completeRequest(msg: RpcResponse): void {
    const pending = this.requests.get(msg.requestId);
    if (!pending) return;
    window.clearTimeout(pending.timer);
    this.requests.delete(msg.requestId);
    if (msg.type === 'rpcError') {
      pending.reject(new MuxRpcError(msg.message, msg.code));
    } else {
      pending.resolve(msg);
    }
    this.scheduleIdleShutdown();
  }

  private removeQueuedRequest(requestId: string): void {
    for (let i = this.outbox.length - 1; i >= 0; i--) {
      const msg = this.outbox[i];
      if (msg && hasRequestId(msg) && msg.requestId === requestId) {
        this.outbox.splice(i, 1);
      }
    }
  }

  private rejectDroppedRequests(dropped: MuxClientMsg[]): void {
    for (const msg of dropped) {
      if (!hasRequestId(msg)) continue;
      const pending = this.requests.get(msg.requestId);
      if (!pending) continue;
      window.clearTimeout(pending.timer);
      this.requests.delete(msg.requestId);
      pending.reject(new Error(`sessionsd mux request dropped while offline: ${msg.type}`));
    }
  }

  private cancelIdleShutdown(): void {
    if (this.idleShutdownTimer !== null) {
      window.clearTimeout(this.idleShutdownTimer);
      this.idleShutdownTimer = null;
    }
  }

  private scheduleIdleShutdown(): void {
    if (this.sessions.size > 0 || this.requests.size > 0 || this.outbox.length > 0) return;
    if (this.idleShutdownTimer !== null) return;
    this.idleShutdownTimer = window.setTimeout(() => {
      this.idleShutdownTimer = null;
      if (this.sessions.size === 0 && this.requests.size === 0 && this.outbox.length === 0) {
        this.shutdownSocket();
      }
    }, IDLE_SHUTDOWN_MS);
  }

  private sendAttach(sessionId: string, handlers: SessionHandlers): void {
    const r = handlers.getResume();
    const msg: MuxClientMsg = { type: 'attach', sessionId };
    if (r.lastSeq > 0) msg.lastSeq = r.lastSeq;
    if (r.claudeEventsSince > 0) msg.claudeEventsSince = r.claudeEventsSince;
    if (!r.outputReplay) msg.outputReplay = false;
    if (!r.claudeReplay) msg.claudeReplay = false;
    if (!r.claudeLive) msg.claudeLive = false;
    this.send(msg);
  }

  private setStatus(s: MuxStatus): void {
    this.status = s;
    for (const h of this.sessions.values()) h.onStatus(s);
  }

  // ── keepalive ────────────────────────────────────────────────────────────
  // Send {type:'ping'} every PING_INTERVAL_MS.  If no message has arrived
  // from the server for STALE_TIMEOUT_MS, force-close the socket so the
  // existing onclose→reconnect path fires.  This catches silently-dead
  // NAT/VPN tunnels that never send a TCP RST.
  private startPing(sock: WebSocket): void {
    this.stopPing();
    this.pingInterval = window.setInterval(() => {
      if (sock !== this.ws || sock.readyState !== WebSocket.OPEN) {
        this.stopPing();
        return;
      }
      if (this.lastMessageAt > 0 && Date.now() - this.lastMessageAt > STALE_TIMEOUT_MS) {
        // No traffic for 30 s → silently-dead connection.  Force-close so
        // onclose triggers a reconnect rather than leaving keystrokes in the void.
        this.stopPing();
        sock.close();
        return;
      }
      sock.send(JSON.stringify({ type: 'ping' }));
    }, PING_INTERVAL_MS);
  }

  private stopPing(): void {
    if (this.pingInterval !== null) {
      window.clearInterval(this.pingInterval);
      this.pingInterval = null;
    }
  }
  // ─────────────────────────────────────────────────────────────────────────

  private connect(): void {
    if (this.sessions.size === 0 && this.requests.size === 0 && this.outbox.length === 0) return;
    this.cancelIdleShutdown();
    this.stopPing(); // clear any interval from the previous socket
    this.setStatus(this.attempt === 0 ? 'connecting' : 'reconnecting');
    const sock = new WebSocket(this.url);
    this.ws = sock;

    sock.onopen = () => {
      if (sock !== this.ws) return;
      this.attempt = 0;
      this.lastMessageAt = Date.now(); // treat open as first "message" so stale check has a baseline
      this.setStatus('open');
      this.startPing(sock);
      // (Re-)attach every registered session at its current resume point.
      for (const [id, handlers] of this.sessions) this.sendAttach(id, handlers);
      // Then flush input/resize typed while we were down — AFTER re-attach,
      // so the server has each session subscribed on this connection first.
      if (this.outbox.length > 0) {
        const pending = this.outbox.splice(0, this.outbox.length);
        for (const m of pending) {
          if (sock.readyState === WebSocket.OPEN) sock.send(JSON.stringify(m));
        }
      }
    };

    sock.onmessage = (ev) => {
      if (sock !== this.ws) return;
      if (typeof ev.data !== 'string') return;
      // Any incoming frame (including pong) resets the staleness clock.
      this.lastMessageAt = Date.now();
      let msg: ServerMsg;
      try {
        const parsed: unknown = JSON.parse(ev.data);
        // Minimal runtime validation before trusting the cast — guards against
        // malformed frames (missing type, wrong shape) causing downstream errors.
        if (!parsed || typeof (parsed as { type?: unknown }).type !== 'string') return;
        msg = parsed as ServerMsg;
      } catch {
        return;
      }
      // {type:'pong'} has no sessionId — silently consumed; lastMessageAt already updated.
      if (isRpcResponse(msg)) {
        this.completeRequest(msg);
        return;
      }
      const sid = (msg as { sessionId?: string }).sessionId;
      if (!sid) return; // mux frames are always tagged; pong / unknown frames stop here
      this.sessions.get(sid)?.onMessage(msg);
    };

    sock.onerror = () => {
      if (sock !== this.ws) return;
      this.setStatus('error');
    };

    sock.onclose = () => {
      if (sock !== this.ws) return;
      this.stopPing(); // stop sending pings into a closed socket
      this.ws = null;
      if (this.sessions.size === 0 && this.requests.size === 0 && this.outbox.length === 0) {
        this.setStatus('closed');
        return;
      }
      const delay = RECONNECT_BACKOFF_MS[Math.min(this.attempt, RECONNECT_BACKOFF_MS.length - 1)] ?? 8000;
      this.attempt += 1;
      this.setStatus('reconnecting');
      this.reconnectTimer = window.setTimeout(() => {
        this.reconnectTimer = null;
        this.connect();
      }, delay);
    };
  }

  private shutdownSocket(): void {
    this.stopPing();
    this.cancelIdleShutdown();
    if (this.reconnectTimer !== null) {
      window.clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    // No sessions left → drop any frames still queued for a dead window.
    this.outbox.length = 0;
    const sock = this.ws;
    this.ws = null; // socket handlers no-op via identity checks
    try { sock?.close(); } catch { /* ignore */ }
  }
}

// One manager per distinct mux URL (i.e. per server). Switching the
// active server yields a different URL → its own socket; the previous
// manager idles out when its last session detaches.
const managers = new Map<string, MuxManager>();

export function attachSession(muxUrl: string, sessionId: string, handlers: SessionHandlers): SessionChannel {
  let m = managers.get(muxUrl);
  if (!m) {
    m = new MuxManager(muxUrl);
    managers.set(muxUrl, m);
  }
  return m.attach(sessionId, handlers);
}

function managerFor(muxUrl: string): MuxManager {
  let m = managers.get(muxUrl);
  if (!m) {
    m = new MuxManager(muxUrl);
    managers.set(muxUrl, m);
  }
  return m;
}

function isNotFound(err: unknown): boolean {
  return err instanceof MuxRpcError && err.code === 'not_found';
}

export interface MuxSnapshot {
  text: string;
  seq: number;
}

export async function requestSnapshot(
  muxUrl: string,
  sessionId: string,
  opts?: { cols?: number }
): Promise<MuxSnapshot | null> {
  const requestId = newRequestId();
  try {
    const msg = await managerFor(muxUrl).request({
      type: 'snapshot',
      requestId,
      sessionId,
      cols: opts?.cols
    });
    if (msg.type !== 'snapshot') throw new Error(`unexpected mux response: ${msg.type}`);
    return { text: msg.text, seq: msg.seq };
  } catch (err) {
    if (isNotFound(err)) return null;
    throw err;
  }
}

export interface MuxEventsResponse {
  events: ClaudeSessionEvent[];
  nextIndex: number;
  totalCount: number;
}

export async function requestClaudeEvents(
  muxUrl: string,
  sessionId: string,
  opts?: { tail?: number; since?: number }
): Promise<MuxEventsResponse | null> {
  const requestId = newRequestId();
  try {
    const msg = await managerFor(muxUrl).request({
      type: 'events',
      requestId,
      sessionId,
      tail: opts?.tail,
      since: opts?.since
    });
    if (msg.type !== 'events') throw new Error(`unexpected mux response: ${msg.type}`);
    return {
      events: msg.events,
      nextIndex: msg.nextIndex,
      totalCount: msg.totalCount
    };
  } catch (err) {
    if (isNotFound(err)) return null;
    throw err;
  }
}

export async function sendSessionInput(
  muxUrl: string,
  sessionId: string,
  data: string
): Promise<void> {
  const requestId = newRequestId();
  const msg = await managerFor(muxUrl).request({
    type: 'input',
    requestId,
    sessionId,
    data
  });
  if (msg.type !== 'inputAck') throw new Error(`unexpected mux response: ${msg.type}`);
  if (!msg.ok) throw new Error(`unknown session ${sessionId}`);
}
