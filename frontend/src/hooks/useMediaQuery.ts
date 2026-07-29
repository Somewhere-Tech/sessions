import { useEffect, useState } from 'react';

// Reactive media-query hook. Same shape as @react-hook/media-query and
// sessions-tmux's hook. Returns the live match state and re-renders when
// the query starts/stops matching (window resize, orientation change,
// dark-mode toggle, etc.).
//
// The CSS-only @media path still owns visual layout — this is for
// JS-side branching (e.g. mounting different components, sending
// different snapshot widths, or skipping desktop-only setup work) so
// we don't pay for desktop machinery on phones.
export function useMediaQuery(query: string): boolean {
  const [matches, setMatches] = useState<boolean>(() => {
    if (typeof window === 'undefined' || !window.matchMedia) return false;
    return window.matchMedia(query).matches;
  });

  useEffect(() => {
    if (typeof window === 'undefined' || !window.matchMedia) return;
    const mql = window.matchMedia(query);
    const onChange = (e: MediaQueryListEvent): void => setMatches(e.matches);
    // Sync once in case `query` changed.
    setMatches(mql.matches);
    if (mql.addEventListener) mql.addEventListener('change', onChange);
    else mql.addListener(onChange); // Safari < 14 fallback
    return () => {
      if (mql.removeEventListener) mql.removeEventListener('change', onChange);
      else mql.removeListener(onChange);
    };
  }, [query]);

  return matches;
}

// Convenience: matches the 720px breakpoint sessions's CSS already
// uses for mobile layout. Use this in components that need to make
// JS-side decisions ("am I on a phone right now?") rather than just
// styling differences.
export function useIsMobile(): boolean {
  return useMediaQuery('(max-width: 720px)');
}
