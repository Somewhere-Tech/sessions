import type { SessionInfo } from '../types';

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
  if (session.tool !== 'claude-code') return null;
  const args = session.args ?? [];
  const flagIndex = args.findIndex((arg) => arg === '--resume' || arg === '--session-id');
  return flagIndex >= 0 ? args[flagIndex + 1]?.trim() || null : null;
}
