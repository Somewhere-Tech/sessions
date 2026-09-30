import { useEffect, useRef, type ReactNode } from 'react';
import '../styles/conversation-header.css';

/** Secondary controls stay available without competing with the conversation. */
export function SessionActionsMenu({ children }: { children: ReactNode }): JSX.Element {
  const ref = useRef<HTMLDetailsElement>(null);
  useEffect(() => {
    const close = (event: PointerEvent): void => {
      if (ref.current && !ref.current.contains(event.target as Node)
        && !(event.target as Element).closest('[role="dialog"]')) ref.current.open = false;
    };
    document.addEventListener('pointerdown', close);
    return () => document.removeEventListener('pointerdown', close);
  }, []);
  return <details ref={ref} className="session-actions-menu" onKeyDown={(event) => {
    if (event.key === 'Escape' && ref.current?.open && !(event.target as Element).closest('[role="dialog"]')) {
      event.preventDefault(); event.stopPropagation(); ref.current.open = false;
      ref.current.querySelector('summary')?.focus();
    }
  }}>
    <summary aria-label="Conversation actions" title="Conversation actions"><span aria-hidden="true">•••</span></summary>
    <div className="session-actions-popover" role="group" aria-label="Conversation actions">{children}</div>
  </details>;
}
