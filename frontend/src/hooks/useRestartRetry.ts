import { useEffect, useRef, useState } from 'react';
import { RESTART_RETRY_INTERVAL_MS, RESTART_RETRY_WINDOW_MS } from '../lib/fleetPeerBudget';

/**
 * Keeps asking a machine that is restarting, and reports whether it is still
 * inside the window where "restarting" is a fair description of it.
 *
 * The clock starts when the first refusal arrives and is dropped the moment the
 * machine answers, so a machine that restarts twice gets its window twice. Past
 * the window the caller stops calling it a restart: a machine that has refused
 * for a minute is a machine that is not running.
 *
 * `attempt` is whatever value changes when an attempt finishes — a new value
 * arms the next one, so the asking is paced by answers rather than by wall
 * clock alone.
 */
export function useRestartRetry(restarting: boolean, attempt: unknown, askAgain: () => void): boolean {
  const [since, setSince] = useState<number | null>(null);
  const ask = useRef(askAgain);
  ask.current = askAgain;

  useEffect(() => {
    if (!restarting) {
      if (since !== null) setSince(null);
      return;
    }
    if (since === null) {
      setSince(Date.now());
      return;
    }
    const remaining = RESTART_RETRY_WINDOW_MS - (Date.now() - since);
    if (remaining <= 0) return;
    const timer = window.setTimeout(() => ask.current(), Math.min(RESTART_RETRY_INTERVAL_MS, remaining));
    return () => window.clearTimeout(timer);
  }, [restarting, since, attempt]);

  return restarting && since !== null && Date.now() - since < RESTART_RETRY_WINDOW_MS;
}
