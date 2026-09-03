import { fetchDaily, type DailyDay } from '../api/sessionsd';

interface DayEntry {
  day?: DailyDay;
  loadedAt?: number;
  pending?: Promise<DailyDay>;
}

const CACHE_MAX_AGE_MS = 30_000;
const dayCache = new Map<string, DayEntry>();

export function currentLocalDate(date = new Date()): string {
  const year = date.getFullYear();
  const month = String(date.getMonth() + 1).padStart(2, '0');
  const day = String(date.getDate()).padStart(2, '0');
  return `${year}-${month}-${day}`;
}

function dayKey(serverId: string, date: string): string {
  return `${serverId}:${date}`;
}

export function getCachedDailyDay(serverId: string, date: string): DailyDay | null {
  return dayCache.get(dayKey(serverId, date))?.day ?? null;
}

export function rememberDailyDay(serverId: string, day: DailyDay): void {
  dayCache.set(dayKey(serverId, day.date), { day, loadedAt: Date.now() });
}

export function requestDailyDay(serverId: string, date: string, force = false): Promise<DailyDay> {
  const key = dayKey(serverId, date);
  const entry = dayCache.get(key) ?? {};
  if (entry.pending) return entry.pending;
  if (!force && entry.day && entry.loadedAt && Date.now() - entry.loadedAt < CACHE_MAX_AGE_MS) {
    return Promise.resolve(entry.day);
  }
  const pending = fetchDaily(date)
    .then((day) => {
      rememberDailyDay(serverId, day);
      return day;
    })
    .catch((error: unknown) => {
      dayCache.set(key, { day: entry.day, loadedAt: entry.loadedAt });
      throw error;
    });
  dayCache.set(key, { ...entry, pending });
  return pending;
}

export async function preloadDaily(serverId: string, date = currentLocalDate()): Promise<void> {
  await Promise.allSettled([requestDailyDay(serverId, date, true)]);
}
