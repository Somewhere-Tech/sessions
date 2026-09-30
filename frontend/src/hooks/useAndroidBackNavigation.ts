import { useEffect, useRef } from 'react';
import { registerAndroidBackHandler } from '../lib/tauriBridge';

// App navigation is state rather than browser history. Keep the current layer
// decision behind one stable native registration. A visible More sheet is the
// only pushed layer owned below App, so close it before consulting App state.
export function useAndroidBackNavigation(handleBack: () => boolean): void {
  const handlerRef = useRef(handleBack);
  handlerRef.current = handleBack;

  useEffect(() => registerAndroidBackHandler(() => {
    const moreClose = document.querySelector<HTMLButtonElement>('.mobile-more-heading button');
    if (moreClose) { moreClose.click(); return true; }
    return handlerRef.current();
  }), []);
}
