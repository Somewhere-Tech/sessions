// What the harness put in the transcript, and what the person actually wrote.
//
// Reported by the founder on 11 September: a structured Claude session showed
// `<task-notification>` blocks, `<system-reminder>` blocks and a queue marker as
// the person's own chat bubbles, each with a timestamp and a copy button, while
// Claude's own Remote Control showed none of them. They are user-role records in
// the provider's JSONL — that is how Claude Code delivers a background task's
// result or a reminder to the model — so a reader that trusts the role renders
// machinery as somebody's words.
//
// The role is not the question. What wrote it is. This module answers that from
// the text, and it is the one place that does, so the transcript, the search
// snippet and the inbox line cannot disagree about who spoke.

export type HarnessKind = 'task-notification' | 'system-reminder' | 'system-notification' | 'queued';

export interface HarnessBlock {
  kind: HarnessKind;
  /** One quiet line: what happened, in the fewest words that are still true. */
  summary: string;
  /** The block itself, for the person who wants to see exactly what arrived. */
  detail: string;
}

export interface HarnessSplit {
  /** What the person wrote, with the harness blocks taken out. */
  personText: string;
  /** The blocks that were mixed in, in the order they appeared. */
  blocks: HarnessBlock[];
}

interface Shape {
  kind: HarnessKind;
  open: RegExp;
  close?: string;
  label: string;
}

// The shapes Claude Code injects as user-role records. `open` is anchored, so a
// person quoting one of these mid-sentence keeps their message.
const SHAPES: Shape[] = [
  { kind: 'task-notification', open: /<task-notification>/, close: '</task-notification>', label: 'Background task' },
  { kind: 'system-reminder', open: /<system-reminder>/, close: '</system-reminder>', label: 'System reminder' },
  // "[SYSTEM NOTIFICATION] …" is one line, and the text after the bracket
  // belongs to the notification: closing at the first ']' would leave the
  // notification's own words looking like something the person typed.
  { kind: 'system-notification', open: /\[SYSTEM NOTIFICATION/, label: 'System notification' },
  // Sessions' own marker for a send the provider has not picked up yet. It
  // belongs on the composer, where the person is waiting, not in the history of
  // what was said.
  { kind: 'queued', open: /⏳\s*queued\b/, label: 'Queued' }
];

/**
 * Split one transcript entry into the person's words and the harness blocks
 * around them.
 *
 * A block runs from its opening marker to its closing one; an unterminated
 * block runs to the end of the entry, because a truncated reminder is still not
 * something the person typed.
 */
export function splitHarnessContent(text: string): HarnessSplit {
  const blocks: HarnessBlock[] = [];
  let remaining = text;
  let person = '';
  for (let guard = 0; guard < 64; guard += 1) {
    const next = nextShape(remaining);
    if (!next) break;
    person += remaining.slice(0, next.start);
    const end = blockEnd(remaining, next.shape, next.start);
    const detail = remaining.slice(next.start, end).trim();
    blocks.push({ kind: next.shape.kind, summary: summarize(next.shape, detail), detail });
    remaining = remaining.slice(end);
  }
  return { personText: (person + remaining).trim(), blocks };
}

/** True when the entry is machinery and nothing else. */
export function isHarnessOnly(text: string): boolean {
  if (!text.trim()) return false;
  const split = splitHarnessContent(text);
  return split.blocks.length > 0 && split.personText === '';
}

/** The single line a system event is shown as. */
export function harnessLine(blocks: HarnessBlock[]): string {
  if (blocks.length === 0) return '';
  const first = blocks[0]!;
  if (blocks.length === 1) return first.summary;
  return `${first.summary} · +${blocks.length - 1} more`;
}

/**
 * How one transcript line should be shown, for any surface that shows lines:
 * the transcript, a search snippet, a preview, the inbox's last message.
 *
 * `speaker` is what the surface should call it — the person only when the
 * person wrote it. `text` is what to print.
 */
export function harnessDisplay(
  text: string, personLabel: string
): { speaker: string; text: string; isHarness: boolean } {
  const split = splitHarnessContent(text);
  if (split.blocks.length === 0) return { speaker: personLabel, text, isHarness: false };
  if (!split.personText) return { speaker: 'System event', text: harnessLine(split.blocks), isHarness: true };
  return { speaker: personLabel, text: split.personText, isHarness: false };
}

function nextShape(text: string): { shape: Shape; start: number } | null {
  let found: { shape: Shape; start: number } | null = null;
  for (const shape of SHAPES) {
    const match = shape.open.exec(text);
    if (!match) continue;
    if (!found || match.index < found.start) found = { shape, start: match.index };
  }
  return found;
}

function blockEnd(text: string, shape: Shape, start: number): number {
  if (!shape.close) {
    const lineEnd = text.indexOf('\n', start);
    return lineEnd < 0 ? text.length : lineEnd + 1;
  }
  const closing = text.indexOf(shape.close, start);
  // An unterminated block runs to the end: a truncated reminder is still not
  // the person's message, and showing its tail as chat would be the bug.
  return closing < 0 ? text.length : closing + shape.close.length;
}

// The first useful line inside the block, so a background task says which task
// finished rather than only that one did.
function summarize(shape: Shape, detail: string): string {
  const inner = detail
    .replace(/<\/?[a-z-]+>/gi, ' ')
    .replace(/^\s*\[SYSTEM NOTIFICATION[:\s]*/i, ' ')
    .replace(/\]\s*$/, ' ')
    .replace(/⏳\s*/u, ' ');
  const line = inner.split('\n').map((part) => part.trim()).find((part) => part.length > 0) ?? '';
  if (!line) return shape.label;
  const bounded = line.length > 120 ? `${line.slice(0, 119).trimEnd()}…` : line;
  return `${shape.label}: ${bounded}`;
}
