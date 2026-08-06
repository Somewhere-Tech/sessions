import { useEffect, useRef, useState, type ReactNode } from 'react';

type MobileLayout = 'home' | 'tabs' | 'today' | 'fleet' | 'search' | 'usage' | 'settings' | 'connections';

interface Props {
  layoutMode: MobileLayout;
  showingSessionDetail: boolean;
  onLayoutChange: (mode: MobileLayout) => void;
  onShowSessions: () => void;
}

/**
 * Phone navigation deliberately mirrors a consumer app instead of shrinking
 * the desktop toolbar. Sessions is a real destination (the hierarchy), and a
 * selected conversation is pushed on top of it by App. Less-frequent
 * destinations live behind More so the five primary tap targets stay large.
 */
export function MobileNav({
  layoutMode,
  showingSessionDetail,
  onLayoutChange,
  onShowSessions
}: Props): JSX.Element {
  const [moreOpen, setMoreOpen] = useState(false);
  const sheetRef = useRef<HTMLDivElement>(null);
  const moreActive = layoutMode === 'fleet'
    || layoutMode === 'usage'
    || layoutMode === 'settings'
    || layoutMode === 'connections';

  useEffect(() => {
    if (!moreOpen) return;
    window.requestAnimationFrame(() => sheetRef.current?.focus());
  }, [moreOpen]);

  const go = (mode: MobileLayout): void => {
    setMoreOpen(false);
    onLayoutChange(mode);
  };

  return (
    <>
      <nav className="mobile-nav" role="navigation" aria-label="Sessions">
        <MobileDestination label="Home" icon={<HomeIcon />} active={layoutMode === 'home'} onClick={() => go('home')} />
        <MobileDestination label="Sessions" icon={<SessionsIcon />} active={layoutMode === 'tabs'} onClick={onShowSessions} detail={showingSessionDetail} />
        <MobileDestination label="Daily" icon={<DailyIcon />} active={layoutMode === 'today'} onClick={() => go('today')} />
        <MobileDestination label="History" icon={<SearchIcon />} active={layoutMode === 'search'} onClick={() => go('search')} />
        <MobileDestination label="More" icon={<MoreIcon />} active={moreActive || moreOpen} onClick={() => setMoreOpen(true)} />
      </nav>

      {moreOpen ? (
        <div className="bottom-sheet-backdrop" onClick={() => setMoreOpen(false)}>
          <div
            ref={sheetRef}
            className="bottom-sheet mobile-more-sheet"
            role="dialog"
            aria-modal="true"
            aria-labelledby="mobile-more-title"
            tabIndex={-1}
            onClick={(event) => event.stopPropagation()}
            onKeyDown={(event) => {
              if (event.key === 'Escape') setMoreOpen(false);
            }}
          >
            <div className="bottom-sheet-handle" />
            <header className="mobile-more-heading">
              <div><span>Sessions</span><h2 id="mobile-more-title">More</h2></div>
              <button type="button" onClick={() => setMoreOpen(false)} aria-label="Close">×</button>
            </header>
            <div className="mobile-more-grid">
              <MoreDestination title="Fleet" detail="Computers and remote agents" icon={<FleetIcon />} onClick={() => go('fleet')} />
              <MoreDestination title="Usage" detail="Tokens, cost, and projects" icon={<UsageIcon />} onClick={() => go('usage')} />
              <MoreDestination title="Settings" detail="Agents, access, and appearance" icon={<SettingsIcon />} onClick={() => go('settings')} />
              <MoreDestination title="Connections" detail="Pair and manage machines" icon={<ConnectionsIcon />} onClick={() => go('connections')} />
            </div>
            <a className="mobile-more-somewhere" href="https://somewhere.tech" target="_blank" rel="noreferrer">
              <img src="/somewhere-logo.svg" alt="" />
              <span><strong>somewhere.tech</strong><small>Cloud and backup for Sessions</small></span>
              <span aria-hidden>↗</span>
            </a>
          </div>
        </div>
      ) : null}
    </>
  );
}

function MobileDestination({
  label,
  icon,
  active,
  onClick,
  detail = false
}: {
  label: string;
  icon: ReactNode;
  active: boolean;
  onClick: () => void;
  detail?: boolean;
}): JSX.Element {
  return (
    <button
      type="button"
      className={`mn-btn mn-destination${active ? ' is-active' : ''}`}
      onClick={onClick}
      aria-label={detail ? `Back to ${label}` : `Open ${label}`}
      aria-current={active ? 'page' : undefined}
    >
      <span className="mn-destination-icon" aria-hidden>{icon}</span>
      <span>{label}</span>
      {detail ? <span className="mn-detail-dot" aria-hidden /> : null}
    </button>
  );
}

function MoreDestination({ title, detail, icon, onClick }: { title: string; detail: string; icon: ReactNode; onClick: () => void }): JSX.Element {
  return (
    <button type="button" onClick={onClick}>
      <span className="mobile-more-icon" aria-hidden>{icon}</span>
      <span><strong>{title}</strong><small>{detail}</small></span>
      <span aria-hidden>›</span>
    </button>
  );
}

function Icon({ children }: { children: ReactNode }): JSX.Element {
  return <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">{children}</svg>;
}
function HomeIcon(): JSX.Element { return <Icon><path d="M3 10.5 12 3l9 7.5"/><path d="M5 9.5V21h14V9.5"/><path d="M9 21v-7h6v7"/></Icon>; }
function SessionsIcon(): JSX.Element { return <Icon><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M8 9h8M8 13h8M8 17h5"/></Icon>; }
function DailyIcon(): JSX.Element { return <Icon><rect x="3" y="5" width="18" height="16" rx="2"/><path d="M8 3v4M16 3v4M3 10h18"/><path d="M8 15h8"/></Icon>; }
function SearchIcon(): JSX.Element { return <Icon><circle cx="11" cy="11" r="7"/><path d="m20 20-4-4"/></Icon>; }
function MoreIcon(): JSX.Element { return <Icon><circle cx="5" cy="12" r="1"/><circle cx="12" cy="12" r="1"/><circle cx="19" cy="12" r="1"/></Icon>; }
function FleetIcon(): JSX.Element { return <Icon><rect x="4" y="3" width="16" height="7" rx="2"/><rect x="4" y="14" width="16" height="7" rx="2"/><path d="M8 6.5h.01M8 17.5h.01"/></Icon>; }
function UsageIcon(): JSX.Element { return <Icon><path d="M4 20V10M10 20V4M16 20v-7M22 20H2"/></Icon>; }
function SettingsIcon(): JSX.Element { return <Icon><circle cx="12" cy="12" r="3"/><path d="M19 12a7 7 0 0 0-.1-1l2-1.6-2-3.4-2.5 1A8 8 0 0 0 15 6l-.4-2.6h-4L10 6a8 8 0 0 0-1.4.8l-2.4-1-2 3.5 2 1.6a7 7 0 0 0 0 2.1l-2 1.6 2 3.4 2.4-1A8 8 0 0 0 10 18l.5 2.6h4L15 18a8 8 0 0 0 1.4-.8l2.5 1 2-3.5-2-1.6a7 7 0 0 0 .1-1.1Z"/></Icon>; }
function ConnectionsIcon(): JSX.Element { return <Icon><path d="M8.5 15.5 6 18a3 3 0 0 1-4-4l4-4a3 3 0 0 1 4 0"/><path d="m15.5 8.5 2.5-2.5a3 3 0 0 1 4 4l-4 4a3 3 0 0 1-4 0"/><path d="m8 16 8-8"/></Icon>; }
