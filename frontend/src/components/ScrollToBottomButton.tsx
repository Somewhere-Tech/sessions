interface Props {
  visible: boolean;
  onClick: () => void;
  // Position pin — Terminal pane tucks it bottom-right, Sessions pane wants
  // it slightly higher to clear the InputBar.
  bottom?: number;
}

// Floating circle with a down-arrow that pops in whenever the pane has
// been scrolled up away from the bottom. Click → snap to latest. Lives
// `position: absolute` inside whichever pane wraps it; the parent must
// be `position: relative`.
export function ScrollToBottomButton({ visible, onClick, bottom }: Props): JSX.Element | null {
  if (!visible) return null;
  return (
    <button
      type="button"
      className="scroll-to-bottom"
      onClick={onClick}
      title="Scroll to latest"
      aria-label="Scroll to latest"
      style={bottom !== undefined ? { bottom: `${bottom}px` } : undefined}
    >
      <svg
        width="16"
        height="16"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="2.5"
        strokeLinecap="round"
        strokeLinejoin="round"
      >
        <polyline points="6 9 12 15 18 9" />
      </svg>
    </button>
  );
}
