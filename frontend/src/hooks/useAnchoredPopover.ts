import { useCallback, useEffect, useLayoutEffect, useState, type CSSProperties, type RefObject } from 'react';

interface Args {
  open: boolean;
  rootRef: RefObject<HTMLElement>;
  popoverRef: RefObject<HTMLElement>;
  focusRef: RefObject<HTMLElement>;
  layoutKey: string;
  onClose: () => void;
}

export function useAnchoredPopover({ open, rootRef, popoverRef, focusRef, layoutKey, onClose }: Args): CSSProperties | null {
  const [style, setStyle] = useState<CSSProperties | null>(null);
  const position = useCallback((): void => {
    const trigger = rootRef.current?.querySelector<HTMLElement>('.model-picker-trigger');
    if (!trigger) return;
    const rect = trigger.getBoundingClientRect();
    const gap = 8;
    const margin = Math.min(12, window.innerWidth / 2, window.innerHeight / 2);
    const width = Math.max(0, Math.min(380, window.innerWidth - margin * 2));
    const measuredHeight = popoverRef.current?.getBoundingClientRect().height ?? 480;
    const roomAbove = Math.max(0, rect.top - margin - gap);
    const roomBelow = Math.max(0, window.innerHeight - rect.bottom - margin - gap);
    const openAbove = roomAbove >= Math.min(measuredHeight, 320) || roomAbove > roomBelow;
    const maxHeight = Math.min(520, openAbove ? roomAbove : roomBelow);
    const renderedHeight = Math.min(measuredHeight, maxHeight);
    const left = Math.max(margin, Math.min(rect.right - width, window.innerWidth - width - margin));
    const top = openAbove ? Math.max(margin, rect.top - gap - renderedHeight) : rect.bottom + gap;
    setStyle({ position: 'fixed', top, left, right: 'auto', bottom: 'auto', width, maxHeight });
  }, [popoverRef, rootRef]);

  useLayoutEffect(() => {
    if (open) position();
  }, [layoutKey, open, position]);

  useEffect(() => {
    if (!open) return;
    position();
    const focus = window.setTimeout(() => { position(); focusRef.current?.focus(); }, 0);
    const closeOutside = (event: PointerEvent): void => {
      const target = event.target as Node;
      if (!rootRef.current?.contains(target) && !popoverRef.current?.contains(target)) onClose();
    };
    const containEscape = (event: KeyboardEvent): void => {
      if (event.key !== 'Escape') return;
      event.preventDefault();
      event.stopPropagation();
      onClose();
    };
    const observer = new ResizeObserver(position);
    if (popoverRef.current) observer.observe(popoverRef.current);
    window.addEventListener('pointerdown', closeOutside);
    window.addEventListener('keydown', containEscape, true);
    window.addEventListener('resize', position);
    window.addEventListener('scroll', position, true);
    return () => {
      window.clearTimeout(focus);
      observer.disconnect();
      window.removeEventListener('pointerdown', closeOutside);
      window.removeEventListener('keydown', containEscape, true);
      window.removeEventListener('resize', position);
      window.removeEventListener('scroll', position, true);
    };
  }, [focusRef, onClose, open, popoverRef, position, rootRef]);

  return style;
}
