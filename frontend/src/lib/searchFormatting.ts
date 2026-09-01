import { relativeDate } from './conversationBrowser';
import type { SearchMatch } from '../api/sessionsd';

function timestampValue(value: string | null): number {
  if (!value) return 0;
  const parsed = new Date(value).getTime();
  return Number.isNaN(parsed) ? 0 : parsed;
}

export function rankedMatchLabel(score: number): string {
  if (score >= 0.85) return 'Best match';
  if (score >= 0.5) return 'Strong match';
  return 'Related';
}

export function operationLabel(kind?: SearchMatch['kind']): string {
  if (kind === 'delegation') return 'Delegation';
  if (kind === 'handoff') return 'Handoff';
  if (kind === 'automation') return 'Automation';
  if (kind === 'status') return 'Status';
  return 'Operation';
}

export function formatHitSpan(first: string | null, last: string | null): { label: string; title: string } | null {
  const start = first && timestampValue(first) ? first : null;
  const end = last && timestampValue(last) ? last : null;
  if (!start && !end) return null;
  const startDay = start ? calendarDay(start) : '';
  const endDay = end ? calendarDay(end) : '';
  if (!start || !end || startDay === endDay) {
    const only = (end ?? start) as string;
    return { label: relativeDate(only), title: `Matched here on ${relativeDate(only)}` };
  }
  return {
    label: `${shortDate(start)} → ${shortDate(end)}`,
    title: `Matches here run from ${relativeDate(start)} to ${relativeDate(end)}`
  };
}

function calendarDay(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toDateString();
}

function shortDate(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
}
