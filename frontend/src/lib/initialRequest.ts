import { submitMessage } from '../api/sessionsd/operations';
import { deliveryReviewKey, readDeliveryReviews, saveDeliveryReviews } from './deliveryReviewStore';
import { draftStorageKey, readDraft, saveDraft } from './draftStore';

/** Save before leaving the launcher; the conversation owns receipt recovery. */
export function prepareInitialRequest(machineId: string, sessionId: string, text: string, operationId: string): void {
  const draftKey = draftStorageKey(machineId, sessionId);
  const existing = readDraft(draftKey);
  if (existing.warning) throw new Error(existing.warning);
  const draft = saveDraft(draftKey, existing.text || text);
  if (!draft.saved) throw new Error(draft.warning);
  const key = deliveryReviewKey(machineId, sessionId);
  const previous = readDeliveryReviews(key);
  if (previous.warning) throw new Error(previous.warning);
  const warning = saveDeliveryReviews(key, [...previous.reviews.filter((review) => review.operationId !== operationId),
    { operationId, text, status: 'sending', queued: false, firstRequest: true }]);
  if (warning) throw new Error(warning);
}

/** Exactly one attempt. Even an unreadable response leaves its receipt to check. */
export async function deliverInitialRequest(machineId: string, sessionId: string, text: string, operationId: string): Promise<void> {
  let status: 'accepted' | 'unknown' = 'unknown';
  try {
    await submitMessage(sessionId, `\x1b[200~${text}\x1b[201~`, machineId, undefined, undefined, operationId);
    status = 'accepted';
  }
  catch { /* The durable receipt, not this response, determines whether resending is safe. */ }
  const key = deliveryReviewKey(machineId, sessionId);
  const previous = readDeliveryReviews(key);
  // A read-only receipt check settles the draft and its optimistic history
  // together. Never clear edits from the launch callback.
  if (previous.warning) return;
  saveDeliveryReviews(key, previous.reviews.map((review) => review.operationId === operationId
    ? { ...review, status } : review));
}
