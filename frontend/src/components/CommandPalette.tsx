import { useEffect, useMemo, useRef, useState } from 'react';
import type { SessionInfo } from '../types';
import { resolvedSessionLabel } from '../lib/tabLabels';
import { classifySession } from '../lib/sessionStatus';
import { normalizeProvider, ProviderMark } from './ProviderBadge';
import type { ProductView } from './ProductSidebar';

interface Props {
  open: boolean;
  sessions: SessionInfo[];
  onClose: () => void;
  onNavigate: (view: ProductView) => void;
  onNewSession: () => void;
  onContinue: () => void;
  onOpenSession: (sessionId: string) => void;
}

interface PaletteAction {
  id: string;
  label: string;
  detail: string;
  group: 'Actions' | 'Go to' | 'Sessions';
  keywords: string;
  icon?: JSX.Element;
  run: () => void;
}

const VIEW_ACTIONS: Array<{ view: ProductView; label: string; detail: string; keywords: string }> = [
  { view: 'home', label: 'Home', detail: 'Agent operations overview', keywords: 'dashboard overview' },
  { view: 'tabs', label: 'Sessions', detail: 'Running and recently ended work', keywords: 'agents conversations lanes' },
  { view: 'today', label: 'Daily', detail: 'Today’s work and saved recaps', keywords: 'journal recap today' },
  { view: 'search', label: 'Conversation history', detail: 'Browse or search every Claude and Codex conversation', keywords: 'resume recall history search find codex claude' },
  { view: 'fleet', label: 'Fleet', detail: 'Machines and sessions across your network', keywords: 'computers mac windows linux' },
  { view: 'usage', label: 'Usage', detail: 'Tokens, cost, projects, and tags', keywords: 'budget tokens cost' },
  { view: 'settings', label: 'Settings', detail: 'Agents, connections, updates, and appearance', keywords: 'preferences configuration' },
  { view: 'feedback', label: 'Send feedback', detail: 'Share an idea or report a problem', keywords: 'support bug issue' }
];

function sessionSearchText(session: SessionInfo): string {
  return [
    resolvedSessionLabel(session),
    session.name,
    session.description,
    session.claudeCustomTitle,
    session.claudeAiTitle,
    session.cwd,
    session.tool,
    session.profile,
    session.model,
    session.lastSummary
  ].filter(Boolean).join(' ').toLowerCase();
}

export function CommandPalette({
  open,
  sessions,
  onClose,
  onNavigate,
  onNewSession,
  onContinue,
  onOpenSession
}: Props): JSX.Element | null {
  const [query, setQuery] = useState('');
  const [activeIndex, setActiveIndex] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const dialogRef = useRef<HTMLElement>(null);

  const actions = useMemo<PaletteAction[]>(() => {
    const closeThen = (action: () => void): (() => void) => () => {
      onClose();
      action();
    };
    return [
      {
        id: 'new',
        label: 'New Session',
        detail: 'Claude, Codex, or Shell · ⌘N',
        group: 'Actions',
        keywords: 'start create agent',
        run: closeThen(onNewSession)
      },
      {
        id: 'continue',
        label: 'Resume a conversation',
        detail: 'Search provider history and resume the exact conversation',
        group: 'Actions',
        keywords: 'resume reopen claude codex history',
        run: closeThen(onContinue)
      },
      ...VIEW_ACTIONS.map((item): PaletteAction => ({
        id: `view:${item.view}`,
        label: item.label,
        detail: item.detail,
        group: 'Go to',
        keywords: item.keywords,
        run: closeThen(() => onNavigate(item.view))
      })),
      ...sessions.slice(0, 160).map((session): PaletteAction => {
        const provider = normalizeProvider(session.tool);
        const status = classifySession(session).label;
        return {
          id: `session:${session.id}`,
          label: resolvedSessionLabel(session),
          detail: `${status} · ${session.cwd || session.id.slice(0, 8)}`,
          group: 'Sessions',
          keywords: sessionSearchText(session),
          icon: provider ? <ProviderMark provider={provider} size={18} /> : <span className="command-palette-shell" aria-hidden>$</span>,
          run: closeThen(() => onOpenSession(session.id))
        };
      })
    ];
  }, [onClose, onContinue, onNavigate, onNewSession, onOpenSession, sessions]);

  const visibleActions = useMemo(() => {
    const tokens = query.trim().toLowerCase().split(/\s+/).filter(Boolean);
    if (tokens.length === 0) return actions.slice(0, 30);
    return actions.filter((action) => {
      const haystack = `${action.label} ${action.detail} ${action.keywords}`.toLowerCase();
      return tokens.every((token) => haystack.includes(token));
    }).slice(0, 40);
  }, [actions, query]);

  useEffect(() => {
    if (!open) return;
    setQuery('');
    setActiveIndex(0);
    const focus = window.setTimeout(() => inputRef.current?.focus(), 0);
    return () => window.clearTimeout(focus);
  }, [open]);

  useEffect(() => {
    setActiveIndex((current) => Math.min(current, Math.max(0, visibleActions.length - 1)));
  }, [visibleActions.length]);

  if (!open) return null;
  return (
    <div className="command-palette-backdrop" role="presentation" onPointerDown={onClose}>
      <section
        ref={dialogRef}
        className="command-palette"
        role="dialog"
        aria-modal="true"
        aria-label="Sessions commands"
        onPointerDown={(event) => event.stopPropagation()}
        onKeyDown={(event) => {
          if (event.key === 'Escape') {
            event.preventDefault();
            onClose();
          } else if (event.key === 'ArrowDown') {
            event.preventDefault();
            setActiveIndex((current) => Math.min(current + 1, visibleActions.length - 1));
          } else if (event.key === 'ArrowUp') {
            event.preventDefault();
            setActiveIndex((current) => Math.max(0, current - 1));
          } else if (event.key === 'Enter' && visibleActions[activeIndex]) {
            event.preventDefault();
            visibleActions[activeIndex].run();
          } else if (event.key === 'Tab') {
            const focusable = Array.from(dialogRef.current?.querySelectorAll<HTMLElement>(
              'input:not([disabled]), button:not([disabled]), [tabindex]:not([tabindex="-1"])'
            ) ?? []);
            if (focusable.length === 0) return;
            const first = focusable[0];
            const last = focusable[focusable.length - 1];
            if (event.shiftKey && document.activeElement === first) {
              event.preventDefault();
              last?.focus();
            } else if (!event.shiftKey && document.activeElement === last) {
              event.preventDefault();
              first?.focus();
            }
          }
        }}
      >
        <div className="command-palette-input">
          <span aria-hidden>⌕</span>
          <input
            ref={inputRef}
            value={query}
            onChange={(event) => {
              setQuery(event.currentTarget.value);
              setActiveIndex(0);
            }}
            placeholder="Find a session or action…"
            aria-label="Find a session or action"
            role="combobox"
            aria-autocomplete="list"
            aria-expanded="true"
            aria-controls="sessions-command-palette-results"
            aria-activedescendant={visibleActions[activeIndex] ? `sessions-command-palette-option-${activeIndex}` : undefined}
          />
          <kbd>esc</kbd>
        </div>
        <div id="sessions-command-palette-results" className="command-palette-results" role="listbox">
          {visibleActions.map((action, index) => {
            const previous = visibleActions[index - 1];
            const showGroup = !previous || previous.group !== action.group;
            return (
              <div key={action.id} className="command-palette-result-wrap">
                {showGroup ? <span className="command-palette-group">{action.group}</span> : null}
                <button
                  id={`sessions-command-palette-option-${index}`}
                  type="button"
                  role="option"
                  aria-selected={index === activeIndex}
                  className={`command-palette-result${index === activeIndex ? ' is-active' : ''}`}
                  onMouseEnter={() => setActiveIndex(index)}
                  onClick={action.run}
                >
                  <span className="command-palette-result-icon">{action.icon ?? <span aria-hidden>→</span>}</span>
                  <span><strong>{action.label}</strong><small>{action.detail}</small></span>
                  {action.group === 'Sessions' ? <span className="command-palette-open">Open</span> : null}
                </button>
              </div>
            );
          })}
          {visibleActions.length === 0 ? (
            <div className="command-palette-empty">No matching session or action</div>
          ) : null}
        </div>
        <footer><span>↑↓ navigate</span><span>↵ open</span><span>⌘K close</span></footer>
      </section>
    </div>
  );
}
