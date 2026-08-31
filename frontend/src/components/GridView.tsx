import { useEffect, useMemo, useRef, useState, type KeyboardEvent, type ClipboardEvent } from 'react';
import type { SessionInfo } from '../types';
import type { TabStatus } from './SessionTabs';
import { wsMuxUrl } from '../api/sessionsd';
import { requestClaudeEvents, sendSessionInput } from '../lib/wsMux';
import { useServers } from '../lib/servers';
import { openSessionWindow } from '../lib/tauriBridge';
import { keyToBytes } from '../lib/keyToBytes';
import { ParserIcon } from './ParserIcon';
import { renderContent } from '../lib/contentRender';
import { eventsToMessages } from '../lib/claudeEvents';
import { resolvedSessionLabel } from '../lib/tabLabels';
import type { DispatchMessage } from '../hooks/useDispatch';
import { CopyButton } from './CopyButton';
import { classifySession } from '../lib/sessionStatus';

// Tile every session in a column-flow grid. Each cell renders a
// chat-style preview (the most recent user message + Claude reply
// bubbles) — same shape as Remote view, just compact. Click on the
// cell to focus it; keystrokes from the cell forward straight to the
// session's PTY so you can dispatch quick replies from any cell.

interface Props {
  sessions: SessionInfo[];
  statusBySession: Record<string, TabStatus>;
  iconBySession: Record<string, string>;
  // Explicitly end the runtime. This is intentionally not called "close":
  // closing a tab/window is presentation-only and must never kill work.
  onEnd?: (id: string) => void | Promise<void>;
  // Expand to single-session (tabs) view. Wired to the ⤢ button AND
  // double-click on the cell. Single click only focuses for typing —
  // the user monitors + types from grid without flipping modes.
  onExpand?: (id: string) => void;
}

// Note: derivedLabel is removed — sessionLabel from lib/tabLabels.ts
// is the single canonical resolution chain shared across all consumers.

// "5s", "3m", "2h ago" — short relative-time label for the cell header
// so the user can see at a glance how stale a session's last activity
// is. Anything older than a day rolls up to "Xd".
function relativeTime(ts: number): string {
  const diff = Math.max(0, Date.now() - ts);
  if (diff < 60_000) return `${Math.floor(diff / 1000)}s`;
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)}m`;
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)}h`;
  return `${Math.floor(diff / 86_400_000)}d`;
}

// Auto-fit column layout: cells flow into 320-px minimum columns. On a
// phone you get 1 column; on a 1080-px laptop you get 3; on a 4k
// monitor you get more. No more fixed N×M; cells flow into whatever
// columns fit the viewport.
export function GridView({ sessions, statusBySession, iconBySession, onEnd, onExpand }: Props): JSX.Element {
  return (
    <div className="grid-view">
      {sessions.map((s) => {
        const label = resolvedSessionLabel(s);
        return (
          <GridCell
            key={s.id}
            session={s}
            status={statusBySession[s.id] ?? 'idle'}
            icon={iconBySession[s.id] ?? '⬛'}
            onPopOut={() => openSessionWindow(s.id, label)}
            onExpand={onExpand ? () => onExpand(s.id) : undefined}
            onEnd={onEnd ? () => onEnd(s.id) : undefined}
          />

        );
      })}
    </div>
  );
}

interface CellProps {
  session: SessionInfo;
  status: TabStatus;
  icon: string;
  onPopOut: () => void;
  onExpand?: () => void;
  onEnd?: () => void | Promise<void>;
}

function GridCell({ session, status, icon, onPopOut, onExpand, onEnd }: CellProps): JSX.Element {
  const [messages, setMessages] = useState<DispatchMessage[]>([]);
  const [focused, setFocused] = useState(false);
  // Tick the clock every 30s so the relative-time label in the header
  // ("3m ago") stays current without forcing a refetch.
  const [, setNow] = useState(Date.now());
  // Two-step confirmation before explicitly ending a live runtime.
  const [endConfirm, setEndConfirm] = useState(false);
  // store/sessions.ts `kill` throws on a non-2xx daemon reply. This used to be
  // `void onEnd()`: the popover closed, the tile stayed exactly as it was, and
  // the rejection went nowhere — the user was told a session had ended that
  // had not. docs/PRINCIPLES.md: "cleanup must never hide an unresolved
  // decision". The confirmation now stays open, and says what went wrong.
  const [ending, setEnding] = useState(false);
  const [endError, setEndError] = useState<string | null>(null);
  // Flash a red border and "send failed" hint when a keystroke POST fails.
  // Cleared automatically after 1.5s so the cell returns to normal.
  const [sendFailed, setSendFailed] = useState(false);
  const cellRef = useRef<HTMLDivElement | null>(null);
  const bodyRef = useRef<HTMLDivElement | null>(null);
  // Auto-stick to bottom only when the user is parked at the bottom.
  // Was unconditional → every 2s poll would yank scroll to bottom,
  // making it impossible to read older messages.
  const stickRef = useRef(true);
  const activeServerId = useServers((s) => s.activeId);
  // Last event count we rendered. The /events endpoint returns nextIndex
  // (absolute total); when it's unchanged since the previous 2s poll there
  // are no new events, so we skip eventsToMessages + setMessages + the
  // markdown re-render entirely. With N idle cells tiled this turns a
  // steady N×(parse+render) every 2s into ~zero React work.
  const lastNextIndexRef = useRef(-1);

  // Refresh the cell's chat preview every 2s from Claude's JSONL
  // event log (cached server-side by the file watcher). Same data
  // source RemoteView consumes via WS — just polled here because
  // grid cells are glance-only and don't justify per-cell WS
  // connections. Event-derived messages drop every parsing hazard
  // that haunted the previous snapshot-scrape implementation
  // ("Wraysbury misparsed as Bash", picker items leaking, etc.).
  useEffect(() => {
    let alive = true;
    lastNextIndexRef.current = -1; // force a render on (re)subscribe
    const tick = async (): Promise<void> => {
      try {
        // Only fetch the tail — a single cell shows max 10 messages
        // (~20 events on average since assistant turns split into
        // tool_use + reply events). Used to pull the full ~15-20 MB
        // ring buffer every 2s per cell, which was the #1 phone perf
        // sink. tail=40 covers any reasonable rendered window.
        const result = await requestClaudeEvents(wsMuxUrl(), session.id, { tail: 40 });
        if (!alive || result === null) return;
        // Nothing new since last poll → skip the parse + re-render.
        if (result.nextIndex === lastNextIndexRef.current) return;
        lastNextIndexRef.current = result.nextIndex;
        const msgs = eventsToMessages(result.events);
        setMessages(msgs.slice(-10));
      } catch { /* transient — try again next tick */ }
    };
    void tick();
    const id = window.setInterval(() => { void tick(); }, 2000);
    const tickNow = window.setInterval(() => setNow(Date.now()), 30_000);
    return () => {
      alive = false;
      window.clearInterval(id);
      window.clearInterval(tickNow);
    };
  }, [session.id, activeServerId]);

  // Bottom-anchor when new messages land AND the user is at the
  // bottom. If they scrolled up to read history, leave them alone —
  // the 2s poll was previously yanking scroll back to bottom on every
  // tick, making it impossible to read older content.
  useEffect(() => {
    if (!stickRef.current) return;
    const el = bodyRef.current;
    if (!el) return;
    el.scrollTop = el.scrollHeight;
  }, [messages]);

  const onBodyScroll = (): void => {
    const el = bodyRef.current;
    if (!el) return;
    stickRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
  };

  // Auto-clear the send-failure indicator after 1.5s. Re-runs each time
  // sendFailed transitions from false→true (a new failure resets the timer).
  useEffect(() => {
    if (!sendFailed) return;
    const id = window.setTimeout(() => setSendFailed(false), 1500);
    return () => window.clearTimeout(id);
  }, [sendFailed]);

  const confirmEnd = async (): Promise<void> => {
    if (!onEnd || ending) return;
    setEnding(true);
    setEndError(null);
    try {
      await onEnd();
      setEndConfirm(false);
    } catch (reason) {
      setEndError(reason instanceof Error ? reason.message : 'Sessions could not end this session.');
    } finally {
      setEnding(false);
    }
  };

  const cwd = useMemo(() => session.cwd ?? '', [session.cwd]);
  const label = resolvedSessionLabel(session);

  // Local typing buffer — shown as a floating popup over the cell while
  // focused so the user can see what they're typing without resizing
  // the cell. The actual bytes still get forwarded to the PTY on each
  // keystroke; this is purely visual feedback. Cleared on Enter (sent),
  // Escape (cancel), and on blur (focus left the cell).
  const [typedBuffer, setTypedBuffer] = useState('');

  // Flag a failed send: clear the typed buffer (bytes didn't land) and
  // flash the cell border red so the user knows the keystroke was lost.
  // Declared after typedBuffer so setTypedBuffer is in scope.
  const flagSendFailed = (): void => {
    setTypedBuffer('');
    setSendFailed(true);
  };

  // Direct keystroke forwarding. Each keystroke is translated to its
  // PTY byte sequence and POSTed via the input endpoint. The 2s
  // snapshot poll picks up the echo and re-renders the cell so the
  // user eventually sees their typing land in Claude's prompt — but
  // the typedBuffer popup gives instant feedback in the meantime.
  // On POST failure: clear the local buffer and flash the red border so
  // the user knows the keystroke was lost (not silently swallowed).
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>): void => {
    const bytes = keyToBytes(e);
    if (bytes === null) return;
    e.preventDefault();
    sendSessionInput(wsMuxUrl(), session.id, bytes).catch(flagSendFailed);

    // Maintain the local visual buffer alongside.
    const k = e.key;
    if (e.metaKey || e.ctrlKey || e.altKey) {
      // Modifier combos (Cmd-Backspace = kill, Ctrl-C = SIGINT, etc.)
      // usually move or clear the input box. Safest to clear here.
      setTypedBuffer('');
    } else if (k === 'Backspace') {
      setTypedBuffer((s) => s.slice(0, -1));
    } else if (k === 'Enter') {
      // Sent — clear the popup. Snapshot poll will surface the
      // message as a user_input bubble in the cell shortly.
      setTypedBuffer('');
    } else if (k === 'Escape') {
      setTypedBuffer('');
    } else if (k.length === 1) {
      setTypedBuffer((s) => s + k);
    } else {
      // Tab, arrows, F-keys etc. — cursor probably moved.
      setTypedBuffer('');
    }
  };

  // Paste handler — mirrors Claude Code's TUI convention. Sends the
  // bracketed-paste sequence to the PTY (so Claude treats it as a
  // single paste, not a keystroke storm) and updates the local
  // buffer with either the literal text or a "[Pasted +N lines]"
  // marker for multi-line content. Single-line short pastes appear
  // inline (the user pasted a URL); long or multi-line pastes get
  // the placeholder so the popup doesn't overflow the cell.
  const onPaste = (e: ClipboardEvent<HTMLDivElement>): void => {
    const text = e.clipboardData?.getData('text');
    if (!text) return;
    e.preventDefault();
    // Bracketed paste, no trailing Enter — user submits with Return
    // separately. Same protocol as InputBar's paste path.
    sendSessionInput(wsMuxUrl(), session.id, `\x1b[200~${text}\x1b[201~`).catch(flagSendFailed);
    // Local-buffer display marker.
    const newlines = (text.match(/\n/g) ?? []).length;
    let marker: string;
    if (newlines > 0) {
      marker = `[Pasted text +${newlines + 1} lines]`;
    } else if (text.length > 80) {
      marker = `[Pasted text ${text.length} chars]`;
    } else {
      marker = text;
    }
    setTypedBuffer((s) => s + marker);
  };

  const focusCell = (): void => {
    cellRef.current?.focus();
  };

  // The typedBuffer used to clear on blur (when the user clicked away
  // from this cell). That dropped the user's draft — they came back
  // and saw nothing. Now we KEEP the buffer across blur so switching
  // between cells preserves each cell's in-progress text. The popup
  // hides while unfocused (it only renders when `focused`) but the
  // state lives. Cleared explicitly on Enter / Escape / modifier
  // combos via the onKeyDown handler.

  // Status text — "Working · 5s", "Ready · 3m". The state word comes from the
  // one classifier (lib/sessionStatus.ts) so a cell never disagrees with the
  // navigator, Fleet, or Home about the same session; the parser/sidebar
  // working signal lifted up via statusBySession is passed in as the live
  // activity override. Time ago is computed off session.lastDataAt, ticked
  // every 30s.
  const sessionStatus = classifySession(session, { working: status === 'working' });
  const statusText = session.exited
    ? `${sessionStatus.label}${session.exitCode != null ? ` · ${session.exitCode}` : ''}`
    : `${sessionStatus.label} · ${relativeTime(session.lastDataAt)}`;

  return (
    <div
      className={`grid-cell${focused ? ' is-focused' : ''}${status === 'working' && !session.exited ? ' is-working' : ''}${session.exited ? ' is-exited' : ''}${sendFailed ? ' is-send-failed' : ''}`}
      ref={cellRef}
      tabIndex={0}
      onClick={focusCell}
      onDoubleClick={onExpand}
      onFocus={() => setFocused(true)}
      onBlur={() => setFocused(false)}
      onKeyDown={onKeyDown}
      onPaste={onPaste}
    >
      <div className="grid-cell-head">
        <span className="grid-cell-icon" aria-hidden><ParserIcon icon={icon} size={18} /></span>
        <span className="grid-cell-name">{label}</span>
        <span className={`grid-cell-status${status === 'working' ? ' is-working' : ''}${session.exited ? ' is-exited' : ''}`}>
          {status === 'working' ? <span className="grid-cell-dot" aria-hidden /> : null}
          {statusText}
        </span>
        {onExpand ? (
          <button
            type="button"
            className="grid-cell-expand"
            onClick={(e) => { e.stopPropagation(); onExpand(); }}
            title="Expand to single-session view"
          >⤢</button>
        ) : null}
        <button
          type="button"
          className="grid-cell-popout"
          onClick={(e) => { e.stopPropagation(); onPopOut(); }}
          title="Pop out into its own window"
        >↗</button>
        {onEnd ? (
          endConfirm ? (
            <>
              <span className="grid-cell-close-label" role={endError ? 'alert' : undefined}>
                {endError ?? (ending ? 'Ending…' : 'End runtime?')}
              </span>
              <button
                type="button"
                className="grid-cell-close-yes"
                aria-label={endError ? 'Try ending this session again' : 'Confirm end session'}
                disabled={ending}
                onClick={(e) => { e.stopPropagation(); void confirmEnd(); }}
              >{endError ? 'Retry' : 'End'}</button>
              <button
                type="button"
                className="grid-cell-close-no"
                aria-label="Cancel ending session"
                disabled={ending}
                onClick={(e) => { e.stopPropagation(); setEndConfirm(false); setEndError(null); }}
              >Cancel</button>
            </>
          ) : (
            <button
              type="button"
              className="grid-cell-close"
              onClick={(e) => {
                e.stopPropagation();
                setEndError(null);
                setEndConfirm(true);
              }}
              title="End the running process. Closing a tab does not."
              aria-label="End session"
            >■</button>
          )
        ) : null}
      </div>
      <div ref={bodyRef} className="grid-cell-body grid-cell-body-chat" onScroll={onBodyScroll}>
        {messages.length === 0 ? (
          <div className="grid-cell-empty">(no recent messages)</div>
        ) : (
          messages.map((m) => (
            <div
              key={m.id}
              className={`grid-cell-msg grid-cell-msg-${m.role}`}
            >
              {m.role === 'user' ? (
                <div className="grid-cell-bubble grid-cell-bubble-user">{m.content}</div>
              ) : (
                <div
                  className="grid-cell-bubble grid-cell-bubble-assistant md-content"
                  dangerouslySetInnerHTML={{ __html: renderContent(m.content, cwd) }}
                />
              )}
              {m.content ? <CopyButton getText={m.content} iconOnly className="grid-cell-copy" label="Copy message" /> : null}
            </div>
          ))
        )}
      </div>
      {/* Typing popup — floats over the cell while focused. Visible
          whenever the cell has focus (not just after the first
          keypress) so the user immediately sees "yes, my keystrokes
          will land here." The caret is rendered regardless; the
          typed text appears alongside as it accumulates. */}
      {focused ? (
        <div className="grid-cell-typing-popup" role="status" aria-live="polite">
          {sendFailed ? (
            // Send-failed hint: the typed byte was not delivered. Buffer is
            // already cleared so the user doesn't retry stale text.
            <span className="grid-cell-typing-failed">send failed — tap to retry</span>
          ) : typedBuffer ? (
            <span className="grid-cell-typing-text">{typedBuffer}</span>
          ) : (
            <span className="grid-cell-typing-placeholder">type to send to {label}</span>
          )}
          <span className="grid-cell-typing-caret" aria-hidden>▮</span>
        </div>
      ) : null}
      {/* Footer strip that doubles as focus indicator. When the cell
          isn't focused it reads "Click to type". When focused, a
          blinking caret glyph + "typing →" makes it obvious that
          keystrokes will land in this session. */}
      <div className="grid-cell-foot">
        {focused ? (
          <>
            <span className="grid-cell-caret" aria-hidden>▮</span>
            <span>typing → {label}</span>
          </>
        ) : (
          <span className="grid-cell-foot-hint">Click to type</span>
        )}
      </div>
    </div>
  );
}
