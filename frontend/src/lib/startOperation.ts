import type { CreateSessionRequest, SessionInfo } from '../types';
import { MessageDeliveryError } from './messageDelivery';
import { randomUUID } from './uuid';
import { DaemonResponseError } from '../api/sessionsd/core';

// Starting a chat is two daemon operations — create the session, then deliver
// its first request — and either can fail after the other succeeded. The ids
// below make both idempotent: re-sending the same create operation returns the
// session the first attempt made, and the first request is delivered at most
// once under its operation id. The daemon records both before launching.

export interface StartOperationIds {
  fingerprint: string;
  create: string;
  prompt: string;
}

/**
 * The ids for one start attempt. The fingerprint is the target computer and
 * the complete create request — tool, folder, account, model, effort, runtime,
 * provider options and the first request itself. Pressing Start again for the
 * same request reuses the ids, so a create whose response was lost returns the
 * session it already made instead of a second one; changing anything
 * substantive starts a new operation rather than replaying the old work.
 */
export function startOperationIds(previous: StartOperationIds | null, fingerprint: string): StartOperationIds {
  if (previous && previous.fingerprint === fingerprint) return previous;
  return { fingerprint, create: randomUUID(), prompt: randomUUID() };
}

/** What to tell a person whose session exists but whose first request did not provably arrive. */
export function firstRequestFailureMessage(sessionId: string, reason: unknown): string {
  const short = sessionId.slice(0, 8);
  if (reason instanceof MessageDeliveryError && reason.deliveryStatus === 'not-delivered') {
    return `Session ${short} started, but its first request did not reach the provider: ${reason.message} Nothing was sent, so it is safe to send the request again from the session.`;
  }
  const detail = reason instanceof Error ? reason.message : String(reason);
  return `Session ${short} started, but Sessions could not confirm its first request: ${detail} It was not resent. Open the session and check the conversation before sending anything else.`;
}

/** The create request with its idempotency ids, added after fingerprinting. */
export function withStartOperation(request: CreateSessionRequest, ids: StartOperationIds, hasFirstRequest: boolean): CreateSessionRequest {
  return { ...request, operation_id: ids.create, prompt_operation_id: hasFirstRequest ? ids.prompt : undefined };
}

/**
 * A replayed create answers with the first-request operation id the daemon
 * recorded the first time. Sending under that id is what keeps the retry from
 * delivering the request twice; a new one would be a second message.
 */
export function recordedPromptOperationId(info: Pick<SessionInfo, 'start'>, fallback: string): string {
  return info.start?.replayed && info.start.prompt_operation_id ? info.start.prompt_operation_id : fallback;
}

/** Keep a partial creation identifiable; a lost reply is not proof of failure. */
export function startFailureMessage(reason: unknown): string {
  if (reason instanceof DaemonResponseError) {
    try {
      const body = JSON.parse(reason.body) as { session_id?: unknown };
      if (typeof body.session_id === 'string' && /^[0-9a-f-]{36}$/.test(body.session_id)) {
        return `Session ${body.session_id} was recorded, but could not finish starting: ${reason.detail} Inspect this session before starting another: sessions status ${body.session_id}`;
      }
    } catch { /* A non-JSON error carries no durable session identity. */ }
  }
  const detail = reason instanceof Error ? reason.message : String(reason);
  return `Could not confirm startup: ${detail} The session may already exist. Check the session list before trying again. Updated runtimes reuse this start request; an older runtime may create a duplicate.`;
}
