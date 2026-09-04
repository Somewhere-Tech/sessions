import { AuthError } from '../api/sessionsd/core';
import { ServerCompatibilityError, fetchServerHealth } from '../api/sessionsd/sessions';
import {
  adoptCurrentOriginServer,
  currentOriginBootstrapCandidate,
  useServers
} from './servers';

// A non-8787 page may still be the UI served by sessionsd itself (for example
// its Tailscale HTTPS origin). Keep the API client outside servers.ts so the
// startup probe does not create a servers -> API -> servers module cycle.
export async function bootstrapCurrentOriginServer(): Promise<void> {
  const candidate = currentOriginBootstrapCandidate();
  if (!candidate) return;

  let tokenRequired = false;
  try {
    await fetchServerHealth(candidate);
  } catch (error) {
    if (error instanceof AuthError) {
      // A daemon that answers 401 has identified itself well enough to adopt;
      // the token prompt is the next step, not a dead end.
      tokenRequired = true;
    } else if (error instanceof ServerCompatibilityError) {
      // Reachable, definitely sessionsd, and unusable by this client. Adopting
      // it would hide that; say so on the connect surface instead.
      useServers.getState().setPairingError(error.message);
      return;
    } else {
      // Unreachable, not a daemon, or an unrecognisable body: a static hosted
      // shell. Fall through silently exactly as before.
      return;
    }
  }

  await adoptCurrentOriginServer(undefined, tokenRequired);
}
