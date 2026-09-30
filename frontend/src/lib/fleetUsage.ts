import type { UsageEvent, UsageReport, UsageRow, UsageTokens } from '../api/sessionsd';
import type { PeerReport, PeerStatus } from './fleetPeerBudget';

export interface FleetUsageSource {
  serverId: string;
  serverName: string;
  report?: UsageReport;
  error?: string;
  /**
   * Where this machine stands. Absent means the obvious reading of the other
   * fields: a report is an answer, an error is a machine that could not be
   * reached, and neither is a machine still being asked.
   */
  status?: PeerStatus;
}

export interface FleetUsageSummary {
  report: UsageReport | null;
  configuredMachines: number;
  reportingMachines: number;
  /**
   * Every machine that has not contributed a report, each with why. A machine
   * that has not answered yet is `pending`, and saying anything worse about it
   * would be a guess about a request that is still in flight.
   */
  missingMachines: PeerReport[];
  /**
   * The machines that answered in the older shape, without event identities.
   * Named only once their answers are in hand: before that, nothing is known
   * about what they can and cannot deduplicate.
   */
  olderHostMachines: string[];
  exactDeduplication: boolean;
  duplicatesRemoved: number;
}

function statusOf(source: FleetUsageSource): PeerStatus {
  if (source.status) return source.status;
  if (source.report) return 'answered';
  return source.error ? 'unreachable' : 'pending';
}

function missingMachines(sources: FleetUsageSource[]): PeerReport[] {
  return sources
    .filter((source) => !source.report)
    .map((source) => ({
      serverId: source.serverId,
      serverName: source.serverName,
      status: statusOf(source),
      detail: source.error ?? null
    }));
}

const emptyTokens = (): UsageTokens => ({
  inputTokens: 0,
  outputTokens: 0,
  cacheCreationTokens: 0,
  cacheReadTokens: 0,
  reasoningTokens: 0
});

const emptyRow = (key: string, start?: string): UsageRow => ({
  key,
  start,
  models: [],
  tokens: emptyTokens(),
  costUSD: 0,
  recordedCostUSD: 0,
  calculatedCostUSD: 0,
  entries: 0,
  missingPricingEntries: 0
});

function addTokens(target: UsageTokens, source: UsageTokens): void {
  target.inputTokens += source.inputTokens;
  target.outputTokens += source.outputTokens;
  target.cacheCreationTokens += source.cacheCreationTokens;
  target.cacheReadTokens += source.cacheReadTokens;
  target.reasoningTokens += source.reasoningTokens;
}

function addRow(target: UsageRow, source: UsageRow): void {
  addTokens(target.tokens, source.tokens);
  target.costUSD += source.costUSD;
  target.recordedCostUSD += source.recordedCostUSD;
  target.calculatedCostUSD += source.calculatedCostUSD;
  target.entries += source.entries;
  target.missingPricingEntries += source.missingPricingEntries;
  target.models = [...new Set([...target.models, ...source.models])].sort();
}

function tokensTotal(tokens: UsageTokens): number {
  return tokens.inputTokens + tokens.outputTokens + tokens.cacheCreationTokens + tokens.cacheReadTokens;
}

function richerEvent(left: UsageEvent, right: UsageEvent): UsageEvent {
  if (tokensTotal(right.tokens) !== tokensTotal(left.tokens)) {
    return tokensTotal(right.tokens) > tokensTotal(left.tokens) ? right : left;
  }
  if (right.hasRecordedCost !== left.hasRecordedCost) return right.hasRecordedCost ? right : left;
  return right.model && !left.model ? right : left;
}

function rowFromEvent(event: UsageEvent): UsageRow {
  return {
    ...emptyRow(event.groupKey, event.start),
    provider: event.provider,
    providerSessionId: event.providerSessionId,
    sessionId: event.sessionId,
    tags: event.tags,
    models: event.model ? [event.model] : [],
    tokens: { ...event.tokens },
    costUSD: event.costUSD,
    recordedCostUSD: event.recordedCostUSD,
    calculatedCostUSD: event.calculatedCostUSD,
    entries: 1,
    missingPricingEntries: event.missingPricing ? 1 : 0
  };
}

function mergeRows(rows: UsageRow[]): UsageRow[] {
  const byKey = new Map<string, UsageRow>();
  const providers = new Map<string, Set<string>>();
  for (const row of rows) {
    const current = byKey.get(row.key);
    if (!current) {
      byKey.set(row.key, { ...row, tokens: { ...row.tokens }, models: [...row.models], tags: row.tags ? { ...row.tags } : undefined });
      providers.set(row.key, new Set(row.provider ? [row.provider] : []));
      continue;
    }
    addRow(current, row);
    if (row.provider) providers.get(row.key)?.add(row.provider);
    if (providers.get(row.key)?.size !== 1) current.provider = undefined;
    if (!current.sessionId && row.sessionId) current.sessionId = row.sessionId;
    if (!current.providerSessionId && row.providerSessionId) current.providerSessionId = row.providerSessionId;
  }
  return [...byKey.values()];
}

function sortRows(rows: UsageRow[], group: UsageReport['group']): UsageRow[] {
  if (group === 'daily' || group === 'weekly' || group === 'monthly') {
    return rows.sort((left, right) => left.key.localeCompare(right.key));
  }
  return rows.sort((left, right) => right.costUSD - left.costUSD || left.key.localeCompare(right.key));
}

export function combineFleetUsage(sources: FleetUsageSource[]): FleetUsageSummary {
  const reports = sources.flatMap((source) => source.report ? [source.report] : []);
  if (reports.length === 0) {
    return {
      report: null,
      configuredMachines: sources.length,
      reportingMachines: 0,
      missingMachines: missingMachines(sources),
      // Nothing has answered, so nothing is known about deduplication yet.
      olderHostMachines: [],
      exactDeduplication: false,
      duplicatesRemoved: 0
    };
  }

  const exactDeduplication = reports.every((report) => report.eventsIncluded === true);
  const olderHostMachines = sources
    .filter((source) => source.report && source.report.eventsIncluded !== true)
    .map((source) => source.serverName);
  let rows: UsageRow[];
  let duplicatesRemoved = 0;
  if (exactDeduplication) {
    const events = new Map<string, UsageEvent>();
    let observed = 0;
    for (const report of reports) {
      for (const event of report.events ?? []) {
        observed += 1;
        const current = events.get(event.eventKey);
        events.set(event.eventKey, current ? richerEvent(current, event) : event);
      }
    }
    duplicatesRemoved = observed - events.size;
    rows = mergeRows([...events.values()].map(rowFromEvent));
  } else {
    // Mixed-version Fleets still get useful totals. The UI labels this as a
    // machine sum because older daemons do not expose stable event identities.
    rows = mergeRows(reports.flatMap((report) => report.rows));
  }

  const first = reports[0];
  const totals = emptyRow('total');
  for (const row of rows) addRow(totals, row);
  // Indexed from the end rather than the newer accessor for it, which arrived
  // in iOS 15.4 — later than the iOS this app is built for.
  const stamps = reports.map((report) => report.generatedAt).sort();
  const generatedAt = stamps[stamps.length - 1] ?? first.generatedAt;
  return {
    report: {
      ...first,
      machine: 'Fleet',
      generatedAt,
      rows: sortRows(rows, first.group),
      totals,
      eventsIncluded: exactDeduplication,
      events: undefined,
      scan: reports.reduce((total, report) => ({
        filesSeen: total.filesSeen + report.scan.filesSeen,
        filesRead: total.filesRead + report.scan.filesRead,
        linesRead: total.linesRead + report.scan.linesRead,
        entriesSeen: total.entriesSeen + report.scan.entriesSeen
      }), { filesSeen: 0, filesRead: 0, linesRead: 0, entriesSeen: 0 })
    },
    configuredMachines: sources.length,
    reportingMachines: reports.length,
    missingMachines: missingMachines(sources),
    olderHostMachines,
    exactDeduplication,
    duplicatesRemoved
  };
}
