import { useEffect, useRef, useState } from 'react';
import { MessageDeliveryError } from '../lib/messageDelivery';

const PREFIX = 'sessions:delivery-review:v1:';
const MAX_BYTES = 512 * 1024;

export interface DeliveryReview {
  operationId: string;
  status: 'unknown' | 'text-delivered';
  text: string;
  baseline?: number;
  queued: boolean;
}

function read(key: string): { reviews: DeliveryReview[]; warning?: string } {
  try {
    const raw = window.localStorage.getItem(key);
    if (!raw) return { reviews: [] };
    const reviews = JSON.parse(raw) as DeliveryReview[];
    if (!Array.isArray(reviews) || reviews.length > 64 || reviews.some((value) => typeof value.operationId !== 'string' || typeof value.text !== 'string'
      || !['unknown', 'text-delivered'].includes(value.status) || typeof value.queued !== 'boolean'
      || (value.baseline !== undefined && (!Number.isSafeInteger(value.baseline) || value.baseline < 0)))) throw new Error('Invalid reminder');
    return { reviews };
  } catch {
    return { reviews: [], warning: 'Delivery reminders could not be read on this device. Check the conversation before resending a saved draft.' };
  }
}

function save(key: string, reviews: DeliveryReview[]): string | null {
  try {
    const storage = window.localStorage;
    if (!reviews.length) { storage.removeItem(key); return null; }
    const raw = JSON.stringify(reviews);
    let total = new TextEncoder().encode(raw).byteLength, count = 0;
    for (let index = 0; index < storage.length; index += 1) {
      const other = storage.key(index);
      if (other?.startsWith(PREFIX) && other !== key) {
        count += 1;
        total += new TextEncoder().encode(storage.getItem(other) ?? '').byteLength;
      }
    }
    if (count >= 64 || reviews.length > 64 || total > MAX_BYTES) throw new Error('Reminder storage full');
    storage.setItem(key, raw);
    return null;
  } catch {
    return 'This delivery reminder could not be saved locally. Keep this window open and check the conversation before resending after reopening.';
  }
}

/** Device-local review reminder, never a queue: mounting/reconnecting sends nothing. */
export function useDeliveryReview(machineId: string, sessionId: string): {
  reviews: DeliveryReview[]; warning: string | null;
  remember: (error: MessageDeliveryError, text: string, queued: boolean, baseline?: number) => void;
  clear: (operationId: string) => boolean;
} {
  const key = `${PREFIX}${encodeURIComponent(machineId)}:${encodeURIComponent(sessionId)}`;
  const [initial] = useState(() => read(key));
  const [state, setState] = useState({ key, reviews: initial.reviews });
  const stateRef = useRef(state);
  stateRef.current = state;
  const [warning, setWarning] = useState<string | null>(initial.warning ?? null);
  const scope = useRef(key);
  const mounted = useRef(true);
  scope.current = key;
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  useEffect(() => {
    if (state.key === key) return;
    const restored = read(key);
    setState({ key, reviews: restored.reviews });
    setWarning(restored.warning ?? null);
  }, [key, state.key]);
  const remember = (error: MessageDeliveryError, text: string, queued: boolean, baseline?: number): void => {
    if (!mounted.current || scope.current !== key || error.deliveryStatus === 'not-delivered') return;
    const review: DeliveryReview = { operationId: error.operationId, status: error.deliveryStatus, text, queued, baseline };
    const reviews = [...stateRef.current.reviews.filter((item) => item.operationId !== review.operationId), review];
    stateRef.current = { key, reviews };
    setState(stateRef.current);
    setWarning(save(key, reviews));
  };
  const clear = (operationId: string): boolean => {
    if (!mounted.current || scope.current !== key || !stateRef.current.reviews.some((review) => review.operationId === operationId)) return false;
    const reviews = stateRef.current.reviews.filter((review) => review.operationId !== operationId);
    stateRef.current = { key, reviews };
    setState(stateRef.current);
    setWarning(save(key, reviews));
    return true;
  };
  return { reviews: state.key === key ? state.reviews : [], warning, remember, clear };
}
