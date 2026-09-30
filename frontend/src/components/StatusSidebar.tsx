// StatusSidebar — pure render. Data source is useSessionSidebar (which
// reads Claude's persisted JSONL events). Component never touches the
// data layer directly.

import type { SidebarChecklistItem } from '../types/sidebar';

export type { SidebarChecklistItem };

export interface SidebarProps {
  parserName: string;
  parserIcon: string;
  isWorking: boolean;
  timer: string;          // "7m 35s" — empty when idle
  tokens: string;         // "1.9k" — empty when idle
  context: string;        // "32% of 258k" — Codex app-server only
  finalElapsed: string;   // "9m 50s" frozen from the last completed turn, empty if no recent run
  currentTask: string;    // latest tool call name + preview
  checklist: SidebarChecklistItem[];
  statusLabel: string;    // shared lifecycle classifier label when not working
}

export default function StatusSidebar({
  isWorking,
  timer,
  tokens,
  context,
  finalElapsed,
  currentTask,
  checklist,
  statusLabel
}: SidebarProps) {
  return (
    <aside className={`status-sidebar${isWorking ? ' is-working' : ' is-idle'}`}>
      <section className="sidebar-section sidebar-metrics">
        <span className="sidebar-run-state" aria-label={isWorking ? 'Agent working' : statusLabel}>
          <span aria-hidden>✻</span>
          {isWorking ? 'Working' : statusLabel}{!isWorking && finalElapsed ? ` · ${finalElapsed}` : ''}
        </span>
        {isWorking ? (
          <>
            <span className="sidebar-metric"><span className="sidebar-metric-label">elapsed</span>{timer || '—'}</span>
            {tokens ? (
              <span className="sidebar-metric"><span className="sidebar-metric-label">tokens</span>{tokens}</span>
            ) : null}
            {context ? (
              <span className="sidebar-metric"><span className="sidebar-metric-label">context</span>{context}</span>
            ) : null}
          </>
        ) : null}
        {currentTask ? (
          <span className="sidebar-metric sidebar-metric-task" title={currentTask}>
            <span className="sidebar-metric-label">doing</span>{currentTask}
          </span>
        ) : null}
        {!isWorking && context ? (
          <span className="sidebar-metric"><span className="sidebar-metric-label">context</span>{context}</span>
        ) : null}
      </section>

      {checklist.length > 0 ? (
        <section className="sidebar-section sidebar-checklist-section">
          {checklist.map((item, i) => (
            <span key={i} className={'sidebar-checklist-item ' + item.status} title={item.text}>
              <span className="sidebar-checklist-mark">
                {item.status === 'done' ? '✓' : item.status === 'active' ? '◼' : '◻'}
              </span>
              <span className="sidebar-checklist-text">{item.text}</span>
            </span>
          ))}
        </section>
      ) : null}
    </aside>
  );
}
