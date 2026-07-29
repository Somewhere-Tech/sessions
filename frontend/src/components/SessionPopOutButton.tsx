import { openSessionWindow } from '../lib/tauriBridge';

interface Props {
  sessionId: string;
  label: string;
}

export function SessionPopOutButton({ sessionId, label }: Props): JSX.Element | null {
  const alreadyPoppedOut = typeof window !== 'undefined'
    && new URLSearchParams(window.location.search).get('mode') === 'single';
  if (alreadyPoppedOut) return null;

  return (
    <button
      type="button"
      className="btn btn-ghost session-popout-view"
      aria-label={`Pop out ${label} into its own window`}
      title="Open this session in its own window"
      onClick={() => void openSessionWindow(sessionId, label)}
    >
      <svg viewBox="0 0 18 18" aria-hidden>
        <path d="M7.25 3.25H3.9A1.65 1.65 0 0 0 2.25 4.9v9.2a1.65 1.65 0 0 0 1.65 1.65h9.2a1.65 1.65 0 0 0 1.65-1.65v-3.35" />
        <path d="M10.25 2.25h5.5v5.5M15.5 2.5 8.25 9.75" />
      </svg>
      <span>Pop out</span>
    </button>
  );
}
