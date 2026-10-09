import type { SessionInfo, StartReceipt } from '../types';

// The daemon's start receipt (SessionInfo.start) says how far delegated work
// provably got. This turns it into the one header line a session shows.

export type StartNoticeTone = 'progress' | 'attention';

export interface StartNotice {
  tone: StartNoticeTone;
  text: string;
  /** Present only when connecting an account is the safe next step. */
  connectAccount?: boolean;
}

/**
 * The one line a session header shows about its delegated start, or null when
 * the start needs no comment (working, completed, or no receipt at all).
 * Blocked-on-provider states other than authentication are already shown by
 * the provider fault card, so they are not repeated here.
 */
export function startNotice(session: Pick<SessionInfo, 'start' | 'exited' | 'launching'>): StartNotice | null {
  const start: StartReceipt | undefined = session.start;
  if (!start) return null;
  switch (start.phase) {
    case 'created':
      if (session.launching) return { tone: 'progress', text: 'Starting this session… Wait for runner readiness before sending' };
      if (start.prompt?.status === 'sending') return { tone: 'progress', text: 'Delivering the first request…' };
      if (start.prompt?.status === 'not-sent') return { tone: 'attention', text: `First request not sent · ${session.exited ? 'Start a new session' : 'Send it from the composer'}` };
      return null;
    case 'prompt-not-delivered':
      return start.prompt?.retry
        ? { tone: 'attention', text: 'First request did not reach the provider · Nothing was sent, so sending it again is safe' }
        : { tone: 'attention', text: 'First request was refused · Check the conversation before sending it again' };
    case 'prompt-unknown':
      return { tone: 'attention', text: 'First request may not have arrived · Sessions did not resend it; check the conversation first' };
    case 'prompt-delivered':
      return { tone: 'progress', text: 'First request delivered · Waiting for the provider to start' };
    case 'blocked':
      return start.blocked_by === 'auth'
        ? { tone: 'attention', text: 'Blocked: the provider account is not signed in', connectAccount: true }
        : null;
    default:
      return null;
  }
}
