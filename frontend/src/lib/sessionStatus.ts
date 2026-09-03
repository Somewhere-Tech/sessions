import type { SessionInfo } from '../types';

// ───────────────────────────────────────────────────────────────────────────
// The one session classifier.
//
// Before this module owned the final answer, five surfaces each derived
// "what state is this session in?" independently — SessionNavigator (twice:
// once for its tree rows, once for its all-machines rows), FleetView,
// HomeView, GridView, SessionView — with different precedence orders and
// different words. Two of them could disagree about the same session while
// both were on screen:
//
//   • An exited session still carrying idleReason:'needs-input' read "Ended"
//     in the navigator and "Needs you" in Fleet.
//   • A degraded session counted toward the navigator's "Needs you" filter
//     badge but not toward Home's "Needs you" tile, so the two numbers on the
//     same screen described different sets.
//
// One classifier, one vocabulary. Surfaces choose how much of it to render;
// they never re-derive it.
//
// ── Precedence, and why it is in this order ────────────────────────────────
//
// 1. unavailable / reconnecting — session.unreachable. When the daemon still
//    has a process identity it is actively reconnecting. A restored record
//    with no process identity cannot honestly promise that; it says
//    "Connection lost" while preserving the saved record and never inventing
//    an exit. Both outrank stale provenance and activity hints.
//
// 2. failed — isCrashedSession(). Highest among known lifecycle outcomes
//    because it is the only state where
//    the user may have lost a runtime they did not choose to lose, and it is
//    true of live sessions too (provenanceStatus 'lost'/'invalid' without an
//    exit frame). Nothing below it may hide it.
//
// 3. provider-down / auth-needed / failed — a provider turn fault. These are
//    ranked with a crashed runtime because the current turn did not complete,
//    but they keep their actionable cause and provider-specific wording.
//
// 4. ended — session.exited. Exit is terminal and it outranks every live
//    hint below, because `working` and `idleReason` describe a process that
//    no longer exists. A dead runtime cannot be waiting for you, so an exited
//    record whose idleReason was frozen at 'needs-input' reads "Ended"
//    everywhere. (The daemon overwrites idleReason on exit — see
//    runtime/internal/state/session.go — so a needs-input+exited pair only
//    reaches the UI from a cached, adopted, or fleet-snapshot record. Those
//    are exactly the records that used to make two surfaces disagree.)
//
// 5. needs-you — idleReason === 'needs-input'. Deliberately ABOVE `working`,
//    reversing what FleetView/HomeView/SessionNavigator each did before.
//    docs/PRINCIPLES.md: provider approval prompts "are durable needs-input
//    state for users and agents", and "cleanup must never hide an unresolved
//    decision". `working` is a transient activity heuristic; needs-input is a
//    recorded, durable, user-blocking fact. The durable fact wins.
//
//    In the daemon these are already mutually exclusive — SetWorking(true)
//    clears IdleReason — so for a live snapshot this reorder changes nothing.
//    It changes two real cases for the better: a stale cached snapshot, and
//    SessionView, whose sidebar derives isWorking from the provider event
//    stream and reports "working" for an assistant turn that stopped on
//    stop_reason 'tool_use' — which is precisely a tool waiting on the user's
//    approval. That surface used to label a pending approval "Working".
//
// 6. working.
//
// 7. limited — isDegradedSession(). The agent is alive and answering; one
//    optional capability (typically an MCP server) did not start. Per
//    "Calm, literal lifecycle language" this is not an alarm and it is not a
//    question, so it must NOT inflate a "Needs you" count — that was the
//    navigator's bug. It also sits below `working` so a degraded session that
//    is actively working says so; `degraded` stays exposed on the result for
//    surfaces that want to badge it alongside.
//
// 8. finished — live, idleReason 'completed'. The provider run finished but
//    the runtime is still up and resumable.
//
// 9. not-started / 10. ready — idle with and without a recorded reason.
//
// Unknown idle state is never escalated: a provider that can recognise a
// question records needs-input explicitly, and guessing would alarm the user
// on no evidence.
// ───────────────────────────────────────────────────────────────────────────

export type SessionStatusState =
  | 'reconnecting'
  | 'unavailable'
  | 'needs-recovery'
  | 'failed'
  | 'provider-down'
  | 'auth-needed'
  | 'ended'
  | 'needs-you'
  | 'working'
  | 'limited'
  | 'finished'
  | 'not-started'
  | 'ready';

export type SessionStatusTone =
  | 'attention'
  | 'ended'
  | 'needs'
  | 'working'
  | 'limited'
  | 'completed'
  | 'ready';

export interface SessionStatus {
  state: SessionStatusState;
  /** The single user-facing word for this state. Do not re-word per surface. */
  label: string;
  /** `is-<state>` — the CSS token every surface styles this state with. */
  className: string;
  tone: SessionStatusTone;
  /** True when the runtime is alive but one optional capability is missing. */
  degraded: boolean;
  /** True only for a recorded, user-blocking provider question. */
  needsYou: boolean;
  /** Ended cleanly, or live with the provider run completed. */
  finished: boolean;
  /** A recovery, provider, question, crash, or capability issue worth surfacing. */
  wantsAttention: boolean;
}

const STATE_LABELS: Record<SessionStatusState, string> = {
  reconnecting: 'Connecting…',
  unavailable: 'Not connected',
  'needs-recovery': 'Needs recovery',
  failed: 'Failed',
  'provider-down': 'Provider unavailable',
  'auth-needed': 'Needs login',
  ended: 'Ended',
  'needs-you': 'Needs you',
  working: 'Working',
  limited: 'Limited',
  finished: 'Finished',
  'not-started': 'Not started',
  ready: 'Ready'
};

const STATE_TONES: Record<SessionStatusState, SessionStatusTone> = {
  reconnecting: 'ready',
  unavailable: 'ended',
  'needs-recovery': 'needs',
  failed: 'attention',
  'provider-down': 'attention',
  'auth-needed': 'needs',
  ended: 'ended',
  'needs-you': 'needs',
  working: 'working',
  limited: 'limited',
  finished: 'completed',
  'not-started': 'ready',
  ready: 'ready'
};

export interface ClassifyOptions {
  /**
   * A richer live activity signal than `session.working` — currently only
   * SessionView's provider-event sidebar has one. It replaces the daemon flag
   * at step 4; it cannot promote a session past `failed`, `ended`, or
   * `needs-you`, which is the whole point of the ordering above.
   */
  working?: boolean;
}

function statusState(session: SessionInfo, options: ClassifyOptions): SessionStatusState {
  if (session.unreachableReason === 'restart-restore-pending') return 'needs-recovery';
  if (session.unreachable) return session.pid && session.pid > 0 ? 'reconnecting' : 'unavailable';
  if (isCrashedSession(session)) return 'failed';
  if (session.failureKind === 'auth') return 'auth-needed';
  if (session.failureKind === 'provider-unavailable' || session.failureKind === 'rate-limited') return 'provider-down';
  if (session.failureKind === 'other') return 'failed';
  if (session.exited) return 'ended';
  if (session.idleReason === 'needs-input') return 'needs-you';
  if (options.working ?? session.working) return 'working';
  if (isDegradedSession(session)) return 'limited';
  if (session.idleReason === 'completed') return 'finished';
  if (session.idleReason === 'never-started') return 'not-started';
  return 'ready';
}

/** The single source of truth for "what state is this session in?". */
export function classifySession(session: SessionInfo, options: ClassifyOptions = {}): SessionStatus {
  const state = statusState(session, options);
  const label = state === 'provider-down'
    ? session.failureKind === 'rate-limited'
      ? 'Rate limited'
      : session.failureProvider === 'codex'
        ? 'Codex unavailable'
        : session.failureProvider === 'claude'
          ? 'Claude unavailable'
          : STATE_LABELS[state]
    : STATE_LABELS[state];
  return {
    state,
    label,
    className: `is-${state}`,
    tone: STATE_TONES[state],
    degraded: isDegradedSession(session),
    needsYou: state === 'needs-you',
    finished: state === 'ended' || state === 'finished',
    wantsAttention: state === 'needs-recovery' || state === 'needs-you' || state === 'failed'
      || state === 'provider-down' || state === 'auth-needed' || state === 'limited'
  };
}

export function sessionHasProviderFault(session: SessionInfo): boolean {
  return Boolean(session.failureKind);
}

/** Convenience predicates. Every one of them defers to classifySession. */
export function sessionNeedsYou(session: SessionInfo): boolean {
  return classifySession(session).needsYou;
}

export function sessionIsFinished(session: SessionInfo): boolean {
  return classifySession(session).finished;
}

export function sessionWantsAttention(session: SessionInfo): boolean {
  return classifySession(session).wantsAttention;
}

export interface EndedSummary {
  label: string;
  detail: string;
  tone: 'completed' | 'attention' | 'ended';
}

export type EndedCategory = 'user' | 'provider' | 'continued' | 'crashed' | 'other';

export function continuationSession(session: SessionInfo, allSessions: SessionInfo[]): SessionInfo | null {
  const linkedIDs = new Set([
    session.reopenedAs,
    session.movedToSessionId
  ].filter((value): value is string => Boolean(value)));
  const providerID = providerConversationId(session);
  const candidates = allSessions.filter((candidate) => {
    if (candidate.id === session.id) return false;
    if (linkedIDs.has(candidate.id)) return true;
    if (candidate.resumedFrom === session.id || candidate.movedFromSessionId === session.id) return true;
    return Boolean(
      providerID
      && providerConversationId(candidate) === providerID
      && candidate.createdAt >= session.createdAt
    );
  });
  return candidates.sort((left, right) => {
    if (left.exited !== right.exited) return left.exited ? 1 : -1;
    return right.createdAt - left.createdAt;
  })[0] ?? null;
}

export function isDegradedSession(session: SessionInfo): boolean {
  return !session.exited
    && !session.failureKind
    && session.idleReason === 'failed'
    && session.provenanceStatus !== 'lost'
    && session.provenanceStatus !== 'invalid';
}

export function isCrashedSession(session: SessionInfo): boolean {
  const reason = session.exitReason?.trim().toLowerCase() ?? '';
  if (reason === 'ended-by-user' || reason === 'continued') return false;
  return session.provenanceStatus === 'lost'
    || session.provenanceStatus === 'invalid'
    || (session.exited && (
      (session.exitCode != null && session.exitCode !== 0)
      || Boolean(session.exitSignal)
      || reason === 'failed'
      || reason === 'runner-lost'
      || reason === 'signaled'
    ));
}

export function endedCategory(session: SessionInfo): EndedCategory {
  const reason = session.exitReason?.trim().toLowerCase() ?? '';
  if (reason === 'ended-by-user') return 'user';
  if (reason === 'continued') return 'continued';
  if (isCrashedSession(session)) return 'crashed';
  if (reason === 'completed' || session.exitCode === 0) return 'provider';
  return 'other';
}

export function endedSummary(session: SessionInfo, allSessions: SessionInfo[] = []): EndedSummary {
  const reason = session.exitReason?.trim().toLowerCase() ?? '';
  const code = session.exitCode;
  const signal = session.exitSignal?.trim() ?? '';
  const continuation = continuationSession(session, allSessions);
  const hasContinuation = Boolean(continuation || session.reopenedAs || session.movedToSessionId);

  if (hasContinuation) {
    const name = continuation?.name?.trim()
      || continuation?.description?.trim()
      || continuation?.claudeCustomTitle?.trim()
      || continuation?.claudeAiTitle?.trim();
    if (continuation && !continuation.exited) {
      return {
        label: name ? `Live as ${name}` : 'Conversation is live in a new session',
        detail: 'This runtime ended after the same conversation continued in a new live session.',
        tone: 'completed'
      };
    }
    return {
      label: name
        ? `Continued as ${name}`
        : session.movedToEndpoint
        ? `Continued on ${session.movedToEndpoint}`
        : 'Continued in a new session',
      detail: 'This runtime ended after the conversation continued elsewhere.',
      tone: 'ended'
    };
  }

  if (reason === 'ended-by-user') {
    const actor = endInitiatorLabel(session, allSessions);
    const detail: string[] = [];
    if (session.endReason?.trim()) detail.push(session.endReason.trim());
    if (session.endOperationId) detail.push('This was part of a batch end operation.');
    if (session.endedByKind === 'session') {
      detail.push('The request came from the Sessions CLI inside that agent session.');
    } else if (session.endedByClient === 'sessions-cli') {
      detail.push('The request came from the Sessions CLI.');
    } else if (session.endedByClient === 'sessions-desktop') {
      detail.push('The request came from the Sessions desktop app.');
    }
    if (detail.length === 0) {
      detail.push('An explicit end request was recorded. Older records do not contain the exact initiator.');
    }
    return { label: actor, detail: detail.join(' '), tone: 'ended' };
  }
  if (reason === 'runner-lost' || session.provenanceStatus === 'lost') {
    return {
      label: 'Ready to continue',
      detail: 'The live runner stopped, but this conversation and its captured output are saved. Resume it here or on another computer whenever you’re ready.',
      tone: 'attention'
    };
  }
  if (reason === 'continued') {
    const resumed = session.reopenedAs
      ? allSessions.find((candidate) => candidate.id === session.reopenedAs)
      : null;
    const name = resumed?.name?.trim()
      || resumed?.description?.trim()
      || resumed?.claudeCustomTitle?.trim()
      || resumed?.claudeAiTitle?.trim();
    return {
      label: name ? `Resumed as ${name}` : 'Resumed in a new session',
      detail: 'A new runtime resumed this conversation.',
      tone: 'ended'
    };
  }
  if (signal) {
    return {
      label: 'Ended unexpectedly',
      detail: `The live process received ${signal}. Saved history is still available.`,
      tone: 'attention'
    };
  }
  if (code != null && code !== 0) {
    return {
      label: 'Ended unexpectedly',
      detail: `The live process exited with code ${code}. Saved history is still available.`,
      tone: 'attention'
    };
  }
  if (reason === 'completed' || code === 0) {
    const provider = session.tool === 'claude-code' ? 'Claude' : session.tool === 'codex' ? 'Codex' : 'The shell';
    return { label: 'Finished on its own', detail: `${provider} exited normally.`, tone: 'completed' };
  }
  return { label: 'Ended', detail: 'The runtime is no longer running.', tone: 'ended' };
}

function endInitiatorLabel(session: SessionInfo, allSessions: SessionInfo[]): string {
  if (session.endedByKind === 'session' && session.endedById) {
    const initiator = allSessions.find((candidate) => candidate.id === session.endedById);
    const name = initiator?.name?.trim()
      || initiator?.description?.trim()
      || initiator?.claudeCustomTitle?.trim()
      || initiator?.claudeAiTitle?.trim()
      || `session ${session.endedById.slice(0, 8)}`;
    return `Ended by ${name}`;
  }
  if (session.endedByKind === 'external' && session.endedById) {
    return `Ended by ${session.endedByName?.trim() || session.endedById}`;
  }
  if (session.endedByClient === 'sessions-desktop') return 'You ended it';
  if (session.endedByClient === 'sessions-cli') return 'Ended from Sessions CLI';
  return 'Ended';
}

export function endedAtLabel(session: SessionInfo): string {
  if (!session.exitedAt) return 'End time unavailable';
  const date = new Date(session.exitedAt);
  if (Number.isNaN(date.getTime())) return 'End time unavailable';
  return date.toLocaleString(undefined, {
    month: 'short',
    day: 'numeric',
    year: date.getFullYear() === new Date().getFullYear() ? undefined : 'numeric',
    hour: 'numeric',
    minute: '2-digit'
  });
}

export function canContinueSession(session: SessionInfo): boolean {
  return session.tool !== 'terminal'
    && !session.reopenedAs
    && !session.movedToSessionId;
}

export function providerConversationId(session: SessionInfo): string | null {
  const known = session.conversationId?.trim() || session.claudeSessionId?.trim();
  if (known) return known;
  const args = session.args ?? [];
  const flags = session.tool === 'claude-code'
    ? ['--resume', '--session-id', '-r']
    : session.tool === 'codex'
      ? ['resume', '--resume']
      : [];
  for (let index = 0; index < args.length; index += 1) {
    const argument = args[index];
    for (const flag of flags) {
      if (argument === flag) {
        const value = args[index + 1]?.trim();
        if (value && !value.startsWith('-')) return value;
      }
      if (flag.startsWith('--') && argument.startsWith(`${flag}=`)) {
        const value = argument.slice(flag.length + 1).trim();
        if (value) return value;
      }
    }
  }
  return null;
}
