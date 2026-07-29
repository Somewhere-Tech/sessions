import { useEffect, useRef, useState } from 'react';
import type { SessionInfo } from '../types';
import { ParserIcon } from './ParserIcon';
import { getTabLabel, setTabLabel, sessionLabel } from '../lib/tabLabels';

export type TabStatus = 'working' | 'finished' | 'idle';

interface Props {
  sessions: SessionInfo[];
  activeId: string | null;
  // Per-session working/finished/idle. Currently only the active session
  // gets a real status (from its own parser); inactive tabs stay 'idle'
  // until we add background polling. Map keyed by session id.
  statusBySession: Record<string, TabStatus>;
  iconBySession: Record<string, string>;
  onSwitch: (id: string) => void;
  onAdd: () => void;
  onClose: (id: string) => void | Promise<void>;
  // Drag-and-drop reorder: move tab `fromId` to land immediately
  // before `toId`. Owner persists the new order.
  onReorder?: (fromId: string, toId: string) => void;
}

// Combined label — user override (set via double-click rename in
// sessions) > Claude's own title > cwd basename. Exported so every
// consumer (tabs, grid cells, pop-out window title) reaches the same
// name. Delegates the base resolution to lib/tabLabels.sessionLabel so
// all callers use one authoritative chain.
export function shortLabel(s: SessionInfo): string {
  return getTabLabel(s.id) ?? sessionLabel(s);
}

export function SessionTabs({
  sessions,
  activeId,
  statusBySession,
  iconBySession,
  onSwitch,
  onAdd,
  onClose,
  onReorder
}: Props): JSX.Element {
  // Auto-scroll the active tab into view when it changes (e.g. via swipe).
  const stripRef = useRef<HTMLDivElement>(null);
  const [overflow, setOverflow] = useState({ left: false, right: false });
  // Drag-and-drop reorder state. dragId is the session being dragged;
  // overId is the tab currently hovered as a drop target. Both clear
  // on drop / dragend so the visual indicators disappear.
  const [dragId, setDragId] = useState<string | null>(null);
  const [overId, setOverId] = useState<string | null>(null);
  // Inline rename. editingId is the session whose label is being
  // edited; null means no tab is in edit mode. editingValue holds
  // the typed-but-not-committed string.
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editingValue, setEditingValue] = useState('');
  const startRename = (s: SessionInfo): void => {
    setEditingId(s.id);
    setEditingValue(shortLabel(s));
  };
  const commitRename = (): void => {
    if (editingId) {
      const s = sessions.find((x) => x.id === editingId);
      setTabLabel(editingId, editingValue, s?.cwd);
    }
    setEditingId(null);
  };
  const cancelRename = (): void => setEditingId(null);

  useEffect(() => {
    if (!activeId) return;
    const strip = stripRef.current;
    if (!strip) return;
    const el = strip.querySelector<HTMLElement>(`[data-tab-id="${activeId}"]`);
    if (el) {
      el.scrollIntoView({ behavior: 'smooth', block: 'nearest', inline: 'nearest' });
    }
  }, [activeId]);

  const updateOverflow = (): void => {
    const strip = stripRef.current;
    if (!strip) return;
    setOverflow({
      left: strip.scrollLeft > 4,
      right: strip.scrollLeft + strip.clientWidth < strip.scrollWidth - 4
    });
  };
  useEffect(() => {
    updateOverflow();
    const strip = stripRef.current;
    if (!strip) return;
    strip.addEventListener('scroll', updateOverflow, { passive: true });
    window.addEventListener('resize', updateOverflow);
    return () => {
      strip.removeEventListener('scroll', updateOverflow);
      window.removeEventListener('resize', updateOverflow);
    };
  }, [sessions.length]);

  return (
    <div className={`tabs${overflow.left ? ' has-overflow-left' : ''}${overflow.right ? ' has-overflow-right' : ''}`}>
      <div className="tabs-strip" ref={stripRef} role="tablist">
        {sessions.length === 0 ? (
          <span className="tabs-empty">no sessions</span>
        ) : (
          sessions.map((s) => {
            const status = statusBySession[s.id] ?? 'idle';
            const icon = iconBySession[s.id] ?? '⬛';
            const isActive = s.id === activeId;
            const isDragging = dragId === s.id;
            const isDropTarget = overId === s.id && dragId !== null && dragId !== s.id;
            return (
              <div
                key={s.id}
                role="tab"
                aria-selected={isActive}
                tabIndex={0}
                data-tab-id={s.id}
                draggable={!!onReorder}
                className={`tab${isActive ? ' is-active' : ''}${isDragging ? ' is-dragging' : ''}${isDropTarget ? ' is-drop-target' : ''}`}
                onClick={() => onSwitch(s.id)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' || e.key === ' ') {
                    e.preventDefault();
                    onSwitch(s.id);
                  }
                }}
                onDragStart={(e) => {
                  if (!onReorder) return;
                  setDragId(s.id);
                  e.dataTransfer.effectAllowed = 'move';
                  // Some browsers require dataTransfer to have some
                  // data set for the drag to proceed at all.
                  try { e.dataTransfer.setData('text/plain', s.id); } catch { /* ignore */ }
                }}
                onDragOver={(e) => {
                  if (!onReorder || !dragId || dragId === s.id) return;
                  e.preventDefault();
                  e.dataTransfer.dropEffect = 'move';
                  if (overId !== s.id) setOverId(s.id);
                }}
                onDragLeave={(e) => {
                  // Only clear the indicator if the pointer left the
                  // tab entirely (not just hopped to a child element).
                  if (e.currentTarget.contains(e.relatedTarget as Node | null)) return;
                  if (overId === s.id) setOverId(null);
                }}
                onDrop={(e) => {
                  if (!onReorder || !dragId) return;
                  e.preventDefault();
                  if (dragId !== s.id) onReorder(dragId, s.id);
                  setDragId(null);
                  setOverId(null);
                }}
                onDragEnd={() => {
                  setDragId(null);
                  setOverId(null);
                }}
                title={s.cmd + (s.args.length > 0 ? ' ' + s.args.join(' ') : '') + '\ncwd: ' + s.cwd}
              >
                <span className="tab-icon" aria-hidden>
                  <ParserIcon icon={icon} size={18} />
                </span>
                {status !== 'idle' ? (
                  <span className={`tab-status-dot tab-${status}`} aria-hidden />
                ) : null}
                {editingId === s.id ? (
                  <input
                    type="text"
                    className="tab-label tab-label-edit"
                    value={editingValue}
                    autoFocus
                    onChange={(e) => setEditingValue(e.target.value)}
                    onClick={(e) => e.stopPropagation()}
                    onKeyDown={(e) => {
                      e.stopPropagation();
                      if (e.key === 'Enter') commitRename();
                      else if (e.key === 'Escape') cancelRename();
                    }}
                    onBlur={commitRename}
                  />
                ) : (
                  <span
                    className="tab-label"
                    onDoubleClick={(e) => {
                      e.stopPropagation();
                      startRename(s);
                    }}
                    title="Double-click to rename"
                  >
                    {shortLabel(s)}
                  </span>
                )}
                <button
                  type="button"
                  className="tab-close"
                  aria-label={`Close ${shortLabel(s)} tab`}
                  title="Close tab — the session keeps running"
                  onClick={(e) => { e.stopPropagation(); void onClose(s.id); }}
                >
                  ×
                </button>
              </div>
            );
          })
        )}
      </div>
      <button type="button" className="tab-new" aria-label="New session" title="New session" onClick={onAdd}>
        +
      </button>
    </div>
  );
}
