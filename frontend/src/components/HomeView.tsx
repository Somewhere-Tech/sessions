import type { SessionInfo } from '../types';
import { getTabLabel, sessionLabel } from '../lib/tabLabels';
import { isCrashedSession, isDegradedSession } from '../lib/sessionStatus';
import { ProviderBadge, normalizeProvider } from './ProviderBadge';

interface Props {
  sessions: SessionInfo[];
  machine: string;
  onOpen: (id: string) => void;
  onNew: () => void;
  onNavigate: (view: 'today' | 'fleet' | 'usage') => void;
}

export function HomeView({ sessions, machine, onOpen, onNew, onNavigate }: Props): JSX.Element {
  const live = sessions.filter((session) => !session.exited);
  const attention = live.filter((session) => !session.working && session.idleReason === 'needs-input');
  const running = live.filter((session) => session.working);
  const recent = [...sessions].sort((a, b) => b.lastDataAt - a.lastDataAt).slice(0, 5);
  return (
    <div className="home-view">
      <header className="home-hero">
        <div><span>Agent operations</span><h1>Good {dayPart()}.</h1><p>{attention.length ? `${attention.length} session${attention.length === 1 ? '' : 's'} need your attention.` : 'Your agents are quiet. Start something ambitious.'}</p></div>
        <button type="button" className="btn btn-primary" onClick={onNew}>＋ New Session</button>
      </header>
      <section className="home-stat-grid">
        <button type="button" onClick={() => onNavigate('today')}><span>Needs you</span><strong>{attention.length}</strong><small>Waiting for direction</small></button>
        <button type="button" onClick={() => onNavigate('fleet')}><span>Running now</span><strong>{running.length}</strong><small>Across {machine}</small></button>
        <button type="button" onClick={() => onNavigate('usage')}><span>Live sessions</span><strong>{live.length}</strong><small>Usage and cost details</small></button>
      </section>
      <section className="home-recent">
        <header><div><span>Inbox</span><h2>Recent sessions</h2></div><button type="button" onClick={() => onNavigate('today')}>Open Daily →</button></header>
        <div className="home-session-list">
          {recent.map((session) => {
            const provider = normalizeProvider(session.tool);
            const failed = isCrashedSession(session);
            const finished = (session.exited && !failed) || session.idleReason === 'completed';
            const needs = session.idleReason === 'needs-input';
            const limited = isDegradedSession(session);
            const dotClass = session.working ? ' is-running' : failed ? ' is-failed' : needs ? ' is-needs' : limited ? ' is-limited' : finished ? ' is-finished' : '';
            return <button type="button" key={session.id} onClick={() => onOpen(session.id)}><span className={`home-session-dot${dotClass}`} /><span className="home-session-copy"><strong>{getTabLabel(session.id) ?? sessionLabel(session)}</strong><small>{session.lastSummary || session.idleDetail || session.description || session.cwd}</small></span>{provider ? <ProviderBadge provider={provider} compact /> : <span className="provider-badge is-shell is-compact">⌘ Shell</span>}<span className="home-session-time">{new Date(session.lastDataAt || session.createdAt).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' })}</span></button>;
          })}
          {recent.length === 0 ? <div className="home-empty">New sessions will appear here as an operational inbox.</div> : null}
        </div>
      </section>
    </div>
  );
}

function dayPart(): string {
  const hour = new Date().getHours();
  return hour < 12 ? 'morning' : hour < 18 ? 'afternoon' : 'evening';
}
