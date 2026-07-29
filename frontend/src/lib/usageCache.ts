import { fetchUsageForServer, type UsageOptions, type UsageReport } from '../api/sessionsd';
import { getServer } from './servers';

export const DEFAULT_USAGE_OPTIONS: UsageOptions = { group: 'daily', mode: 'auto' };

interface UsageCacheEntry {
  report?: UsageReport;
  loadedAt?: number;
  pending?: Promise<UsageReport>;
}

const CACHE_MAX_AGE_MS = 30_000;
const cache = new Map<string, UsageCacheEntry>();

function requestKey(serverId: string, options: UsageOptions): string {
  return JSON.stringify([
    serverId,
    options.group,
    options.mode,
    options.provider ?? '',
    options.since ?? '',
    options.until ?? '',
    options.dimension ?? ''
  ]);
}

export function getCachedUsage(serverId: string, options: UsageOptions): UsageReport | null {
  return cache.get(requestKey(serverId, options))?.report ?? null;
}

export function requestUsageReport(
  serverId: string,
  options: UsageOptions,
  force = false
): Promise<UsageReport> {
  const key = requestKey(serverId, options);
  const entry = cache.get(key) ?? {};
  if (entry.pending) return entry.pending;
  if (!force && entry.report && entry.loadedAt && Date.now() - entry.loadedAt < CACHE_MAX_AGE_MS) {
    return Promise.resolve(entry.report);
  }

	const server = getServer(serverId);
	const pending = fetchUsageForServer(server, options)
    .then((report) => {
      cache.set(key, { report, loadedAt: Date.now() });
      return report;
    })
    .catch((error: unknown) => {
      cache.set(key, { report: entry.report, loadedAt: entry.loadedAt });
      throw error;
    });
  cache.set(key, { ...entry, pending });
  return pending;
}

// App startup calls this after the local daemon is connected. It refreshes
// the default local usage index without making the user open Usage or press a
// button; the dashboard then adopts the warmed report immediately.
export function preloadUsage(serverId: string): Promise<UsageReport> {
  return requestUsageReport(serverId, DEFAULT_USAGE_OPTIONS, true);
}
