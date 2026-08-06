import { useEffect, useMemo, useState } from 'react';
import { type UsageOptions, type UsageReport, type UsageRow, type UsageTokens } from '../api/sessionsd';
import { serverDisplayName, useServers } from '../lib/servers';
import { getCachedUsage, requestUsageReport } from '../lib/usageCache';
import { combineFleetUsage, type FleetUsageSummary } from '../lib/fleetUsage';
import { useSessions } from '../store/sessions';
import { TagEditor } from './TagEditor';
import { ProviderBadge, normalizeProvider } from './ProviderBadge';

type Group = UsageReport['group'];
type Mode = UsageReport['mode'];
type Provider = 'all' | 'claude' | 'codex';
type Period = 'today' | 'week' | 'month' | 'all' | 'custom';
type Scope = 'fleet' | 'machine';

interface SavedUsageView {
  id: string;
  name: string;
  group: Group;
  mode: Mode;
  provider: Provider;
  dimension: string;
  since: string;
  until: string;
  period: Period;
  scope: Scope;
}

const SAVED_VIEWS_KEY = 'sessions:usage-saved-views:v1';
const GROUPS: Array<{ id: Group; label: string }> = [
  { id: 'daily', label: 'By day' },
  { id: 'weekly', label: 'By week' },
  { id: 'monthly', label: 'By month' },
  { id: 'session', label: 'Sessions' },
  { id: 'tag', label: 'Tags' },
  { id: 'model', label: 'Models' },
  { id: 'provider', label: 'Providers' }
];

const PERIODS: Array<{ id: Period; label: string }> = [
  { id: 'today', label: 'Today' },
  { id: 'week', label: 'This week' },
  { id: 'month', label: 'This month' },
  { id: 'all', label: 'All time' },
  { id: 'custom', label: 'Custom' }
];

function localDate(value: Date): string {
  const year = value.getFullYear();
  const month = String(value.getMonth() + 1).padStart(2, '0');
  const day = String(value.getDate()).padStart(2, '0');
  return `${year}-${month}-${day}`;
}

function periodDates(period: Period, since: string, until: string): { since?: string; until?: string } {
  if (period === 'custom') return { since: since || undefined, until: until || undefined };
  if (period === 'all') return {};
  const now = new Date();
  const start = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  if (period === 'week') {
    const mondayOffset = (start.getDay() + 6) % 7;
    start.setDate(start.getDate() - mondayOffset);
  } else if (period === 'month') {
    start.setDate(1);
  }
  return { since: localDate(start), until: localDate(now) };
}

function totalTokens(tokens: UsageTokens): number {
  return tokens.inputTokens + tokens.outputTokens + tokens.cacheCreationTokens + tokens.cacheReadTokens;
}

function compactNumber(value: number): string {
  return new Intl.NumberFormat(undefined, { notation: value >= 10_000 ? 'compact' : 'standard', maximumFractionDigits: 1 }).format(value);
}

function dollars(value: number): string {
  return new Intl.NumberFormat(undefined, { style: 'currency', currency: 'USD', minimumFractionDigits: value < 1 ? 3 : 2, maximumFractionDigits: value < 1 ? 3 : 2 }).format(value);
}

function readSavedViews(): SavedUsageView[] {
  try {
    const parsed = JSON.parse(window.localStorage.getItem(SAVED_VIEWS_KEY) ?? '[]') as unknown;
    if (!Array.isArray(parsed)) return [];
    return parsed.filter((item): item is SavedUsageView => {
      if (!item || typeof item !== 'object') return false;
      const view = item as Partial<SavedUsageView>;
      const valid = typeof view.id === 'string' && typeof view.name === 'string'
        && GROUPS.some((group) => group.id === view.group)
        && (view.mode === 'auto' || view.mode === 'calculate' || view.mode === 'display')
        && (view.provider === 'all' || view.provider === 'claude' || view.provider === 'codex')
        && typeof view.dimension === 'string' && typeof view.since === 'string' && typeof view.until === 'string';
      if (!valid) return false;
      view.period = PERIODS.some((period) => period.id === view.period) ? view.period : (view.since || view.until ? 'custom' : 'all');
      view.scope = view.scope === 'machine' ? 'machine' : 'fleet';
      return true;
    }).slice(0, 20);
  } catch {
    return [];
  }
}

function writeSavedViews(views: SavedUsageView[]): void {
  try { window.localStorage.setItem(SAVED_VIEWS_KEY, JSON.stringify(views)); } catch { /* storage is optional */ }
}

export function UsageDashboard(): JSX.Element {
  const activeServerId = useServers((state) => state.activeId);
  const servers = useServers((state) => state.servers);
  const [group, setGroup] = useState<Group>('daily');
  const [period, setPeriod] = useState<Period>('today');
  const [scope, setScope] = useState<Scope>('fleet');
  const [mode, setMode] = useState<Mode>('auto');
  const [provider, setProvider] = useState<Provider>('all');
  const [dimension, setDimension] = useState('product');
  const [since, setSince] = useState('');
  const [until, setUntil] = useState('');
  const [savedViews, setSavedViews] = useState<SavedUsageView[]>(readSavedViews);
  const [activeSavedId, setActiveSavedId] = useState('');
  const [viewName, setViewName] = useState('');
  const [report, setReport] = useState<UsageReport | null>(null);
  const [fleetSummary, setFleetSummary] = useState<FleetUsageSummary | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [refreshToken, setRefreshToken] = useState(0);

	useEffect(() => {
		const selectedServers = scope === 'fleet'
			? servers
			: servers.filter((server) => server.id === activeServerId);
		if (selectedServers.length === 0) {
			setReport(null);
			setFleetSummary(null);
			setLoading(false);
			return;
		}
		let alive = true;
		const dates = periodDates(period, since, until);
		const options: UsageOptions = {
			group,
			mode,
			provider: provider === 'all' ? undefined : provider,
			dimension: group === 'tag' ? dimension.trim().toLowerCase() || 'product' : undefined,
			since: dates.since,
			until: dates.until,
			includeEvents: scope === 'fleet'
		};
		const cached = selectedServers.map((server) => ({
			serverId: server.id,
			serverName: serverDisplayName(server, true),
			report: getCachedUsage(server.id, options) ?? undefined
		}));
		const cachedSummary = combineFleetUsage(cached);
		setReport(cachedSummary.report);
		setFleetSummary(cachedSummary);
		setLoading(true);
		setError(null);
		const timer = window.setTimeout(() => {
			void Promise.all(selectedServers.map(async (server) => {
				try {
					return { serverId: server.id, serverName: serverDisplayName(server, true), report: await requestUsageReport(server.id, options, refreshToken > 0) };
				} catch (reason) {
					return { serverId: server.id, serverName: serverDisplayName(server, true), error: reason instanceof Error ? reason.message : 'Usage unavailable' };
				}
			})).then((sources) => {
				if (!alive) return;
				const summary = combineFleetUsage(sources);
				setFleetSummary(summary);
				setReport(summary.report);
				setError(summary.reportingMachines === 0 ? sources.map((source) => `${source.serverName}: ${source.error ?? 'unavailable'}`).join(' · ') : null);
			})
        .finally(() => { if (alive) setLoading(false); });
    }, group === 'tag' ? 250 : 0);
    return () => { alive = false; window.clearTimeout(timer); };
  }, [activeServerId, servers, scope, period, group, mode, provider, dimension, since, until, refreshToken]);

  // While the dashboard is open, an inexpensive incremental sync keeps newly
  // appended provider usage visible without making ingestion a daemon hot path.
  useEffect(() => {
    const timer = window.setInterval(() => {
      if (!document.hidden) setRefreshToken((value) => value + 1);
    }, 30_000);
    return () => window.clearInterval(timer);
  }, []);

  const maxTokens = useMemo(() => Math.max(1, ...(report?.rows.map((row) => totalTokens(row.tokens)) ?? [])), [report]);
  const selectGroup = (value: Group): void => { setGroup(value); setActiveSavedId(''); };
  const applySavedView = (id: string): void => {
    setActiveSavedId(id);
    const view = savedViews.find((candidate) => candidate.id === id);
    if (!view) return;
    setGroup(view.group); setMode(view.mode); setProvider(view.provider); setDimension(view.dimension);
    setSince(view.since); setUntil(view.until); setPeriod(view.period); setScope(view.scope);
  };
  const saveCurrentView = (): void => {
    const name = viewName.trim();
    if (!name) return;
    const view: SavedUsageView = {
      id: `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`,
      name: name.slice(0, 60), group, mode, provider, dimension: dimension.trim().toLowerCase() || 'product', since, until, period, scope
    };
    const next = [...savedViews.filter((candidate) => candidate.name.toLowerCase() !== view.name.toLowerCase()), view].slice(-20);
    setSavedViews(next); setActiveSavedId(view.id); setViewName(''); writeSavedViews(next);
  };
  const deleteActiveView = (): void => {
    if (!activeSavedId) return;
    const next = savedViews.filter((view) => view.id !== activeSavedId);
    setSavedViews(next); setActiveSavedId(''); writeSavedViews(next);
  };

  return (
    <div className="usage-view">
      <div className="usage-shell">
        <header className="usage-heading">
          <div>
            <h1>Usage</h1>
            <p>{scope === 'fleet' ? 'Claude and Codex usage across every configured machine.' : `Usage on ${report?.machine || 'the selected machine'}`} Reports are requested directly from each machine.</p>
          </div>
          <button type="button" className="btn btn-ghost" disabled={loading} onClick={() => setRefreshToken((value) => value + 1)}>
            {loading ? 'Indexing…' : 'Refresh'}
          </button>
        </header>

        <div className="usage-saved-views">
          <label>Saved view
            <select value={activeSavedId} onChange={(event) => applySavedView(event.target.value)}>
              <option value="">Current filters</option>
              {savedViews.map((view) => <option value={view.id} key={view.id}>{view.name}</option>)}
            </select>
          </label>
          <label>Name<input value={viewName} maxLength={60} onChange={(event) => setViewName(event.target.value)} onKeyDown={(event) => { if (event.key === 'Enter') { event.preventDefault(); saveCurrentView(); } }} placeholder="This month by product" /></label>
          <button type="button" className="btn btn-ghost" disabled={!viewName.trim()} onClick={saveCurrentView}>Save current</button>
          {activeSavedId ? <button type="button" className="usage-delete-view" onClick={deleteActiveView}>Delete</button> : null}
        </div>

        <div className="usage-controls">
          <div className="usage-control-group">
            <span>Scope</span>
            <div className="usage-segmented" role="tablist" aria-label="Usage scope">
              <button type="button" className={scope === 'fleet' ? 'is-active' : ''} onClick={() => { setScope('fleet'); setActiveSavedId(''); }}>Entire fleet</button>
              <button type="button" className={scope === 'machine' ? 'is-active' : ''} onClick={() => { setScope('machine'); setActiveSavedId(''); }}>Selected machine</button>
            </div>
          </div>
          <div className="usage-control-group">
            <span>Time range</span>
            <div className="usage-segmented" role="tablist" aria-label="Usage time range">
              {PERIODS.map((item) => <button key={item.id} type="button" className={period === item.id ? 'is-active' : ''} onClick={() => { setPeriod(item.id); setActiveSavedId(''); }}>{item.label}</button>)}
            </div>
          </div>
          <div className="usage-control-group">
            <span>Group by</span>
            <div className="usage-segmented" role="tablist" aria-label="Usage grouping">
              {GROUPS.map((item) => (
                <button key={item.id} type="button" className={group === item.id ? 'is-active' : ''} onClick={() => selectGroup(item.id)}>{item.label}</button>
              ))}
            </div>
          </div>
          <label>Provider
            <select value={provider} onChange={(event) => { setProvider(event.target.value as Provider); setActiveSavedId(''); }}>
              <option value="all">All</option><option value="claude">Claude</option><option value="codex">Codex</option>
            </select>
          </label>
          <label>Cost
            <select value={mode} onChange={(event) => { setMode(event.target.value as Mode); setActiveSavedId(''); }}>
              <option value="auto">Auto</option><option value="calculate">Calculate</option><option value="display">Recorded</option>
            </select>
          </label>
          {group === 'tag' ? (
            <label>Tag key<input value={dimension} onChange={(event) => { setDimension(event.target.value); setActiveSavedId(''); }} placeholder="product" /></label>
          ) : null}
          {period === 'custom' ? <><label>Since<input type="date" value={since} onChange={(event) => { setSince(event.target.value); setActiveSavedId(''); }} /></label>
          <label>Until<input type="date" value={until} onChange={(event) => { setUntil(event.target.value); setActiveSavedId(''); }} /></label></> : null}
        </div>

        {fleetSummary ? (
          <div className="usage-fleet-status">
            <strong>{scope === 'fleet' ? `${fleetSummary.reportingMachines} of ${fleetSummary.configuredMachines} machines reporting` : 'Selected machine'}</strong>
            {scope === 'fleet' ? <span>{fleetSummary.exactDeduplication ? `Copied history deduplicated${fleetSummary.duplicatesRemoved > 0 ? ` · ${fleetSummary.duplicatesRemoved} duplicate events removed` : ''}.` : 'Machine totals are combined; update older machines for exact copied-history deduplication.'}</span> : null}
            {fleetSummary.unavailableMachines.length > 0 ? <span>Unavailable: {fleetSummary.unavailableMachines.join(', ')}</span> : null}
          </div>
        ) : null}

        {error ? <div className="usage-error">{error}</div> : null}
        {report ? (
          <>
            <section className="usage-kpis" aria-label="Usage totals">
              <UsageKPI label="Total tokens" value={compactNumber(totalTokens(report.totals.tokens))} detail={`${compactNumber(report.totals.entries)} billable events`} />
              <UsageKPI label="Estimated cost" value={dollars(report.totals.costUSD)} detail={mode === 'auto' ? 'recorded where available' : mode === 'calculate' ? 'pinned token pricing' : 'recorded costs only'} />
              <UsageKPI label="Cache reads" value={compactNumber(report.totals.tokens.cacheReadTokens)} detail={`${percent(report.totals.tokens.cacheReadTokens, totalTokens(report.totals.tokens))}% of tokens`} />
              <UsageKPI label="Reasoning" value={compactNumber(report.totals.tokens.reasoningTokens)} detail="included in output tokens" />
              <UsageKPI label="Sources" value={compactNumber(report.scan.filesSeen)} detail={`${report.scan.filesRead} changed this refresh`} />
            </section>

            <section className="usage-panel">
              <header><h2>{GROUPS.find((item) => item.id === group)?.label} breakdown</h2><span>{report.rows.length} rows · schema v{report.schemaVersion}</span></header>
              {report.rows.length === 0 ? <div className="usage-empty">No usage matched this scope, time range, and filter set.</div> : (
                <div className="usage-row-list">
                  {report.rows.map((row) => (
                    <UsageReportRow key={row.key} row={row} maxTokens={maxTokens} editable={scope === 'machine' && group === 'session'} onTagsSaved={() => setRefreshToken((value) => value + 1)} />
                  ))}
                </div>
              )}
            </section>

            <footer className="usage-provenance">
              <span>Costs are estimates.</span> {report.pricing.note}{' '}
              <a href={report.pricing.url} target="_blank" rel="noreferrer">Pricing revision {report.pricing.revision}</a>
              {report.totals.missingPricingEntries > 0 ? <strong>{report.totals.missingPricingEntries} entries are visibly unpriced.</strong> : null}
            </footer>
          </>
        ) : loading ? <div className="usage-empty">Building the local usage index…</div> : null}
      </div>
    </div>
  );
}

function UsageKPI({ label, value, detail }: { label: string; value: string; detail: string }): JSX.Element {
  return <div className="usage-kpi"><span>{label}</span><strong>{value}</strong><small>{detail}</small></div>;
}

function UsageReportRow({ row, maxTokens, editable, onTagsSaved }: { row: UsageRow; maxTokens: number; editable: boolean; onTagsSaved: () => void }): JSX.Element {
  const updateTags = useSessions((state) => state.updateTags);
  const [expanded, setExpanded] = useState(false);
  const [editing, setEditing] = useState(false);
  const [tags, setTags] = useState<Record<string, string>>(row.tags ?? {});
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const tokens = totalTokens(row.tokens);
  return (
    <div className="usage-row">
      <div className="usage-row-main">
        <div className="usage-row-title">
          <strong>{row.key}</strong>
          {normalizeProvider(row.provider) ? <ProviderBadge provider={normalizeProvider(row.provider)!} compact /> : null}
          {row.missingPricingEntries > 0 ? <span className="usage-unpriced" title="No matching model price in the pinned snapshot">unpriced</span> : null}
          {Object.entries(row.tags ?? {}).slice(0, 3).map(([key, value]) => <span className="usage-mini-tag" key={key}>{key}={value}</span>)}
        </div>
        <div className="usage-bar"><span style={{ width: `${Math.max(1.5, (tokens / maxTokens) * 100)}%` }} /></div>
        <div className="usage-row-meta">{row.models.join(' · ') || 'Unknown model'} · {compactNumber(row.entries)} events</div>
      </div>
      <div className="usage-row-number"><strong>{compactNumber(tokens)}</strong><span>tokens</span></div>
      <div className="usage-row-number"><strong>{dollars(row.costUSD)}</strong><span>cost</span></div>
      <div className="usage-row-actions">
        <button type="button" onClick={() => setExpanded((value) => !value)}>{expanded ? 'Less' : 'Details'}</button>
        {editable && row.sessionId ? <button type="button" onClick={() => setEditing((value) => !value)}>{editing ? 'Close tags' : 'Tags'}</button> : null}
      </div>
      {expanded ? (
        <div className="usage-row-detail">
          <UsageDetail label="Fresh input" value={compactNumber(row.tokens.inputTokens)} />
          <UsageDetail label="Output" value={compactNumber(row.tokens.outputTokens)} />
          <UsageDetail label="Reasoning" value={compactNumber(row.tokens.reasoningTokens)} hint="subset of output" />
          <UsageDetail label="Cache write" value={compactNumber(row.tokens.cacheCreationTokens)} />
          <UsageDetail label="Cache read" value={compactNumber(row.tokens.cacheReadTokens)} />
          <UsageDetail label="Recorded" value={dollars(row.recordedCostUSD)} />
          <UsageDetail label="Calculated" value={dollars(row.calculatedCostUSD)} />
        </div>
      ) : null}
      {editing && row.sessionId ? (
        <div className="usage-tag-edit">
          <TagEditor value={tags} onChange={setTags} disabled={saving} />
          {error ? <span className="tag-editor-error">{error}</span> : null}
          <div className="usage-tag-actions">
            <button type="button" className="btn btn-primary" disabled={saving} onClick={() => {
              setSaving(true); setError(null);
              void updateTags(row.sessionId!, tags).then(() => { setEditing(false); onTagsSaved(); }).catch((reason: unknown) => setError(reason instanceof Error ? reason.message : 'Could not save tags')).finally(() => setSaving(false));
            }}>{saving ? 'Saving…' : 'Save tags'}</button>
          </div>
        </div>
      ) : null}
    </div>
  );
}

function UsageDetail({ label, value, hint }: { label: string; value: string; hint?: string }): JSX.Element {
  return <div><span>{label}</span><strong>{value}</strong>{hint ? <small>{hint}</small> : null}</div>;
}

function percent(part: number, total: number): number { return total > 0 ? Math.round((part / total) * 100) : 0; }
