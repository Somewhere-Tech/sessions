import { useEffect, useMemo, useState, type CSSProperties } from 'react';
import { type DailyDay } from '../api/sessionsd';
import {
  currentLocalDate,
  getCachedDailyDay,
  requestDailyDay
} from '../lib/dailyCache';
import { useServers } from '../lib/servers';
import { ProviderBadge, normalizeProvider } from './ProviderBadge';

function shiftDate(value: string, days: number): string {
  const [year, month, day] = value.split('-').map(Number);
  return currentLocalDate(new Date(year!, month! - 1, day! + days));
}

function readableDate(value: string): string {
  const [year, month, day] = value.split('-').map(Number);
  return new Intl.DateTimeFormat(undefined, {
    weekday: 'long',
    month: 'long',
    day: 'numeric',
    year: year === new Date().getFullYear() ? undefined : 'numeric'
  }).format(new Date(year!, month! - 1, day!));
}

function compactNumber(value: number): string {
  return new Intl.NumberFormat(undefined, { notation: value >= 10_000 ? 'compact' : 'standard', maximumFractionDigits: 1 }).format(value);
}

function dollars(value: number): string {
  return new Intl.NumberFormat(undefined, { style: 'currency', currency: 'USD', maximumFractionDigits: 2 }).format(value);
}

function totalTokens(day: DailyDay): number {
  const tokens = day.usage.tokens;
  return tokens.inputTokens + tokens.outputTokens + tokens.cacheCreationTokens + tokens.cacheReadTokens;
}

export function DailyView(): JSX.Element {
  const activeServerId = useServers((state) => state.activeId ?? '');
  const [date, setDate] = useState(currentLocalDate);
  const [day, setDay] = useState<DailyDay | null>(() => getCachedDailyDay(activeServerId, currentLocalDate()));
  const [loading, setLoading] = useState(() => day === null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!activeServerId) return;
    let cancelled = false;
    const cached = getCachedDailyDay(activeServerId, date);
    setDay(cached);
    setLoading(cached === null);
    setError(null);
    void requestDailyDay(activeServerId, date)
      .then((loaded) => {
        if (!cancelled) setDay(loaded);
      })
      .catch((reason: unknown) => {
        if (!cancelled) setError(reason instanceof Error ? reason.message : 'Could not load the day');
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => { cancelled = true; };
  }, [activeServerId, date]);

  const activityRoots = useMemo(() => {
    if (!day) return [];
    const ids = new Set(day.activities.map((activity) => activity.id));
    return day.activities.map((activity) => ({
      ...activity,
      depth: activity.creatorAncestry?.filter((id) => ids.has(id)).length ?? (activity.parentSessionId && ids.has(activity.parentSessionId) ? 1 : 0)
    }));
  }, [day]);

  const managedCount = day?.activities.filter((activity) => activity.source !== 'provider').length ?? 0;
  const observedCount = (day?.activities.length ?? 0) - managedCount;
  const projectCount = day
    ? new Set(day.activities.map((activity) => activity.sourceRepo || activity.cwd).filter(Boolean)).size
    : 0;
  const today = currentLocalDate();

  return (
    <div className="today-view">
      <div className="today-shell">
        <header className="today-heading">
          <div>
            <span className="today-eyebrow">Private work journal</span>
            <h1>Daily</h1>
            <p>Sessions and provider logs supply local activity and usage facts. No model call is made.</p>
          </div>
          <div className="today-date-panel">
            <div className="today-date-control">
              <button type="button" onClick={() => setDate(shiftDate(date, -1))} aria-label="Previous day">←</button>
              <label>
                <span>{date === today ? 'Today' : 'Selected day'}</span>
                <strong>{readableDate(date)}</strong>
                <input type="date" value={date} max={today} onChange={(event) => setDate(event.currentTarget.value)} aria-label="Choose a day" />
              </label>
              <button type="button" disabled={date >= today} onClick={() => setDate(shiftDate(date, 1))} aria-label="Next day">→</button>
            </div>
            {date !== today ? <button type="button" className="today-jump" onClick={() => setDate(today)}>Jump to today</button> : null}
          </div>
        </header>

        {error ? <div className="today-error" role="alert">{error}</div> : null}
        {loading && !day ? <DailySkeleton /> : day ? (
          <>
            <section className="today-kpis" aria-label="Daily totals">
              <DailyKPI
                label="Activity"
                value={String(day.activities.length)}
                detail={`${managedCount} in Sessions · ${observedCount} outside`}
              />
              <DailyKPI label="Tokens" value={compactNumber(totalTokens(day))} detail={`${compactNumber(day.usage.tokens.reasoningTokens)} reasoning`} />
              <DailyKPI
                label="Estimated cost"
                value={dollars(day.usage.costUSD)}
                detail={day.usage.missingPricingEntries > 0 ? `${day.usage.missingPricingEntries} unpriced events` : `${day.usage.entries} usage events`}
              />
              <DailyKPI label="Projects" value={String(projectCount)} detail={day.timezone} />
            </section>

            <section className="today-activity-card">
              <header><div><span className="today-section-kicker">Local evidence</span><h2>What was worked on</h2></div><span>{managedCount} Sessions · {observedCount} outside</span></header>
              {activityRoots.length === 0 ? <div className="today-empty">No Sessions or locally observed provider activity was recorded for this day.</div> : (
                <div className="today-timeline">
                  {activityRoots.map((activity) => (
                    <article className="today-activity" key={activity.id} style={{ '--activity-depth': Math.min(activity.depth, 3) } as CSSProperties}>
                      <span className={`today-outcome is-${activity.outcome}`} aria-label={activity.outcome} />
                      <div className="today-activity-main">
                        <header><strong>{activity.name}</strong><time>{new Date(activity.lastActivityAt).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' })}</time></header>
                        {activity.summary || activity.description ? <p>{activity.summary || activity.description}</p> : null}
                        <div className="today-activity-meta">
                          {normalizeProvider(activity.tool)
                            ? <ProviderBadge provider={normalizeProvider(activity.tool)!} compact />
                            : <span>{activity.tool}</span>}
                          {activity.source === 'provider' ? <span className="today-observed-source" title="This conversation has not been brought into Sessions">Outside Sessions</span> : null}
                          {activity.origin && activity.origin !== 'Codex' && activity.origin !== 'Claude Code' ? <span>{activity.origin}</span> : null}
                          <span>{activity.branch || activity.sourceRepo || activity.cwd.replace(/^\/Users\/[^/]+/, '~')}</span>
                          {activity.parentSessionId ? <span>child lane</span> : null}
                          {Object.entries(activity.tags ?? {}).map(([key, value]) => <span className="today-tag" key={key}>{key}={value}</span>)}
                        </div>
                      </div>
                    </article>
                  ))}
                </div>
              )}
            </section>
          </>
        ) : <DailySkeleton />}
      </div>
    </div>
  );
}

function DailyKPI({ label, value, detail }: { label: string; value: string; detail: string }): JSX.Element {
  return <div className="today-kpi"><span>{label}</span><strong>{value}</strong><small>{detail}</small></div>;
}

function DailySkeleton(): JSX.Element {
  return (
    <div className="today-skeleton" aria-busy="true" aria-label="Loading daily activity">
      <section className="today-kpis">
        {[0, 1, 2, 3].map((key) => <div className="today-kpi" key={key}><i className="today-skeleton-line is-label" /><i className="today-skeleton-line is-value" /><i className="today-skeleton-line is-detail" /></div>)}
      </section>
      <section className="today-activity-card">
        <header><div><i className="today-skeleton-line is-label" /><i className="today-skeleton-line is-heading" /></div></header>
        <div className="today-skeleton-activities">
          {[0, 1, 2].map((key) => <div key={key}><i className="today-skeleton-dot" /><span><i className="today-skeleton-line is-row-title" /><i className="today-skeleton-line is-row-copy" /></span></div>)}
        </div>
      </section>
    </div>
  );
}
