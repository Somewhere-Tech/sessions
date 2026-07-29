import { useCallback, useEffect, useRef, useState } from 'react';
import { useServers } from '../lib/servers';

// Minimal Block shape — retained only to keep the legacy parser-based
// reconciler path in this hook compiling. The whole parser pipeline
// (lib/parser.ts, parsers/, hooks/usePrettyParser.ts) is gone; nobody
// passes `blocks` anymore. The reconciler effect below short-circuits
// to a no-op when blocks is empty/undefined, and the JSONL-event path
// (eventUserContentCounts + per-message confirmBaseline) handles all live
// pending → sent flips.
//
// Kept rather than ripped because removing it touches ~200 lines of
// already-tested reconciler logic — leaving it dead is lower risk.
interface Block {
  id: string;
  type: string;
  content: string;
  metadata?: Record<string, unknown>;
}

// Remote view's message log. Each entry is OUR record of an exchange,
// not a re-derivation of the parser's blocks. The parser is just the
// observable we use to detect state transitions:
//   • User message landed in the buffer ⇒ flip from 'pending' to 'sent'
//   • A claude_message block we haven't recorded yet ⇒ append as assistant
//
// Once a message is in the log, it never moves and never re-renders
// from a different source — that's what stops the bouncing the user
// complained about. The buffer can lose, redraw, scroll-past, or
// truncate; our log is durable.

// Tool invocation Claude made during an assistant turn. We surface a
// chip with the tool name + a short input summary so the user can see
// "what Claude did" without expanding the full tool output. Sourced
// from `tool_use` content blocks inside an assistant event.
//
// Multi-step assistant turns (Claude calls 5 tools then replies)
// arrive as a sequence of assistant events in the JSONL: each tool
// is its own event with just thinking+tool_use, then a final event
// with the text reply. We collapse all those into one logical
// message and ship every ToolCall as part of the message's
// `toolCalls`. The matching tool_result events (user role,
// content[].type=='tool_result') are indexed by tool_use_id and
// stored on `resultPreview` + `resultFull` so the UI can show the
// full output on click without an extra fetch.
export interface ToolCall {
  id: string;       // Anthropic-issued tool use id (toolu_…)
  name: string;     // tool name: Read, Bash, Edit, Glob, Grep, Task, …
  inputPreview: string;  // short, single-line summary of the input
  // Full raw input + result, for the expanded view.
  inputFull?: string;
  resultPreview?: string;  // first ~120 chars of the tool's output
  resultFull?: string;     // full output for the expandable panel
  // Provider-neutral lifecycle details used by Codex app-server activity.
  kind?: string;
  status?: string;
  durationMs?: number;
}

export interface MessagePlanStep {
  step: string;
  status: string;
}

export interface DispatchMessage {
  id: string;
  role: 'user' | 'assistant';
  content: string;
  status: 'pending' | 'sent' | 'failed';
  createdAt: number;
  author?: import('../types').MessageAuthor;
  // For user messages: when the parser confirmed the bytes landed in
  // Claude's scrollback. Useful for "sent · 200ms ago" debug pills.
  confirmedAt?: number;
  // The parser block.id we sourced (or confirmed) this entry from.
  // For assistant messages: dedupes Claude's streaming re-emits of the
  // same turn. For user messages: prevents a previously-sent message
  // with identical content from false-confirming a brand-new pending
  // (each block can only confirm ONE pending).
  blockId?: string;
  // For user messages: error response captured by the parser (e.g.,
  // "API Error: Server is temporarily limiting requests…"). Surfaces
  // the failure state in the chat — message was delivered to Claude
  // but Claude couldn't process it.
  errorResponse?: string;
  // For assistant messages: tools Claude called during this turn
  // (sourced from `tool_use` content blocks). Rendered as chips
  // under or alongside the message body.
  toolCalls?: ToolCall[];
  // For assistant messages: presence of a `thinking` content block.
  // We don't display the raw reasoning (often signed/encrypted in
  // recent Claude versions anyway) but a "💭 thought for …" badge
  // tells the user Claude reasoned before answering.
  hadThinking?: boolean;
  // Codex app-server exposes safe reasoning summaries and commentary as
  // first-class UI material. They stay separate from the final answer so a
  // streaming progress update never overwrites the durable response.
  reasoningSummary?: string;
  updates?: string[];
  plan?: MessagePlanStep[];
  planExplanation?: string;
  streaming?: boolean;
  turnStatus?: string;
  // True if the user message is a "[Request interrupted by user]"
  // sentinel — Claude's own log of the user pressing Esc mid-stream.
  // Rendered with a distinct style so it's clear this wasn't typed.
  interrupted?: boolean;
  // True if this is a queued user input — the user typed while Claude
  // was still working on a previous turn, so the message is captured
  // by Claude's queue but not yet processed. Shown faded; replaced by
  // the real user_input event once Claude picks it off the queue.
  queued?: boolean;
  // For failed user sends: one-line reason surfaced in the red status
  // strip. Kept short because the actionable detail lives in RemoteView's
  // off-path banner when the terminal snapshot shows a picker/prompt.
  failureReason?: string;
  // For pending user sends: the number of JSONL occurrences of this exact
  // content that ALREADY existed when we sent it. The send is confirmed
  // only once the occurrence count EXCEEDS this baseline — so a re-send of
  // text already in history (from a prior turn, the terminal, or another
  // client) isn't falsely confirmed by the old occurrence.
  confirmBaseline?: number;
}

const STORAGE_PREFIX = 'sessions:dispatch:';
const MAX_PER_SESSION = 200;       // localStorage budget cap
const PENDING_TIMEOUT_MS = 6_000;  // pending → failed after this
const SUFFIX_LEN = 30;             // chars at end-of-message used for matching
const PENDING_TIMEOUT_REASON = 'no matching user event appeared within 6s';

// Closed-loop Enter-retry: after the initial paste+Enter, if the parser
// hasn't confirmed receipt by these offsets, send another `\r`. Each
// retry is a single Enter byte — safe to repeat because once Claude
// submits the input box, subsequent Enters land on an empty box (no-op)
// until the user types again. We do NOT re-send the paste content
// here — that would risk doubling whatever's still in Claude's input
// buffer. Manual retry button (red bubble) does re-paste.
const ENTER_RETRY_OFFSETS_MS = [2000, 4500];

function storageKey(serverId: string, sessionId: string): string {
  return `${STORAGE_PREFIX}${serverId}:${sessionId}`;
}

function readStored(serverId: string, sessionId: string): DispatchMessage[] {
  try {
    const raw = window.localStorage.getItem(storageKey(serverId, sessionId));
    if (!raw) return [];
    const parsed = JSON.parse(raw) as DispatchMessage[];
    return Array.isArray(parsed) ? parsed.slice(-MAX_PER_SESSION) : [];
  } catch { return []; }
}

function writeStored(serverId: string, sessionId: string, messages: DispatchMessage[]): void {
  try {
    const cap = messages.slice(-MAX_PER_SESSION);
    window.localStorage.setItem(storageKey(serverId, sessionId), JSON.stringify(cap));
  } catch { /* quota / private mode — non-fatal */ }
}

// Normalize content for fuzzy matching. Whitespace collapsed, leading/
// trailing trimmed. Long messages match on suffix, since Claude can
// elide pasted regions as "[Pasted text +N lines]" in its scrollback.
function normalizeForMatch(s: string): string {
  return s.replace(/\s+/g, ' ').trim();
}
function suffixOf(s: string, n: number = SUFFIX_LEN): string {
  return normalizeForMatch(s).slice(-n);
}

// Fuzzy match: does this user_input BLOCK plausibly correspond to a
// PENDING message we sent? The exact-suffix path is the cheap fast-path
// in the main loop; this is the fallback for messages where Claude's
// rendering diverges enough to break suffix equality:
//   • Pasted content → "[Pasted text #N +M lines]" placeholder.
//   • Long messages truncated with "…" or "+N more lines".
//   • Claude swaps in an image marker for an attachment.
//   • Trailing whitespace / punctuation differences.
//
// We accept any of: prefix match (≥10 chars), block content contained
// in pending, or pending content contained in block. The 10-char floor
// stops a short shared word from confirming an unrelated send.
function fuzzyMatchPending(blockContent: string, pendingContent: string): boolean {
  const b = normalizeForMatch(blockContent);
  const p = normalizeForMatch(pendingContent);
  if (!b || !p) return false;
  // Exact equality of normalized text.
  if (b === p) return true;
  // Prefix overlap — typical when Claude truncates the tail.
  const head = Math.min(b.length, p.length, SUFFIX_LEN);
  if (head >= 10 && b.slice(0, head) === p.slice(0, head)) return true;
  // Containment — handles the pasted-text-placeholder case in either
  // direction (block has extra "[Pasted text]" prefix, or pending has
  // an explicit marker the block doesn't).
  if (b.length >= 10 && p.includes(b)) return true;
  if (p.length >= 10 && b.includes(p)) return true;
  return false;
}

interface Args {
  sessionId: string;
  // Parser blocks. Optional now that JSONL events are the canonical
  // chat-log source (see RemoteView → eventsToMessages). When omitted,
  // the reconciler effectively becomes a no-op for block matching and
  // pendings are confirmed via RemoteView's content-suffix filter
  // against the event-derived messages instead. Pendings that don't
  // confirm within PENDING_TIMEOUT_MS are still marked failed by the
  // 2s timeout interval.
  blocks?: Block[];
  // Occurrence COUNT, per trimmed content, of user messages in the JSONL
  // event stream. Pending entries get flipped to 'sent' only when the
  // JSONL has more occurrences of their content than we've already matched
  // to a sent entry — so re-sending text that already appears in history
  // isn't falsely confirmed by the old occurrence (a count, not a set, is
  // required to distinguish "this exact send landed" from "this text has
  // appeared before").
  eventUserContentCounts?: ReadonlyMap<string, number>;
  send: (data: string) => void;
}

export interface DispatchAPI {
  messages: DispatchMessage[];
  // Called from InputBar.onSubmitted right after it dispatched the
  // bytes through the WS. We just record the entry; the actual byte
  // send is owned by InputBar so the existing keyboard-input + paste
  // protocol stays in one place.
  recordSent: (content: string) => void;
  // Re-dispatch a failed message's bytes and reset its status to
  // pending. Used by the retry button on red-bordered entries.
  retry: (id: string) => void;
  // Drop a message from the local log. Useful for failed messages
  // that were actually delivered (false negatives) — the user wants
  // the misleading "not delivered" tombstone gone. If Claude really
  // did render it, the bootstrap path will re-add it as a fresh
  // sent entry on the next reconciler tick.
  remove: (id: string) => void;
  // Wipe the entire local log and let the bootstrap path rebuild from
  // current parser blocks. Recovery for cases where accumulated
  // dispatch-log cruft (old retry artifacts, picker-misparses, etc.)
  // makes Remote disagree with the Terminal view.
  resetLog: () => void;
}

export function useDispatch({ sessionId, blocks = [], eventUserContentCounts, send }: Args): DispatchAPI {
  // Session views only mount behind App's active-server gate.
  const activeServerId = useServers((s) => s.activeId!);
  const [messages, setMessages] = useState<DispatchMessage[]>(() =>
    readStored(activeServerId, sessionId)
  );

  // Hydrate when (server, session) flips. Pure in-memory swap — no
  // network. The reconciler effect below catches it up to current
  // parser state on the next blocks update.
  useEffect(() => {
    setMessages(readStored(activeServerId, sessionId));
  }, [activeServerId, sessionId]);

  // Persist on every change. Synchronous write is fine — localStorage
  // is fast enough for a few hundred messages.
  useEffect(() => {
    writeStored(activeServerId, sessionId, messages);
  }, [messages, activeServerId, sessionId]);

  // Reconciler. Runs on every parser-blocks update.
  //   1. Walk our pending user messages — flip to 'sent' when a
  //      user_input block has a matching suffix.
  //   2. Bootstrap from existing user_input blocks: any block we
  //      don't already have a record for (matched by content key)
  //      gets appended. This covers two cases — sessions we opened
  //      Remote on for the first time (parser already has history
  //      from before we existed) AND user_inputs that arrived via
  //      a different client (Tauri pop-out, mobile, the terminal
  //      directly). Without this, Remote shows a one-sided
  //      conversation of just Claude's replies.
  //   3. Append claude_message blocks we haven't seen yet (by
  //      block.id) as assistant entries.
  //   4. Mark stale pendings (>10s) as failed.
  //
  // Strategy: walk blocks in order, building a NEW message list that
  // merges parser-discovered turns with our own pending sends. Then
  // diff against `prev` — if no actual change, return prev to avoid a
  // pointless re-render.
  useEffect(() => {
    setMessages((prev) => {
      const now = Date.now();

      // Pre-compute lookups against our existing history.
      const seenAssistantBlockIds = new Set(
        prev.filter((m) => m.role === 'assistant' && m.blockId).map((m) => m.blockId!)
      );
      // Block IDs we've already consumed for user messages — either by
      // bootstrap (block → new log entry) or confirmation (block →
      // existing pending). Each block confirms AT MOST ONE pending; a
      // re-emit of the same block on the next tick is a no-op. This is
      // what prevents an older "ok" block from false-confirming a
      // fresh "ok" pending — the older block's id is already consumed.
      const consumedUserBlockIds = new Set(
        prev.filter((m) => m.role === 'user' && m.blockId).map((m) => m.blockId!)
      );
      // Legacy content-key fallback for log entries written before we
      // started storing blockId on user messages. Lets the bootstrap
      // path still recognize "we already have this in the log" for
      // older entries; new entries always carry blockId.
      const seenUserKeysLegacy = new Set(
        prev
          .filter((m) => m.role === 'user' && !m.blockId)
          .map((m) => suffixOf(m.content) + '|' + normalizeForMatch(m.content).length)
      );
      // Pending indexes keyed by content tail. We process this as a
      // QUEUE — when a block confirms a tail, only the OLDEST matching
      // pending flips to sent and is shifted off. Two identical pendings
      // need two distinct user_input blocks to both reach 'sent'.
      const pendingByTail = new Map<string, number[]>();
      prev.forEach((m, i) => {
        if (m.role === 'user' && m.status === 'pending') {
          const tail = suffixOf(m.content);
          if (!tail) return;
          const arr = pendingByTail.get(tail) ?? [];
          arr.push(i);
          pendingByTail.set(tail, arr);
        }
      });

      const next = prev.slice();
      let changed = false;

      // Single pass through parser blocks in order. Each block is
      // either: (a) a user_input we already have, (b) a user_input
      // matching a pending we just sent, (c) a new user_input from
      // somewhere else (bootstrap), (d) a new claude_message, (e) an
      // already-recorded claude_message, or (f) something we don't
      // care about. Dispatching in block order is what gives the
      // chat its natural interleaving — user/assistant/user/assistant
      // — instead of all-users-then-all-assistants.
      //
      // Bootstrap timestamps: when we bootstrap historical messages
      // from blocks the parser already had before we ran, we use a
      // monotonically-increasing fake timestamp so block order is
      // preserved by sort. New live discoveries use real Date.now().
      let bootstrapTs = now - blocks.length;
      for (const b of blocks) {
        bootstrapTs += 1;
        if (b.type === 'user_input' && b.content) {
          const tail = suffixOf(b.content);
          if (!tail) continue;
          // Already consumed (either matched a pending earlier, or
          // bootstrapped on a prior tick). Skip — same block can't
          // confirm two messages.
          if (consumedUserBlockIds.has(b.id)) {
            // Still update an attached error response if the parser
            // observed one now that wasn't there before.
            const errResp = ((b.metadata?.responses as Array<{ type: string; content: string }> | undefined) ?? [])
              .filter((r) => r.type === 'error').map((r) => r.content).join('\n');
            if (errResp) {
              const idx = next.findIndex((m) => m.role === 'user' && m.blockId === b.id);
              if (idx >= 0 && next[idx]!.errorResponse !== errResp) {
                next[idx] = { ...next[idx]!, errorResponse: errResp };
                changed = true;
              }
            }
            continue;
          }
          // Surface error responses (API errors, rate limits) captured
          // by the parser as user_input metadata. Joined into a single
          // string for display under the user bubble.
          const errResponses = ((b.metadata?.responses as Array<{ type: string; content: string }> | undefined) ?? [])
            .filter((r) => r.type === 'error')
            .map((r) => r.content);
          const errorResponse = errResponses.length > 0 ? errResponses.join('\n') : undefined;

          const pendingIdxs = pendingByTail.get(tail);
          if (pendingIdxs && pendingIdxs.length > 0) {
            // Match the OLDEST pending only — each block confirms a
            // single message. The remaining pendings stay pending,
            // awaiting their own block to land.
            const idx = pendingIdxs.shift()!;
            if (pendingIdxs.length === 0) pendingByTail.delete(tail);
            const m = next[idx]!;
            if (m.status === 'pending') {
              next[idx] = { ...m, status: 'sent', confirmedAt: now, errorResponse, blockId: b.id, failureReason: undefined };
              consumedUserBlockIds.add(b.id);
              changed = true;
            }
            continue;
          }
          // Legacy log entries (pre-blockId) — match by content key.
          const key = tail + '|' + normalizeForMatch(b.content).length;
          if (seenUserKeysLegacy.has(key)) {
            seenUserKeysLegacy.delete(key);  // one block per legacy entry
            if (errorResponse) {
              const idx = next.findIndex((m) =>
                m.role === 'user' && !m.blockId &&
                suffixOf(m.content) + '|' + normalizeForMatch(m.content).length === key
              );
              if (idx >= 0 && next[idx]!.errorResponse !== errorResponse) {
                next[idx] = { ...next[idx]!, errorResponse, blockId: b.id };
                consumedUserBlockIds.add(b.id);
                changed = true;
              }
            }
            continue;
          }
          next.push({
            id: `user-${bootstrapTs}-${Math.random().toString(36).slice(2, 8)}-${b.id.slice(-6)}`,
            role: 'user',
            content: b.content,
            status: 'sent',
            createdAt: bootstrapTs,
            confirmedAt: bootstrapTs,
            blockId: b.id,
            errorResponse
          });
          consumedUserBlockIds.add(b.id);
          changed = true;
        } else if (b.type === 'claude_message') {
          if (seenAssistantBlockIds.has(b.id)) {
            // Already in our log. But Claude's TUI streams: an
            // assistant block can be SHORTER on the first parse tick
            // (just the leading "Nothing needs to ship.") and GROW
            // on later ticks (bullets get added). The block.id is
            // stable because it hashes the first line, so we'd
            // otherwise keep the early truncated copy forever.
            // Update the existing entry's content when it differs.
            // (We previously tried to refuse "shrinks" here to fix
            // the visible bouncing of the latest bubble, but that
            // surfaced edge cases where stale content was retained.
            // Bouncing is now handled at the CSS layer — RemoteView
            // applies a min-height ratchet to the latest bubble.)
            const idx = next.findIndex(
              (m) => m.role === 'assistant' && m.blockId === b.id
            );
            if (idx >= 0 && next[idx]!.content !== b.content) {
              next[idx] = { ...next[idx]!, content: b.content };
              changed = true;
            }
            continue;
          }
          next.push({
            id: `assistant-${bootstrapTs}-${Math.random().toString(36).slice(2, 8)}-${b.id.slice(-6)}`,
            role: 'assistant',
            content: b.content,
            status: 'sent',
            createdAt: bootstrapTs,
            blockId: b.id
          });
          seenAssistantBlockIds.add(b.id);
          changed = true;
        }
      }

      // Second-pass fuzzy match: any pending entries that didn't get
      // confirmed via the exact-suffix lookup get re-checked against
      // every user_input block via the looser fuzzyMatchPending. This
      // is what saves messages whose Claude-rendered form diverges
      // enough that suffix equality fails — pasted text placeholders,
      // tail truncation, image-attachment markers, etc.
      // Only consider blocks NOT already consumed for someone else.
      // Same rule as the exact-suffix path: one block, one
      // confirmation. Without this, the fuzzy pass could re-use the
      // same old block to false-confirm multiple new pendings.
      const fuzzyCandidates = blocks.filter(
        (b) => b.type === 'user_input' && b.content && !consumedUserBlockIds.has(b.id)
      );
      for (let i = 0; i < next.length; i++) {
        const m = next[i]!;
        // Apply the fuzzy match to BOTH pending and already-failed
        // messages. The failed-recovery path matters: a message can
        // tombstone at the timeout mark and only later have a matching
        // block appear (parser was lagging, or Claude rendered the
        // input belatedly). When the match shows up, flip back to
        // sent rather than leaving a permanent "not delivered" lie.
        if (m.role !== 'user') continue;
        if (m.status !== 'pending' && m.status !== 'failed') continue;
        const matchedBlock = fuzzyCandidates.find((b) => fuzzyMatchPending(b.content, m.content));
        if (matchedBlock) {
          next[i] = { ...m, status: 'sent', confirmedAt: now, blockId: matchedBlock.id, failureReason: undefined };
          consumedUserBlockIds.add(matchedBlock.id);
          // Remove from candidates so the next pending in the loop
          // doesn't also match the same block.
          const idx = fuzzyCandidates.indexOf(matchedBlock);
          if (idx >= 0) fuzzyCandidates.splice(idx, 1);
          changed = true;
        }
      }

      // JSONL-event confirmation (baseline-aware). A send is confirmed only
      // when the JSONL occurrence count for its content EXCEEDS the baseline
      // captured when we sent it (how many occurrences already existed).
      // This is what stops a re-send of text that's already in history —
      // from a prior turn, the terminal, or another client — from being
      // falsely confirmed by the OLD occurrence while the new bytes were
      // dropped. Distinct re-sends get successive baselines (N, N+1, …) so
      // they confirm against successive new occurrences.
      if (eventUserContentCounts && eventUserContentCounts.size > 0) {
        for (let i = 0; i < next.length; i++) {
          const m = next[i]!;
          if (m.role !== 'user') continue;
          if (m.status !== 'pending' && m.status !== 'failed') continue;
          const have = eventUserContentCounts.get(m.content.trim()) ?? 0;
          if (have > (m.confirmBaseline ?? 0)) {
            next[i] = { ...m, status: 'sent', confirmedAt: now, failureReason: undefined };
            changed = true;
          }
        }
      }

      // Stale-pending → failed. Only after fuzzy match has had its
      // chance — otherwise we'd race the parser tick and tombstone
      // genuinely-delivered messages.
      for (let i = 0; i < next.length; i++) {
        const m = next[i]!;
        if (m.role === 'user' && m.status === 'pending' && now - m.createdAt > PENDING_TIMEOUT_MS) {
          next[i] = { ...m, status: 'failed', failureReason: PENDING_TIMEOUT_REASON };
          changed = true;
        }
      }

      // Stable sort by createdAt — pending sends (with real Date.now())
      // land at the END since their ts is greater than the bootstrap
      // run. Bootstrapped messages keep their block-order ts.
      if (changed) {
        next.sort((a, b) => a.createdAt - b.createdAt);
      }

      return changed ? next : prev;
    });
  }, [blocks, eventUserContentCounts]);

  // Re-check pending timeouts every 2s even when blocks haven't changed
  // — otherwise a message dispatched while the parser is offline could
  // sit in 'pending' forever waiting for the next blocks update.
  const timeoutCheckRef = useRef(0);
  useEffect(() => {
    const tick = (): void => {
      timeoutCheckRef.current = Date.now();
      setMessages((prev) => {
        let changed = false;
        const now = Date.now();
        const next = prev.map((m) => {
          if (m.role === 'user' && m.status === 'pending' && now - m.createdAt > PENDING_TIMEOUT_MS) {
            changed = true;
            return { ...m, status: 'failed' as const, failureReason: PENDING_TIMEOUT_REASON };
          }
          return m;
        });
        return changed ? next : prev;
      });
    };
    const id = window.setInterval(tick, 2000);
    return () => window.clearInterval(id);
  }, []);

  // Keep a ref to the latest messages so callbacks can read from it
  // without putting the lookup inside a state setter. Setter callbacks
  // must be PURE — React (StrictMode in dev, concurrent mode in prod
  // under some scenarios) re-runs them to detect impurity, which
  // double-fires any side effect inside.
  const messagesRef = useRef(messages);
  useEffect(() => {
    messagesRef.current = messages;
  });

  // Latest JSONL occurrence counts, so recordSent can snapshot the
  // confirmation baseline at send time without re-running on every change.
  const eventCountsRef = useRef(eventUserContentCounts);
  useEffect(() => {
    eventCountsRef.current = eventUserContentCounts;
  });

  // Closed-loop dispatch — every pending message owns a small queue of
  // scheduled Enter-retry timers. The reason: Claude's TUI sometimes
  // swallows the first `\r` (paste-end marker still being processed,
  // active streaming animation, autocomplete picker, etc.) and the
  // user has to press Enter again manually. With closed-loop, the
  // reconciler watches for a matching user_input block; if it doesn't
  // appear, we automatically resend `\r` at backoff offsets. Each
  // retry is safe because once Claude submits the input box, the box
  // is empty and subsequent Enters are no-ops.
  const retryTimersRef = useRef<Map<string, number[]>>(new Map());

  const clearRetryTimers = useCallback((id: string): void => {
    const timers = retryTimersRef.current.get(id);
    if (!timers) return;
    for (const t of timers) window.clearTimeout(t);
    retryTimersRef.current.delete(id);
  }, []);

  const scheduleEnterRetries = useCallback((id: string): void => {
    clearRetryTimers(id);
    const timers: number[] = [];
    for (const delay of ENTER_RETRY_OFFSETS_MS) {
      const t = window.setTimeout(() => {
        const cur = messagesRef.current.find((m) => m.id === id);
        if (!cur || cur.status !== 'pending') return;
        // Send a bare Enter. If Claude's input box still holds our
        // earlier paste, this submits it. If the box is empty (e.g.,
        // earlier Enter already submitted but our parser hasn't seen
        // the user_input block yet), this is a no-op.
        send('\r');
      }, delay);
      timers.push(t);
    }
    retryTimersRef.current.set(id, timers);
  }, [send, clearRetryTimers]);

  // When any pending message flips to a non-pending status (sent /
  // failed / removed), cancel its retry timers. Hygiene only — the
  // timers self-cancel via the status check when they fire, but this
  // releases the timer slot earlier.
  useEffect(() => {
    for (const [id, timers] of retryTimersRef.current.entries()) {
      const m = messages.find((x) => x.id === id);
      if (!m || m.status !== 'pending') {
        for (const t of timers) window.clearTimeout(t);
        retryTimersRef.current.delete(id);
      }
    }
  }, [messages]);

  // Cleanup on unmount — clear all pending retry timers.
  useEffect(() => {
    return () => {
      for (const timers of retryTimersRef.current.values()) {
        for (const t of timers) window.clearTimeout(t);
      }
      retryTimersRef.current.clear();
    };
  }, []);

  const recordSent = useCallback((content: string): void => {
    if (!content.trim()) return;
    const now = Date.now();
    const prev = messagesRef.current;

    // Dedupe accidental double-sends. If the most recent message is a
    // still-pending user message with the same content (within 30s),
    // reuse its id rather than appending a duplicate. The bytes from
    // InputBar already went to the PTY again, and our retry-Enter
    // logic will handle submission.
    const last = prev[prev.length - 1];
    let targetId: string;
    if (
      last &&
      last.role === 'user' &&
      last.status === 'pending' &&
      last.content === content &&
      now - last.createdAt < 30_000
    ) {
      targetId = last.id;
      setMessages((p) =>
        p.map((m) => (m.id === targetId ? { ...m, createdAt: now, failureReason: undefined } : m))
      );
    } else {
      // Snapshot the confirmation baseline: how many JSONL occurrences of
      // this exact content already exist, plus any unconfirmed sends of the
      // same content still queued ahead of this one (so identical re-sends
      // confirm against successive new occurrences, not the same one).
      const trimmed = content.trim();
      const jsonlCount = eventCountsRef.current?.get(trimmed) ?? 0;
      const queuedAhead = prev.filter(
        (m) => m.role === 'user'
          && (m.status === 'pending' || m.status === 'failed')
          && m.content.trim() === trimmed
      ).length;
      targetId = `user-${now}-${Math.random().toString(36).slice(2, 8)}`;
      const msg: DispatchMessage = {
        id: targetId,
        role: 'user',
        content,
        status: 'pending',
        createdAt: now,
        confirmBaseline: jsonlCount + queuedAhead
      };
      setMessages((p) => [...p, msg]);
    }

    // Schedule Enter-retry watchers (outside setMessages so React
    // doesn't double-fire under StrictMode).
    scheduleEnterRetries(targetId);
  }, [scheduleEnterRetries]);

  const retry = useCallback((id: string): void => {
    const target = messagesRef.current.find((m) => m.id === id);
    if (!target || target.role !== 'user') return;
    // Manual retry: re-paste + Enter, and schedule the same closed-loop
    // Enter retries. We re-paste here (unlike the auto-retries) because
    // the user explicitly clicked retry, so we should assume the
    // original delivery is fully gone — not just stuck waiting on a
    // missing Enter.
    send('\x1b[200~' + target.content + '\x1b[201~');
    window.setTimeout(() => send('\r'), 50);
    const trimmed = target.content.trim();
    const jsonlCount = eventCountsRef.current?.get(trimmed) ?? 0;
    const queuedAhead = messagesRef.current.filter(
      (m) => m.id !== id
        && m.role === 'user'
        && (m.status === 'pending' || m.status === 'failed')
        && m.content.trim() === trimmed
    ).length;
    setMessages((prev) =>
      prev.map((m) =>
        m.id === id
          ? {
              ...m,
              status: 'pending' as const,
              createdAt: Date.now(),
              confirmBaseline: jsonlCount + queuedAhead,
              failureReason: undefined
            }
          : m
      )
    );
    scheduleEnterRetries(id);
  }, [send, scheduleEnterRetries]);

  const remove = useCallback((id: string): void => {
    setMessages((prev) => prev.filter((m) => m.id !== id));
  }, []);

  // Wipe the entire local message log for this session. The persistence
  // useEffect writes [] back to localStorage as a side-effect, so this
  // is also durable across reloads. The reconciler will immediately
  // re-bootstrap from current parser blocks on the next tick — so the
  // user sees the conversation re-derived from honest scrollback,
  // minus any cruft we'd accumulated.
  const resetLog = useCallback((): void => {
    setMessages([]);
  }, []);

  return { messages, recordSent, retry, remove, resetLog };
}
