import { useEffect, useMemo, useState, type CSSProperties } from 'react';
import {
  generateRecap,
  updateRecapSettings,
  type RecapDay,
  type RecapProvider,
  type RecapSettings
} from '../api/sessionsd';
import {
  currentLocalDate,
  getCachedDailyDay,
  getCachedRecapDates,
  rememberDailyDay,
  rememberRecapDate,
  requestDailyDay,
  requestRecapDates
} from '../lib/dailyCache';
import { renderContent } from '../lib/contentRender';
import { useServers } from '../lib/servers';
import { isTauri } from '../lib/tauriBridge';
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

function savedDateLabel(value: string): string {
  const [year, month, day] = value.split('-').map(Number);
  return new Intl.DateTimeFormat(undefined, {
    month: 'short',
    day: 'numeric',
    year: year === new Date().getFullYear() ? undefined : '2-digit'
  }).format(new Date(year!, month! - 1, day!));
}

function compactNumber(value: number): string {
  return new Intl.NumberFormat(undefined, { notation: value >= 10_000 ? 'compact' : 'standard', maximumFractionDigits: 1 }).format(value);
}

function dollars(value: number): string {
  return new Intl.NumberFormat(undefined, { style: 'currency', currency: 'USD', maximumFractionDigits: 2 }).format(value);
}

function totalTokens(day: RecapDay): number {
  const tokens = day.usage.tokens;
  return tokens.inputTokens + tokens.outputTokens + tokens.cacheCreationTokens + tokens.cacheReadTokens;
}

export function DailyView(): JSX.Element {
  const nativeClient = isTauri();
  const activeServerId = useServers((state) => state.activeId ?? '');
  const [date, setDate] = useState(currentLocalDate);
  const [day, setDay] = useState<RecapDay | null>(() => getCachedDailyDay(activeServerId, currentLocalDate()));
  const [savedDates, setSavedDates] = useState<string[]>(() => getCachedRecapDates(activeServerId));
  const [loading, setLoading] = useState(() => day === null);
  const [generating, setGenerating] = useState(false);
  const [savingSettings, setSavingSettings] = useState(false);
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

  useEffect(() => {
    if (!activeServerId) return;
    let cancelled = false;
    const cached = getCachedRecapDates(activeServerId);
    if (cached.length > 0) setSavedDates(cached);
    void requestRecapDates(activeServerId)
      .then((dates) => {
        if (!cancelled) setSavedDates(dates);
      })
      .catch(() => {
        // Day navigation still works against the local API. An older runtime
        // simply will not show the saved-day shortcuts until it is upgraded.
      });
    return () => { cancelled = true; };
  }, [activeServerId]);

  const saveSettings = async (settings: RecapSettings): Promise<void> => {
    if (savingSettings) return;
    setSavingSettings(true);
    setError(null);
    try {
      const saved = await updateRecapSettings(settings);
      setDay((current) => {
        if (!current) return current;
        const next = { ...current, settings: saved };
        rememberDailyDay(activeServerId, next);
        return next;
      });
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : 'Could not save recap settings');
    } finally {
      setSavingSettings(false);
    }
  };

  const generate = async (): Promise<void> => {
    if (!day || day.settings.provider === 'off' || generating) return;
    setGenerating(true);
    setError(null);
    try {
      const generated = await generateRecap(date, day.document !== null);
      rememberDailyDay(activeServerId, generated);
      setDay(generated);
      setSavedDates(rememberRecapDate(activeServerId, date));
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : 'Could not generate the recap');
    } finally {
      setGenerating(false);
    }
  };

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
            <p>Local facts need no model call. Written recaps are optional, saved on this Mac, and easy to revisit.</p>
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

        <section className="today-history" aria-label="Saved daily recaps">
          <div>
            <span>Saved recaps</span>
            <strong>{savedDates.length > 0 ? `${savedDates.length} day${savedDates.length === 1 ? '' : 's'} on this Mac` : 'Your journal starts here'}</strong>
          </div>
          <div className="today-history-days">
            {savedDates.length > 0 ? savedDates.map((savedDate) => (
              <button
                type="button"
                key={savedDate}
                className={savedDate === date ? 'is-active' : ''}
                onClick={() => setDate(savedDate)}
                aria-current={savedDate === date ? 'date' : undefined}
              >
                <span aria-hidden />
                {savedDateLabel(savedDate)}
              </button>
            )) : <span>Generate a recap to save the first day.</span>}
          </div>
        </section>

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

            <section className="today-recap-card">
              <header>
                <div>
                  <span className="today-section-kicker">{nativeClient ? 'One compact model call' : 'Native app feature'}</span>
                  <h2>Daily recap</h2>
                </div>
                {nativeClient ? <div className="today-recap-controls">
                  <select
                    value={day.settings.provider}
                    disabled={savingSettings || generating}
                    onChange={(event) => void saveSettings({ ...day.settings, provider: event.currentTarget.value as RecapProvider })}
                    aria-label="Daily recap provider"
                  >
                    <option value="off">Off</option>
                    <option value="codex">Codex · recommended</option>
                    <option value="claude">Claude</option>
                  </select>
                  <button type="button" className="btn today-generate" disabled={day.settings.provider === 'off' || generating || savingSettings} onClick={() => void generate()}>
                    {generating ? 'Writing…' : day.document ? 'Refresh recap' : 'Generate recap'}
                  </button>
                </div> : <span className="today-section-note">Open Sessions.app to write or refresh a recap.</span>}
              </header>
              {day.document ? (
                <>
                  {day.documentStale ? <div className="today-stale-note">Saved recap · local facts or your selected provider changed after this was written. Refresh only when you want a new version.</div> : null}
                  <div className="today-markdown" dangerouslySetInnerHTML={{ __html: renderContent(day.document.markdown) }} />
                  <footer>Saved {new Date(day.document.generatedAt).toLocaleString()} with {day.document.provider}. Stored only on this Mac.</footer>
                </>
              ) : day.settings.provider === 'off' ? (
                <div className="today-opt-in">
                  <strong>No model calls are enabled.</strong>
                  <p>The timeline and usage below come directly from local provider logs and Sessions metadata. {nativeClient ? 'Turn on Codex when you want a written recap.' : 'Written recaps are available only in the signed native app.'}</p>
                  {nativeClient ? <button type="button" className="btn" onClick={() => void saveSettings({ provider: 'codex' })}>Use Codex for recaps</button> : null}
                </div>
              ) : (
                <div className="today-opt-in">
                  <strong>{nativeClient ? `Ready to write with ${day.settings.provider === 'codex' ? 'Codex' : 'Claude'}.` : 'Open Sessions.app to write this recap.'}</strong>
                  <p>{nativeClient ? 'Sessions sends at most 32 KiB of compact metadata, final summaries, tags, and authoritative totals—not full transcripts. Your CLI chooses its default model.' : 'The browser view does not launch provider CLIs or make model calls.'}</p>
                </div>
              )}
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
      <section className="today-recap-card">
        <header><div><i className="today-skeleton-line is-label" /><i className="today-skeleton-line is-heading" /></div></header>
        <div className="today-skeleton-copy">
          <i className="today-skeleton-line" />
          <i className="today-skeleton-line" />
          <i className="today-skeleton-line is-short" />
        </div>
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
