import { useCallback, useEffect, useRef, useState } from 'react';
// xterm + its CSS are dynamically imported below — Vite splits them into
// a separate chunk so the initial bundle (Sessions / Remote / Reflowed —
// the views most users hit first) doesn't pay xterm's ~250KB parse cost.
// On a fresh Android install over cellular, this is the difference
// between "instant tap-to-content" and "wait for the terminal lib to
// download even though you didn't open Terminal view."
import { muxEndpointKey, snapshot as fetchServerSnapshot, fetchClaudeEvents } from '../api/sessionsd';
import { attachSession, sendSessionInput, submitSessionMessage, type SessionChannel, type MuxStatus } from '../lib/wsMux';
import { useServers } from '../lib/servers';
import { isTauri } from '../lib/tauriBridge';
import { terminalRenderer } from '../lib/terminalRenderer';
import type { ServerMsg, ClaudeSessionEvent } from '../types';

type Status = 'connecting' | 'open' | 'reconnecting' | 'closed' | 'error';

interface UseTerminalResult {
  containerRef: (el: HTMLDivElement | null) => void;
  status: Status;
  exitInfo: { code: number | null; signal: string | null } | null;
  resumedFromSeq: number | null;
  // Send raw input (text or control bytes) through the live WS. Lets
  // InputBar share the same channel as the xterm itself — xterm echoes
  // the result back, so the terminal stays the source of truth.
  sendInputRef: { current: (data: string) => void };
  // Composer sends are acknowledged by sessionsd. Unlike raw terminal
  // keystrokes, they are never queued through a reconnect: failure leaves the
  // user's draft in place so delivery is explicit and retryable.
  sendConfirmedInputRef: { current: (data: string) => Promise<void> };
  submitMessageRef: { current: (data: string) => Promise<void> };
  // Scroll position state for the floating "scroll to latest" button.
  // True when xterm's viewport is parked at the live tail; flips false
  // as soon as the user scrolls up. Driven by xterm's onScroll event.
  terminalAtBottom: boolean;
  // Imperative jump-to-bottom on xterm. Wired to the floating button.
  scrollTerminalToBottomRef: { current: () => void };
  // Imperative focus of xterm's hidden input textarea. Wired to a
  // pointer-down handler on the terminal pane so ANY click in the pane
  // (including the 8px padding ring, which isn't part of .xterm and so
  // never triggered xterm's own click-to-focus) puts the cursor in the
  // terminal. Without this, the shared-socket rewrite left tab switches
  // with no focus call at all (display toggle fires no 'open' status),
  // so you'd click a visible terminal and type into nothing.
  focusTerminalRef: { current: () => void };
  // Imperative re-fit (FitAddon) of xterm to its container. Fired when a
  // tab becomes active, since fits are gated to the visible session.
  fitTerminalRef: { current: () => void };
  // Structured provider events captured from the same WS as raw bytes.
  // Claude sessions are sourced from JSONL; Codex sessions use normalized
  // rollouts or the app-server history stream. RemoteView consumes these
  // instead of parsing a TUI snapshot. The wire retains its historical
  // claudeEvent naming until the next protocol-version bump.
  claudeEvents: ClaudeSessionEvent[];
  historyPending: boolean;
  hasEarlierClaudeEvents: boolean;
  loadingEarlierClaudeEvents: boolean;
  loadEarlierClaudeEventsRef: { current: () => void };
}

// Bounded Sessions history. A RemoteView renders ~50 recent messages; 300
// structured events covers that tail generously without walking weeks of
// JSONL on every switch. Older chunks are paged in explicitly, and the hard
// cap prevents an intentionally expanded session from becoming unbounded.
const CLAUDE_EVENT_TAIL = 300;
const CLAUDE_EVENT_PAGE = 300;
const CLAUDE_EVENT_HELD_CAP = 1200;

// Connection model: every session in this window shares ONE multiplexed
// WebSocket (lib/wsMux) with sessionId-tagged frames. This
// hook attaches its session to that shared socket and receives exactly
// the ServerMsg stream it used to read off a private socket. With 50+
// sessions mounted, the old one-socket-per-session shape meant reconnect
// herds on every daemon restart and proxy pile-ups that left "open"
// sockets dead (typing went nowhere).
//
// `mountTerminal=true` enables full xterm rendering: dynamic import,
// term.open(container), FitAddon resize, snapshot prefill, the works.
// When `false`, the hook still attaches to the mux socket so InputBar can
// send bytes and Remote can receive active-session events — but skips the
// ~250KB xterm instance, its DOM tree, and the FitAddon ResizeObserver.
//
// `isActive` is whether this is the session the user is currently viewing.
// Only the active session loads Claude conversation history. Activation
// fetches a bounded HTTP tail, then the WS resumes from that tail's end so
// only race deltas replay. Hidden sessions attach without output or Claude
// event frames and backfill when activated.
export function useTerminal(sessionId: string | null, mountTerminal: boolean = true, isActive: boolean = true): UseTerminalResult {
  const [status, setStatus] = useState<Status>('connecting');
  const [exitInfo, setExitInfo] = useState<{ code: number | null; signal: string | null } | null>(null);
  const [resumedFromSeq, setResumedFromSeq] = useState<number | null>(null);
  const [claudeEvents, setClaudeEvents] = useState<ClaudeSessionEvent[]>([]);
  const [historyPending, setHistoryPending] = useState(true);
  const [hasEarlierClaudeEvents, setHasEarlierClaudeEvents] = useState(false);
  const [loadingEarlierClaudeEvents, setLoadingEarlierClaudeEvents] = useState(false);
  const containerElRef = useRef<HTMLDivElement | null>(null);
  // Current activeness, readable from the attach closure without making it
  // an effect dependency. Updated every render below.
  const isActiveRef = useRef(isActive);
  isActiveRef.current = isActive;
  const sendInputRef = useRef<(data: string) => void>(() => {});
  const sendConfirmedInputRef = useRef<(data: string) => Promise<void>>(() =>
    Promise.reject(new Error('Sessions is reconnecting. Your message was not sent.'))
  );
  const submitMessageRef = useRef<(data: string) => Promise<void>>(() =>
    Promise.reject(new Error('Sessions is reconnecting. Your message was not sent.'))
  );
  const scrollTerminalToBottomRef = useRef<() => void>(() => {});
  const focusTerminalRef = useRef<() => void>(() => {});
  const loadEarlierClaudeEventsRef = useRef<() => void>(() => {});
  // Re-subscribe this session's mux stream with flags recomputed for the
  // current activeness, and re-fit the terminal — invoked when isActive
  // flips (tab switch). Set inside the mount effect once the channel
  // exists; no-ops until then.
  const reattachRef = useRef<(active: boolean) => void>(() => {});
  const fitTerminalRef = useRef<() => void>(() => {});
  const [terminalAtBottom, setTerminalAtBottom] = useState(true);
  // Re-attach whenever the mux endpoint changes. Same effect-key pattern as
  // sessionId — no need to share buffers across servers. This is the full
  // socket URL rather than the server id because the auth token is carried in
  // the query string: pasting a token for the machine you are already on keeps
  // the id identical while changing the endpoint this stream must bind to.
  // The selector re-runs on every servers-store change and the hook only
  // re-renders when the resulting URL actually differs.
  const muxUrl = useServers(() => muxEndpointKey());

  useEffect(() => {
    if (!sessionId) return;
    // No machine configured yet — nothing to bind a stream to. The effect
    // re-runs as soon as one is, because muxUrl is a dependency.
    if (!muxUrl) return;

    // We do the entire setup in an async IIFE so we can `await` the
    // dynamic-import of xterm. The cleanup function returned from
    // useEffect captures `disposed` + the lazily-assigned `runCleanup`
    // closure — if the effect tears down before xterm finishes loading
    // (rare, but happens on rapid session switches), we just bail.
    let disposed = false;
    let runCleanup: (() => void) | null = null;

    (async () => {
      // xterm refs — null when mountTerminal=false. Every term.* call
      // is wrapped in `if (term)` so the mux / claudeEvents path runs
      // identically in either mode.
      let term: import('@xterm/xterm').Terminal | null = null;
      let fit: import('@xterm/addon-fit').FitAddon | null = null;
      let scrollDisp: { dispose: () => void } | null = null;
      let dataDisp: { dispose: () => void } | null = null;
      let ro: ResizeObserver | null = null;
      let repaintAlternateScreenAfterWrite = false;

      if (mountTerminal) {
        const [xtermMod, serializeMod, fitMod, webglMod, canvasMod] = await Promise.all([
          import('@xterm/xterm'),
          import('@xterm/addon-serialize'),
          import('@xterm/addon-fit'),
          // GPU renderers (maintained @xterm/* 5.5 generation, matching the
          // core). Kept in the same lazy chunk. See the loadAddon block
          // after open() — this is THE fix for slow typing.
          import('@xterm/addon-webgl'),
          import('@xterm/addon-canvas'),
          // CSS side-effect import — Vite injects the stylesheet on resolve.
          // Keeps the CSS in the same lazy chunk as the JS so the initial
          // bundle stays slim. Discard the unused module value via void.
          import('@xterm/xterm/css/xterm.css').then(() => undefined)
        ]);
        if (disposed) return;
        const { Terminal } = xtermMod;
        const { SerializeAddon } = serializeMod;
        const { FitAddon } = fitMod;

        const container = containerElRef.current;
        if (container) {
          term = new Terminal({
            cursorBlink: true,
            fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace',
            fontSize: 13,
            scrollback: 5000,
            theme: {
              background: '#0a0a0a',
              foreground: '#e6e6e6'
            },
            allowProposedApi: true
          });
          const serialize = new SerializeAddon();
          fit = new FitAddon();
          term.loadAddon(serialize);
          term.loadAddon(fit);
          term.open(container);
          // GPU renderer — THE fix for slow typing. The default DOM renderer
          // rebuilds one <span>-run-per-style for every dirty row each frame
          // and reflows them; at 254×127 with Claude's full-pane repaint that
          // is thousands of DOM nodes + layout per keystroke (tens of ms of
          // main-thread work = the browser-side echo lag). WebGL/canvas
          // rasterize glyphs to a single canvas with no per-cell DOM or
          // reflow. MUST load AFTER open(). Chain: webgl → canvas → DOM, all
          // in try/catch so a missing GPU context degrades safely to the old
          // behavior instead of blanking the terminal. With the live-session
          // cap (≤3 mounted), the WebGL per-page context limit isn't a risk;
          // term.dispose() (runCleanup) frees the context on unmount.
          // WKWebView has a separate failure mode: Claude's full-screen
          // settings/MCP pickers rapidly erase and repaint rows, and both GPU
          // and retained-canvas layers can leave old glyphs composited over a
          // new frame. Native Apple WebViews stay on xterm's DOM renderer for
          // correctness. Chromium/WebView2 keeps WebGL and falls back to
          // Canvas on context loss.
          const renderer = terminalRenderer(isTauri(), navigator.userAgent);
          repaintAlternateScreenAfterWrite = renderer === 'dom';
          if (renderer === 'dom') {
            // No addon: xterm's built-in DOM renderer is the reliable Apple
            // WebView path. Output batching and active-session mounting keep
            // its main-thread cost bounded.
          } else if (renderer === 'canvas') {
            try { term.loadAddon(new canvasMod.CanvasAddon()); } catch { /* stay on DOM */ }
          } else {
            try {
              const webgl = new webglMod.WebglAddon();
              webgl.onContextLoss(() => {
                try { webgl.dispose(); } catch { /* ignore */ }
                try { term?.loadAddon(new canvasMod.CanvasAddon()); } catch { /* stay on DOM */ }
              });
              term.loadAddon(webgl);
            } catch {
              try { term.loadAddon(new canvasMod.CanvasAddon()); } catch { /* stay on DOM */ }
            }
          }
          scrollDisp = term.onScroll(() => {
            const buf = term!.buffer.active;
            const atBottom = buf.viewportY >= buf.baseY - 2;
            setTerminalAtBottom((prev) => (prev === atBottom ? prev : atBottom));
          });
          setTerminalAtBottom(true);
          scrollTerminalToBottomRef.current = (): void => {
            try { term?.scrollToBottom(); } catch { /* ignore */ }
          };
          focusTerminalRef.current = (): void => {
            try { term?.focus(); } catch { /* ignore */ }
          };
        }
      }

      // The session's channel on the shared mux socket. Assigned by
      // attachNow below; input before then is dropped (matches the old
      // "socket not open yet" behavior).
      let channel: SessionChannel | null = null;

      sendInputRef.current = (data: string): void => {
        channel?.sendInput(data);
      };
      // Bound to the same `muxUrl` this effect attached with, never
      // re-derived at send time. Re-deriving let a composer send open a
      // second socket on a newly-saved token while the stream stayed on the
      // old one — the composer looked healthy while the terminal was dead.
      sendConfirmedInputRef.current = (data: string): Promise<void> =>
        sendSessionInput(muxUrl, sessionId, data);
      submitMessageRef.current = (data: string): Promise<void> =>
        submitSessionMessage(muxUrl, sessionId, data);

      setExitInfo(null);
      setResumedFromSeq(null);
      // Fresh session: start from an empty bounded window. Activation loads
      // an HTTP tail first; the WS then only replays small race deltas.
      setClaudeEvents([]);
      setHasEarlierClaudeEvents(false);
      setLoadingEarlierClaudeEvents(false);

      let ptyExited = false;
      // A runner socket can disappear without the provider process ending.
      // Keep that distinction local as well as in daemon state: the composer
      // refuses sends while the runner is unreachable, and this hook retries
      // attachment until the daemon has reconnected the same durable session.
      let runnerUnavailable = false;
      let runnerReconnectTimer: number | null = null;
      let runnerReconnectAttempt = 0;
      // Microtask-batched buffer for incoming claudeEvent messages.
      // On a fresh attach the server replays history (potentially
      // thousands of events); coalescing into a single setState avoids
      // storming React.
      const pendingClaudeEvents: ClaudeSessionEvent[] = [];
      let claudeFlushScheduled = false;
      // In-memory only. Each fresh page mount starts xterm at seq 0 (empty
      // buffer) and asks the server to replay everything. Within this mount,
      // transient reconnects (phone-lock, network blip) reuse the live value
      // so we ask only for the deltas xterm hasn't seen. Deliberately NOT
      // persisted to localStorage: a previous tab's high-water-mark would
      // tell the server "client already has up to N" while xterm is
      // actually empty — causing the entire conversation to be skipped.
      let lastSeq = 0;
      // Running count of claudeEvents the client has ingested. The mux
      // manager reads it via getResume() on every (re)attach so the
      // server skips events we already have.
      let claudeEventsSeen = 0;
      // Absolute index of claudeEvents[0] in the daemon's event ring, plus
      // local length/total tracking for bounded prepend/append operations.
      let claudeEventsStart = 0;
      let claudeEventsLoadedLength = 0;
      let claudeEventsTotal = 0;
      let loadingEarlier = false;

      const normalizeStartIndex = (startIndex: number, eventCount: number, nextIndex: number): number => {
        return Number.isFinite(startIndex) ? startIndex : Math.max(0, nextIndex - eventCount);
      };

      const updateEarlierAvailability = (): void => {
        setHasEarlierClaudeEvents(claudeEventsStart > 0 && claudeEventsLoadedLength < CLAUDE_EVENT_HELD_CAP);
      };

      const replaceClaudeWindow = (
        events: ClaudeSessionEvent[],
        startIndex: number,
        totalCount: number,
        nextIndex: number
      ): void => {
        const overflow = Math.max(0, events.length - CLAUDE_EVENT_HELD_CAP);
        const kept = overflow > 0 ? events.slice(overflow) : events;
        claudeEventsStart = startIndex + overflow;
        claudeEventsLoadedLength = kept.length;
        claudeEventsTotal = totalCount;
        claudeEventsSeen = nextIndex;
        pendingClaudeEvents.length = 0;
        setClaudeEvents(kept);
        setHistoryPending(false);
        updateEarlierAvailability();
      };

      const loadClaudeTail = async (): Promise<boolean> => {
        try {
          const result = await fetchClaudeEvents(sessionId, { tail: CLAUDE_EVENT_TAIL });
          if (disposed || result === null || !isActiveRef.current) return false;
          const startIndex = normalizeStartIndex(result.startIndex, result.events.length, result.nextIndex);
          replaceClaudeWindow(result.events, startIndex, result.totalCount, result.nextIndex);
          return true;
        } catch {
          // Fall back to the WS initial replay cap if HTTP is transiently
          // unavailable. Keep the resume point at 0 so the server sends a tail,
          // not "nothing".
          claudeEventsSeen = 0;
          claudeEventsStart = 0;
          claudeEventsLoadedLength = 0;
          claudeEventsTotal = 0;
          setClaudeEvents([]);
          setHasEarlierClaudeEvents(false);
          return false;
        }
      };

      const loadEarlierClaudeEvents = async (): Promise<void> => {
        if (loadingEarlier || disposed || !isActiveRef.current) return;
        if (claudeEventsStart <= 0 || claudeEventsLoadedLength >= CLAUDE_EVENT_HELD_CAP) {
          updateEarlierAvailability();
          return;
        }
        const remainingCapacity = CLAUDE_EVENT_HELD_CAP - claudeEventsLoadedLength;
        if (remainingCapacity <= 0) {
          updateEarlierAvailability();
          return;
        }
        loadingEarlier = true;
        setLoadingEarlierClaudeEvents(true);
        try {
          const result = await fetchClaudeEvents(sessionId, {
            before: claudeEventsStart,
            tail: Math.min(CLAUDE_EVENT_PAGE, remainingCapacity)
          });
          if (disposed || result === null || !isActiveRef.current) return;
          if (result.events.length === 0) {
            setHasEarlierClaudeEvents(false);
            return;
          }
          let events = result.events;
          let startIndex = normalizeStartIndex(result.startIndex, result.events.length, result.nextIndex);
          if (events.length > remainingCapacity) {
            const dropped = events.length - remainingCapacity;
            events = events.slice(dropped);
            startIndex += dropped;
          }
          claudeEventsStart = startIndex;
          claudeEventsLoadedLength += events.length;
          claudeEventsTotal = Math.max(claudeEventsTotal, result.totalCount);
          setClaudeEvents((prev) => [...events, ...prev]);
          updateEarlierAvailability();
        } catch {
          // Leave the existing tail alone; the user can retry.
        } finally {
          loadingEarlier = false;
          setLoadingEarlierClaudeEvents(false);
        }
      };

      loadEarlierClaudeEventsRef.current = (): void => { void loadEarlierClaudeEvents(); };

      // Output coalescing: a busy TUI emits MANY small PTY frames per
      // keystroke (each ~1KB). Writing each one to xterm separately multiplies
      // VT-parser invocations and render scheduling. Buffer incoming frames
      // and flush them as ONE term.write per animation frame, so the parser
      // and renderer run once per painted frame instead of N times. lastSeq is
      // tracked per-message (above) so resume is unaffected by the batching.
      let pendingOutput: string[] = [];
      let outputRaf: number | null = null;
      let outputWriteInFlight = false;
      let alternateScreenRefreshTimer: number | null = null;
      const flushOutput = (): void => {
        outputRaf = null;
        if (!term || outputWriteInFlight || pendingOutput.length === 0) return;
        const data = pendingOutput.length === 1 ? pendingOutput[0]! : pendingOutput.join('');
        pendingOutput = [];
        outputWriteInFlight = true;
        term.write(data, () => {
          outputWriteInFlight = false;
          if (disposed || !term) return;
          // WKWebView's correctness-first DOM renderer needs the repaint only
          // after xterm has parsed the full frame. Calling refresh before the
          // write callback leaves stale row spans visible during Claude's
          // slash-command/settings alternate screen. Normal scrollback does
          // not pay this full-viewport repaint cost.
          if (
            repaintAlternateScreenAfterWrite
            && term.buffer.active.type === 'alternate'
            && term.rows > 0
          ) {
            // A full DOM-row refresh on every Claude echo frame made typing
            // visibly lag behind the keyboard. The refresh exists only as an
            // Apple-WebView artifact scrub, so debounce it to the end of the
            // current repaint burst; xterm's ordinary write still paints each
            // frame immediately.
            if (alternateScreenRefreshTimer !== null) {
              window.clearTimeout(alternateScreenRefreshTimer);
            }
            alternateScreenRefreshTimer = window.setTimeout(() => {
              alternateScreenRefreshTimer = null;
              if (!disposed && term && term.rows > 0 && term.buffer.active.type === 'alternate') {
                term.refresh(0, term.rows - 1);
              }
            }, 80);
          }
          if (pendingOutput.length > 0 && outputRaf === null) {
            outputRaf = requestAnimationFrame(flushOutput);
          }
        });
      };

      // Sizing strategy: FitAddon measures cell metrics + container dims
      // and computes cols/rows that fill the visible terminal pane. We
      // then forward the new size to sessionsd so the PTY (and the TUI
      // inside it) redraws at the actual viewport size. Last-resize-wins
      // across clients. Debounced: ResizeObserver can fire many times
      // during a window-drag; the PTY receives one resize 60ms after the
      // last container change.
      let resizeSendTimer: number | null = null;
      const sendResize = (cols: number, rows: number): void => {
        channel?.sendResize(cols, rows);
      };
      const applyFit = (): void => {
        if (!term || !fit) return;
        // Only the viewed session fits/resizes. A window-resize fires the
        // listener on EVERY mounted session; without this gate, all ~36
        // run fit.fit() — each a synchronous layout measurement — dozens of
        // times per drag (the resize was ~5s instead of ~500ms). Hidden
        // panes are display:none so fit would throw on them anyway, but the
        // measurement attempt is the cost. Fit-on-activate (the isActive
        // effect) sizes a session the moment it becomes visible.
        if (!isActiveRef.current) return;
        try {
          fit.fit();
        } catch {
          // Fit can throw if the container has zero dims (eg the pane is
          // display:none on an inactive tab). Ignore — when the pane is
          // visible again, the ResizeObserver fires and we try again.
          return;
        }
        // A renderer that was hidden, resized, or moved between native panes
        // may still have a valid buffer but stale pixels. Force one bounded
        // repaint after the new cell geometry is committed.
        if (term.rows > 0) term.refresh(0, term.rows - 1);
        if (resizeSendTimer !== null) window.clearTimeout(resizeSendTimer);
        resizeSendTimer = window.setTimeout(() => {
          resizeSendTimer = null;
          if (term) sendResize(term.cols, term.rows);
        }, 60);
      };

      const onResize = (): void => {
        requestAnimationFrame(applyFit);
      };
      fitTerminalRef.current = applyFit;

      const clearRunnerReconnect = (): void => {
        if (runnerReconnectTimer !== null) {
          window.clearTimeout(runnerReconnectTimer);
          runnerReconnectTimer = null;
        }
        runnerReconnectAttempt = 0;
      };

      const scheduleRunnerReconnect = (): void => {
        if (disposed || ptyExited || !runnerUnavailable || runnerReconnectTimer !== null) return;
        const delays = [1200, 3000, 8000, 10_000] as const;
        const delay = delays[Math.min(runnerReconnectAttempt, delays.length - 1)]!;
        runnerReconnectAttempt += 1;
        runnerReconnectTimer = window.setTimeout(() => {
          runnerReconnectTimer = null;
          if (disposed || ptyExited || !runnerUnavailable) return;
          void attachNow(isActiveRef.current).finally(() => {
            if (runnerUnavailable) scheduleRunnerReconnect();
          });
        }, delay);
      };

      const onMessage = (msg: ServerMsg): void => {
        if (disposed) return;
        if (msg.type === 'hello') {
          if (msg.session.unreachable) {
            runnerUnavailable = true;
            setStatus('reconnecting');
            scheduleRunnerReconnect();
          } else {
            runnerUnavailable = false;
            clearRunnerReconnect();
            sendInputRef.current = (data: string): void => { channel?.sendInput(data); };
            sendConfirmedInputRef.current = (data: string): Promise<void> =>
              sendSessionInput(muxUrl, sessionId, data);
            submitMessageRef.current = (data: string): Promise<void> =>
              submitSessionMessage(muxUrl, sessionId, data);
            setStatus('open');
          }
          setResumedFromSeq(msg.resumedFromSeq);
          // Initialize our local claudeEvents counter to the server's
          // starting index — the server caps initial replay to the
          // tail for long sessions, so we'd otherwise believe we'd
          // received 5000 events when we received 300, and the next
          // reconnect would skip 4700 events the server still wants
          // to push.
          const replayStart = msg.claudeReplayStart ?? claudeEventsSeen;
          if (claudeEventsLoadedLength === 0 && pendingClaudeEvents.length === 0) {
            claudeEventsStart = replayStart;
            updateEarlierAvailability();
          }
          claudeEventsSeen = replayStart;
          if (msg.claudeEventsCount === 0) setHistoryPending(false);
          if (term) {
            // First chance to fit the terminal to the visible container.
            requestAnimationFrame(() => {
              if (!term || !fit) return;
              try {
                fit.fit();
              } catch {
                if (msg.session.cols !== term.cols || msg.session.rows !== term.rows) {
                  try { term.resize(msg.session.cols, msg.session.rows); } catch { /* ignore */ }
                }
                return;
              }
              sendResize(term.cols, term.rows);
            });
          }
          return;
        }
        if (msg.type === 'output') {
          lastSeq = msg.seq;
          if (term) {
            pendingOutput.push(msg.data);
            if (outputRaf === null) outputRaf = requestAnimationFrame(flushOutput);
          }
          return;
        }
        if (msg.type === 'gap') {
          // Resync wipes the screen — any buffered-but-unwritten frames are
          // now stale; drop them and cancel the pending flush.
          pendingOutput = [];
          if (outputRaf !== null) { cancelAnimationFrame(outputRaf); outputRaf = null; }
          if (term) {
            term.reset();
            term.writeln(
              `\x1b[2m[reconnect: missed ${msg.oldestAvailableSeq - 1 - lastSeq} chunks; ` +
              `resyncing from seq ${msg.oldestAvailableSeq}]\x1b[0m`
            );
          }
          lastSeq = msg.oldestAvailableSeq - 1;
          return;
        }
        if (msg.type === 'exit') {
          // Flush any buffered output so the final bytes show before the
          // exit banner, then stop the coalescer.
          flushOutput();
          if (outputRaf !== null) { cancelAnimationFrame(outputRaf); outputRaf = null; }
          ptyExited = true;
          clearRunnerReconnect();
          setExitInfo({ code: msg.code, signal: msg.signal });
          setStatus('closed');
          term?.writeln(
            `\r\n\x1b[2m[session exited code=${msg.code ?? '∅'} signal=${msg.signal ?? '∅'}]\x1b[0m`
          );
          // The server auto-detaches an exited session from the mux
          // socket; detach locally too so the manager forgets us (and
          // can close the socket when nothing else is attached).
          channel?.detach();
          return;
        }
        if (msg.type === 'unreachable') {
          const firstNotice = !runnerUnavailable;
          runnerUnavailable = true;
          setStatus('reconnecting');
          sendInputRef.current = (): void => {};
          sendConfirmedInputRef.current = (): Promise<void> =>
            Promise.reject(new Error('Sessions is reconnecting to this runner. Your message was not sent.'));
          submitMessageRef.current = (): Promise<void> =>
            Promise.reject(new Error('Sessions is reconnecting to this runner. Your message was not sent.'));
          if (firstNotice) {
            term?.writeln('\r\n\x1b[2m[runner connection lost; Sessions is reconnecting]\x1b[0m');
          }
          scheduleRunnerReconnect();
          return;
        }
        if (msg.type === 'error') {
          term?.writeln(`\r\n\x1b[31m[error] ${msg.message}\x1b[0m`);
          // "unknown session <id>" means this session id doesn't exist
          // server-side anymore — typically a stale pop-out window
          // pointing at a session that's since been killed/recreated.
          // Mark terminally dead so the user sees ONE error message
          // instead of a reconnect-spam wave.
          if (/unknown session/i.test(msg.message)) {
            ptyExited = true;
            setStatus('closed');
            setExitInfo({ code: null, signal: 'unknown-session' });
            channel?.detach();
          }
          return;
        }
        if (msg.type === 'claudeEvent') {
          // Only the viewed session folds events into React state. Hidden
          // sessions ask the daemon not to send live claudeEvent frames; this
          // guard is the fallback for an older daemon or an in-flight frame
          // during reattach.
          if (!isActiveRef.current) return;
          // Batch appends via a microtask so a replay delta does not schedule
          // one React render per event.
          pendingClaudeEvents.push(msg.event);
          claudeEventsSeen += 1;
          if (!claudeFlushScheduled) {
            claudeFlushScheduled = true;
            queueMicrotask(() => {
              claudeFlushScheduled = false;
              const batch = pendingClaudeEvents.slice();
              pendingClaudeEvents.length = 0;
              const overflow = Math.max(0, claudeEventsLoadedLength + batch.length - CLAUDE_EVENT_HELD_CAP);
              claudeEventsLoadedLength = claudeEventsLoadedLength + batch.length - overflow;
              claudeEventsStart += overflow;
              claudeEventsTotal = Math.max(claudeEventsTotal, claudeEventsSeen);
              setClaudeEvents((prev) => {
                const merged = [...prev, ...batch];
                return overflow > 0 ? merged.slice(overflow) : merged;
              });
              setHistoryPending(false);
              updateEarlierAvailability();
            });
          }
          return;
        }
      };

      const onStatus = (s: MuxStatus): void => {
        // Once this session has terminally ended (exit / unknown
        // session) the shared socket's state no longer describes us.
        if (disposed || ptyExited) return;
        setStatus(s === 'open' && runnerUnavailable ? 'reconnecting' : s);
        if (s === 'open') term?.focus();
      };

      if (term) {
        dataDisp = term.onData((d) => {
          channel?.sendInput(d);
        });
        // macOS line-editing: xterm doesn't map Cmd+Backspace, so ⌘⌫
        // silently does nothing in the terminal (it works in tmux/iTerm only
        // because the native terminal maps it). Send Ctrl+U — the universal
        // "delete to start of line" — so ⌘⌫ wipes the current input line like
        // it does everywhere else on macOS. Return false so xterm doesn't
        // also process the key; everything else falls through untouched.
        term.attachCustomKeyEventHandler((e) => {
          if (e.type === 'keydown' && e.metaKey && !e.ctrlKey && !e.altKey && e.key === 'Backspace') {
            channel?.sendInput('\x15');
            return false;
          }
          return true;
        });
        window.addEventListener('resize', onResize);
        ro = new ResizeObserver(onResize);
        const c = containerElRef.current;
        if (c) ro.observe(c);
      }

      const getResume = (): {
        lastSeq: number;
        claudeEventsSince: number;
        outputReplay: boolean;
        claudeReplay: boolean;
        claudeLive: boolean;
      } => ({
        lastSeq,
        claudeEventsSince: claudeEventsSeen,
        // Raw PTY bytes (replay AND live) only for the session the user is
        // actually viewing. Without the isActive gate, every mounted
        // terminal replays its output ring on page load / reconnect — at
        // 36 sessions that backlog drains slowly over Tailscale and the
        // keystroke echo queues behind it (the "typing is 5s" symptom).
        // No terminal mounted (Sessions-only) → no bytes at all. A hidden
        // session catches up via snapshot-prefill when it's reactivated.
        outputReplay: term !== null && isActiveRef.current,
        // Likewise only the viewed session replays Claude history and live
        // Claude event frames. Hidden sessions are refreshed from a bounded
        // HTTP tail when activated.
        claudeReplay: isActiveRef.current,
        claudeLive: isActiveRef.current
      });

      // Snapshot-prefill: instead of attaching at seq=0 and watching xterm
      // fill top-to-bottom as every frame replays one at a time, fetch the
      // server-side serialized snapshot in ONE HTTP request and write it in
      // a single bulk call, then attach with lastSeq=<snapshot's seq> so we
      // only receive deltas. Visually: the buffer is just THERE on first
      // paint. Reused on reactivation (a hidden terminal had output
      // suppressed, so it's stale — reset + reprime from the snapshot).
      const prefillTerminalSnapshot = async (): Promise<void> => {
        if (!term) return;
        await new Promise<void>((resolve) => {
          requestAnimationFrame(() => {
            applyFit();
            resolve();
          });
        });
        try {
          // Terminal priming asks for the mirror's bounded scrollback as one
          // bulk stream. The old viewport-only snapshot made the terminal
          // look current but left nothing above it to scroll into; replaying
          // every raw event would reintroduce the multi-second attach cost.
          const snap = await fetchServerSnapshot(sessionId, undefined, true);
          if (disposed || !isActiveRef.current) return;
          if (snap && snap.seq > 0) {
            term.reset();
            term.write(snap.text, () => {
              try { term?.scrollToBottom(); } catch { /* ignore */ }
            });
            lastSeq = snap.seq;
          }
        } catch { /* fall through to plain attach — full replay */ }
      };

      // Generation counter, same pattern SettingsView uses for its async
      // loads. attachNow awaits the snapshot prefill and the HTTP tail between
      // clearing `channel` and reassigning it. Rapid tab switching (or a
      // reattach landing on top of the mount attach) could therefore run two
      // attachNow calls through that window at once: both saw `channel` as
      // null, both called attachSession, and the loser's socket was
      // overwritten by the winner's assignment with nothing left holding a
      // reference to detach it — a leaked mux subscription still receiving
      // this session's bytes for the life of the mount.
      //
      // Every attach now takes a generation. Only the newest one is allowed to
      // publish its channel; an older one that finishes late detaches whatever
      // it opened and leaves the current attach alone.
      let attachGeneration = 0;

      const attachNow = async (active: boolean): Promise<void> => {
        if (disposed) return;
        const generation = ++attachGeneration;
        if (active) setHistoryPending(true);
        channel?.detach();
        sendInputRef.current = (): void => {};
        sendConfirmedInputRef.current = (): Promise<void> =>
          Promise.reject(new Error('Sessions is reconnecting. Your message was not sent.'));
        submitMessageRef.current = (): Promise<void> =>
          Promise.reject(new Error('Sessions is reconnecting. Your message was not sent.'));
        channel = null;
        const tailPromise = active ? loadClaudeTail() : Promise.resolve(false);
        if (active && term) await prefillTerminalSnapshot();
        if (active) await tailPromise;
        // Stale generation: a newer attachNow started while this one was
        // awaiting and now owns `channel`. Returning here is what keeps the
        // winner's socket intact instead of overwriting it.
        if (disposed || generation !== attachGeneration) return;
        channel = attachSession(muxUrl, sessionId, { onMessage, onStatus, getResume });
      };
      // Re-subscribe with flags recomputed for current activeness. Becoming
      // active → prefill (the terminal was frozen while hidden) + the
      // server starts streaming output again; going inactive → re-attach
      // output-less so the server stops sending this session's bytes.
      reattachRef.current = (active: boolean): void => { void attachNow(active); };
      void attachNow(isActiveRef.current);

      // Wire up the cleanup closure now that all locals exist. The
      // outer-effect cleanup invokes this if it's set.
      runCleanup = () => {
        window.removeEventListener('resize', onResize);
        ro?.disconnect();
        if (resizeSendTimer !== null) window.clearTimeout(resizeSendTimer);
        if (alternateScreenRefreshTimer !== null) window.clearTimeout(alternateScreenRefreshTimer);
        clearRunnerReconnect();
        if (outputRaf !== null) { cancelAnimationFrame(outputRaf); outputRaf = null; }
        dataDisp?.dispose();
        scrollDisp?.dispose();
        channel?.detach();
        loadEarlierClaudeEventsRef.current = (): void => {};
        term?.dispose();
      };
    })();

    return () => {
      disposed = true;
      if (runCleanup) runCleanup();
    };
  }, [sessionId, muxUrl, mountTerminal]);

  // Activeness changes are NOT a mount-effect dependency (that would
  // dispose + rebuild xterm on every tab switch). Instead, re-subscribe
  // the mux stream with output gated to the active session, and fit the
  // terminal the moment it becomes visible. Skip the initial run — the
  // mount effect already did the first attach.
  const activeFirstRunRef = useRef(true);
  useEffect(() => {
    if (activeFirstRunRef.current) { activeFirstRunRef.current = false; return; }
    reattachRef.current(isActive);
    if (isActive) {
      const id = requestAnimationFrame(() => fitTerminalRef.current());
      return () => cancelAnimationFrame(id);
    }
    return undefined;
  }, [isActive]);

  const containerRef = useCallback((el: HTMLDivElement | null) => {
    containerElRef.current = el;
  }, []);

  return {
    containerRef,
    status,
    exitInfo,
    resumedFromSeq,
    sendInputRef,
    sendConfirmedInputRef,
    submitMessageRef,
    terminalAtBottom,
    scrollTerminalToBottomRef,
    focusTerminalRef,
    fitTerminalRef,
    claudeEvents,
    historyPending,
    hasEarlierClaudeEvents,
    loadingEarlierClaudeEvents,
    loadEarlierClaudeEventsRef
  };
}
