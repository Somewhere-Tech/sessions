// A bounded signal built from parts every shipped WebView has.
//
// AbortSignal.any needs WebKit 17.4 — newer than the iOS this app is built
// against — and AbortSignal.timeout needs WebKit 16. A phone whose WebView
// lacks them throws where a read starts, so instead of one request being slow
// the surface around it fails. A controller, one listener and one timer are the
// same thing and run everywhere.

export interface AbortBudget {
  /** Aborts when the caller aborts, or when this budget elapses. */
  readonly signal: AbortSignal;
  /** Stop the clock. Idempotent; call it once the read is over either way. */
  release(): void;
}

/**
 * `budgetMS` is how long the caller is willing to wait. `parent`, when given,
 * ends the read early with its own reason, so a caller that already gave up is
 * not overwritten by a timeout that had not happened.
 *
 * The budget aborts with a `TimeoutError`, which is what AbortSignal.timeout
 * raises and what callers already distinguish from an ordinary abort.
 */
export function abortBudget(budgetMS: number, parent?: AbortSignal): AbortBudget {
  const controller = new AbortController();
  let timer: ReturnType<typeof setTimeout> | undefined;
  function release(): void {
    if (timer !== undefined) {
      clearTimeout(timer);
      timer = undefined;
    }
    parent?.removeEventListener('abort', onCallerAbort);
  }
  function onCallerAbort(): void {
    release();
    controller.abort(parent?.reason);
  }
  if (parent?.aborted) {
    controller.abort(parent.reason);
    return { signal: controller.signal, release };
  }
  parent?.addEventListener('abort', onCallerAbort);
  timer = setTimeout(() => {
    timer = undefined;
    release();
    controller.abort(new DOMException('The request did not finish inside its budget.', 'TimeoutError'));
  }, budgetMS);
  return { signal: controller.signal, release };
}
