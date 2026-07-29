import { useState } from 'react';
import { copyText } from '../lib/copyText';

interface Props {
  // Either the raw text to copy, OR a function that returns it (for
  // cases where the text is computed on click — e.g. concatenating
  // streaming sub-blocks).
  getText: string | (() => string);
  className?: string;
  label?: string;
  iconOnly?: boolean;
}

// Tiny "Copy" button with built-in flash-to-Copied feedback. No toasts,
// no global state — the button itself shows the success/fail state for
// 1.4s, then reverts. Matches what sessions-tmux had on MessageBlock /
// CommandBlock / FileCard.
export function CopyButton({ getText, className, label = 'Copy', iconOnly = false }: Props): JSX.Element {
  const [state, setState] = useState<'idle' | 'ok' | 'err'>('idle');

  const onClick = async (e: React.MouseEvent): Promise<void> => {
    e.stopPropagation();
    const text = typeof getText === 'function' ? getText() : getText;
    const ok = await copyText(text);
    setState(ok ? 'ok' : 'err');
    setTimeout(() => setState('idle'), 1400);
  };

  return (
    <button
      type="button"
      className={`copy-btn ${state}${iconOnly ? ' is-icon-only' : ''} ${className ?? ''}`}
      onClick={onClick}
      aria-label={state === 'ok' ? 'Copied' : state === 'err' ? 'Copy failed' : label}
      title={state === 'ok' ? 'Copied' : state === 'err' ? 'Copy failed' : label}
    >
      {iconOnly ? (
        state === 'ok' ? <span aria-hidden>✓</span> : state === 'err' ? <span aria-hidden>×</span> : (
          <svg viewBox="0 0 20 20" aria-hidden>
            <rect x="6.25" y="6.25" width="9" height="9" rx="1.75" />
            <path d="M4.25 12.75H4A1.75 1.75 0 0 1 2.25 11V4A1.75 1.75 0 0 1 4 2.25h7A1.75 1.75 0 0 1 12.75 4v.25" />
          </svg>
        )
      ) : state === 'ok' ? '✓ Copied' : state === 'err' ? '× Failed' : label}
    </button>
  );
}
