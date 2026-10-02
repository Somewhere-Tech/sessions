import { useEffect, useRef, useState } from 'react';
import { MessageDeliveryError } from '../lib/messageDelivery';

import { DELIVERY_REVIEW_CHANGED, deliveryReviewKey, readDeliveryReviews, saveDeliveryReviews, type DeliveryReview } from '../lib/deliveryReviewStore';
export type { DeliveryReview } from '../lib/deliveryReviewStore';

function read(key: string): { reviews: DeliveryReview[]; warning?: string } {
  return readDeliveryReviews(key);
}

function save(key: string, reviews: DeliveryReview[]): string | null {
  return saveDeliveryReviews(key, reviews);
}

/** Device-local review reminder, never a queue: mounting/reconnecting sends nothing. */
export function useDeliveryReview(machineId: string, sessionId: string): {
  reviews: DeliveryReview[]; warning: string | null;
  remember: (error: MessageDeliveryError, text: string, queued: boolean, baseline?: number) => void;
  clear: (operationId: string) => boolean;
} {
  const key = deliveryReviewKey(machineId, sessionId);
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
    const restore = (): void => {
      const restored = read(key);
      stateRef.current = { key, reviews: restored.reviews };
      setState(stateRef.current);
      setWarning(restored.warning ?? null);
    };
    restore();
    const changed = (event: Event): void => { if ((event as CustomEvent<string>).detail === key) restore(); };
    window.addEventListener(DELIVERY_REVIEW_CHANGED, changed);
    return () => window.removeEventListener(DELIVERY_REVIEW_CHANGED, changed);
  }, [key]);
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
