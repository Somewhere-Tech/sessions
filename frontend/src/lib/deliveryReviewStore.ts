const PREFIX = 'sessions:delivery-review:v1:';
const MAX_BYTES = 512 * 1024;
export const DELIVERY_REVIEW_CHANGED = 'sessions:delivery-review-changed';

export interface DeliveryReview {
  operationId: string;
  status: 'unknown' | 'text-delivered' | 'sending' | 'accepted';
  text: string;
  baseline?: number;
  queued: boolean;
  firstRequest?: boolean;
}

export function deliveryReviewKey(machineId: string, sessionId: string): string {
  return `${PREFIX}${encodeURIComponent(machineId)}:${encodeURIComponent(sessionId)}`;
}

export function readDeliveryReviews(key: string): { reviews: DeliveryReview[]; warning?: string } {
  try {
    const raw = window.localStorage.getItem(key);
    if (!raw) return { reviews: [] };
    const reviews = JSON.parse(raw) as DeliveryReview[];
    if (!Array.isArray(reviews) || reviews.length > 64 || reviews.some((value) => typeof value.operationId !== 'string' || typeof value.text !== 'string'
      || !['unknown', 'text-delivered', 'sending', 'accepted'].includes(value.status) || typeof value.queued !== 'boolean'
      || (value.firstRequest !== undefined && typeof value.firstRequest !== 'boolean')
      || (value.baseline !== undefined && (!Number.isSafeInteger(value.baseline) || value.baseline < 0)))) throw new Error('Invalid reminder');
    return { reviews };
  } catch {
    return { reviews: [], warning: 'Delivery reminders could not be read on this device. Check the conversation before resending a saved draft.' };
  }
}

export function saveDeliveryReviews(key: string, reviews: DeliveryReview[]): string | null {
  try {
    const storage = window.localStorage;
    const raw = JSON.stringify(reviews);
    let total = new TextEncoder().encode(raw).byteLength, count = 0;
    for (let index = 0; index < storage.length; index += 1) {
      const other = storage.key(index);
      if (other?.startsWith(PREFIX) && other !== key) {
        count += 1;
        total += new TextEncoder().encode(storage.getItem(other) ?? '').byteLength;
      }
    }
    if (reviews.length && (count >= 64 || reviews.length > 64 || total > MAX_BYTES)) throw new Error('Reminder storage full');
    if (reviews.length) storage.setItem(key, raw);
    else storage.removeItem(key);
    window.dispatchEvent(new CustomEvent<string>(DELIVERY_REVIEW_CHANGED, { detail: key }));
    return null;
  } catch {
    return 'This delivery reminder could not be saved locally. Keep this window open and check the conversation before resending after reopening.';
  }
}
