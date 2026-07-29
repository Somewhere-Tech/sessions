import { fetchRecap, fetchRecapDates, type RecapDay } from '../api/sessionsd';

interface DayEntry {
  day?: RecapDay;
  loadedAt?: number;
  pending?: Promise<RecapDay>;
}

interface DatesEntry {
  dates?: string[];
  loadedAt?: number;
  pending?: Promise<string[]>;
}

const CACHE_MAX_AGE_MS = 30_000;
const dayCache = new Map<string, DayEntry>();
const datesCache = new Map<string, DatesEntry>();

export function currentLocalDate(date = new Date()): string {
  const year = date.getFullYear();
  const month = String(date.getMonth() + 1).padStart(2, '0');
  const day = String(date.getDate()).padStart(2, '0');
  return `${year}-${month}-${day}`;
}

function dayKey(serverId: string, date: string): string {
  return `${serverId}:${date}`;
}

export function getCachedDailyDay(serverId: string, date: string): RecapDay | null {
  return dayCache.get(dayKey(serverId, date))?.day ?? null;
}

export function rememberDailyDay(serverId: string, day: RecapDay): void {
  dayCache.set(dayKey(serverId, day.date), { day, loadedAt: Date.now() });
}

export function requestDailyDay(serverId: string, date: string, force = false): Promise<RecapDay> {
  const key = dayKey(serverId, date);
  const entry = dayCache.get(key) ?? {};
  if (entry.pending) return entry.pending;
  if (!force && entry.day && entry.loadedAt && Date.now() - entry.loadedAt < CACHE_MAX_AGE_MS) {
    return Promise.resolve(entry.day);
  }
  const pending = fetchRecap(date)
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

export function getCachedRecapDates(serverId: string): string[] {
  return datesCache.get(serverId)?.dates ?? [];
}

export function rememberRecapDate(serverId: string, date: string): string[] {
  const current = datesCache.get(serverId)?.dates ?? [];
  const dates = Array.from(new Set([date, ...current])).sort().reverse();
  datesCache.set(serverId, { dates, loadedAt: Date.now() });
  return dates;
}

export function requestRecapDates(serverId: string, force = false): Promise<string[]> {
  const entry = datesCache.get(serverId) ?? {};
  if (entry.pending) return entry.pending;
  if (!force && entry.dates && entry.loadedAt && Date.now() - entry.loadedAt < CACHE_MAX_AGE_MS) {
    return Promise.resolve(entry.dates);
  }
  const pending = fetchRecapDates()
    .then((dates) => {
      datesCache.set(serverId, { dates, loadedAt: Date.now() });
      return dates;
    })
    .catch((error: unknown) => {
      datesCache.set(serverId, { dates: entry.dates, loadedAt: entry.loadedAt });
      throw error;
    });
  datesCache.set(serverId, { ...entry, pending });
  return pending;
}

export async function preloadDaily(serverId: string, date = currentLocalDate()): Promise<void> {
  await Promise.allSettled([
    requestDailyDay(serverId, date, true),
    requestRecapDates(serverId, true)
  ]);
}
