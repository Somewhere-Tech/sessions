import {
  adoptConversation,
  repairAdoption,
  type AdoptConversationResult,
  type AdoptRepairRequest
} from '../api/sessionsd';

// Adopt-then-repair, once, for every entry point.
//
// This sequence was implemented twice and the two copies answered "did my
// history annotations finish?" differently. App.tsx auto-repaired and dropped
// a failed repair into console.warn, so continuing a conversation from Fleet
// or Search reported success while its provenance records were incomplete.
// ResumeDialog did not auto-repair at all: it surfaced the partial result and
// asked the user to press a button. Same daemon contract, two answers,
// decided by which surface the user happened to start from.
//
// One behaviour now: try the repair automatically — it is record-only and
// never creates a second runtime — and if anything is still missing
// afterwards, say so. docs/PRINCIPLES.md, "cleanup must never hide an
// unresolved decision": a silent annotation failure is exactly the kind of
// unresolved state that later makes a session hard to find.

export interface AdoptOutcome {
  /** The most complete result we obtained — the repair's, if one succeeded. */
  result: AdoptConversationResult;
  /** Set only when a repair was attempted and rejected. */
  repairError: string | null;
  /** True while the history annotations are still known to be incomplete. */
  unresolved: boolean;
  /** Present when another repair attempt is still possible. */
  repair: AdoptRepairRequest | null;
}

function outcome(result: AdoptConversationResult, repairError: string | null): AdoptOutcome {
  const unresolved = Boolean(result.partial);
  return {
    result,
    repairError,
    unresolved,
    repair: unresolved ? result.repair ?? null : null
  };
}

/**
 * Runs one more repair pass over an already-partial adoption — the manual
 * retry behind ResumeDialog's "Repair records" button. Rejections propagate;
 * the caller is a surface that shows them.
 */
export async function runAdoptionRepair(request: AdoptRepairRequest): Promise<AdoptOutcome> {
  return outcome(await repairAdoption(request), null);
}

/**
 * Adopts a provider conversation and, when the daemon reports the adoption as
 * partial with a repair token, immediately attempts that repair once. The
 * live successor created by the first call is never duplicated: repair is
 * record-only. A repair that fails is reported, not swallowed.
 */
export async function adoptConversationWithRepair(
  ...args: Parameters<typeof adoptConversation>
): Promise<AdoptOutcome> {
  const first = await adoptConversation(...args);
  if (!first.partial || !first.repair) return outcome(first, null);
  try {
    return outcome(await repairAdoption(first.repair), null);
  } catch (reason) {
    // Keep the first result: it holds the live lane the user must be taken
    // to. Only the annotations are in doubt, and the caller must show that.
    return outcome(first, reason instanceof Error ? reason.message : String(reason));
  }
}

/** The one sentence every surface uses for an unfinished adoption. */
export function adoptionWarning(result: AdoptOutcome): string | null {
  if (!result.unresolved && !result.repairError) return null;
  const missing = result.result.missingAnnotations?.length
    ? ` Missing: ${result.result.missingAnnotations.join(', ')}.`
    : '';
  const detail = result.result.warning?.trim()
    || 'The conversation is live, but Sessions could not finish recording where it came from.';
  const repair = result.repairError ? ` The automatic repair failed: ${result.repairError}` : '';
  return `${detail}${missing}${repair}`;
}
