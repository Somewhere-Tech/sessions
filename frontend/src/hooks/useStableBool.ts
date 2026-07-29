import { useEffect, useRef, useState } from 'react';

// Debounce a boolean signal to prevent rapid flipping in the UI.
// Specifically tuned for the working↔idle transition in sessions's
// status strips: the parser briefly reports `isWorking: false` between
// thinking-active redraws (a few times per second), which previously
// caused the strip to flicker between "processing" and "idle".
//
// Behavior: a transition from `false → true` propagates immediately
// (we want "started working" to feel responsive). A transition from
// `true → false` is held for `falseHoldMs` before propagating — if
// the source flips back to true within that window, the false drop
// is suppressed entirely. So a brief "blink to idle" never reaches
// the consumer.
//
// 1500ms covers Claude's ✻-glyph cycle (typically 500-800ms between
// re-emits) and the parser's 200ms throttle without taking forever
// to acknowledge a real "Cooked for X" state change.
export function useStableBool(value: boolean, falseHoldMs: number = 1500): boolean {
  const [stable, setStable] = useState(value);
  const timerRef = useRef<number | null>(null);
  useEffect(() => {
    if (value) {
      // True wins immediately. Cancel any pending false-flip.
      if (timerRef.current !== null) {
        window.clearTimeout(timerRef.current);
        timerRef.current = null;
      }
      setStable(true);
      return;
    }
    // False — schedule the drop.
    if (timerRef.current !== null) return; // already pending
    timerRef.current = window.setTimeout(() => {
      timerRef.current = null;
      setStable(false);
    }, falseHoldMs);
    return () => {
      if (timerRef.current !== null) {
        window.clearTimeout(timerRef.current);
        timerRef.current = null;
      }
    };
  }, [value, falseHoldMs]);
  return stable;
}
