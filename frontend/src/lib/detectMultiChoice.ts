// Detect Claude Code's numbered-choice picker. The TUI surfaces this
// whenever Claude wants the user to pick one of N alternatives — e.g.
// "How should the 'Book a workflow audit' CTA work?" with a numbered
// list of options. The picker requires arrow-keys + Enter to interact;
// from Remote view that means typing a number into the text box won't
// work, so we surface clickable buttons that send the right keystrokes.
//
// Detection signature is the footer string Claude prints at the bottom
// of the picker:
//   "Enter to select · ↑/↓ to navigate · Esc to cancel"
// That phrasing is consistent across Claude versions. From there we
// walk upward, collecting `<glyph> N. <title>` lines as options (with
// `❯` marking the highlighted one) and treating any non-option lines
// above the first option as the question/title.

export interface MultiChoiceOption {
  number: number;
  text: string;
  description: string;
}

export interface MultiChoice {
  question: string;
  options: MultiChoiceOption[];
  // Index into `options` of whichever has the `❯` highlight marker.
  // Used to compute arrow-key delta when the user clicks a different
  // option.
  selectedIndex: number;
}

export type SnapshotComposerStateKind =
  | 'normal-composer'
  | 'numbered-picker'
  | 'trust-prompt'
  | 'update/notice-banner'
  | 'unknown-blocking';

export interface SnapshotComposerState {
  kind: SnapshotComposerStateKind;
  title: string;
  description: string;
}

const FOOTER_RE = /Enter to select.*[↑↓].*to navigate/;
const OPTION_RE = /^(\s*)([❯>])?\s*(\d+)\.\s+(.+?)\s*$/;
const RULE_RE = /^\s*─{4,}\s*$/;
const CURSOR_FORWARD_RE = /\x1b\[(\d+)C/g;
const GENERIC_NUMBERED_OPTION_RE = /^\s*(?:[❯>]\s*)?\d+[\.)]\s+\S.+$/;
const TRUST_PROMPT_RE = /\b(?:do you trust|trust (?:this|the)|trusted (?:folder|directory|workspace|project)|trust the files|only grant access to directories you trust)\b/i;
const TRUST_CONTEXT_RE = /\b(?:folder|directory|workspace|project|files in this)\b/i;
const UPDATE_NOTICE_RE = /\b(?:update available|new version|latest version|release notes|what'?s new|restart to update|install update|update now|press enter to continue|notice)\b/i;
const BLOCKING_PROMPT_RE = /\b(?:press enter|hit enter|continue\?|confirm|are you sure|allow|deny|approve|permission|yes\/no|\[y\/n\]|\(y\/n\)|select|choose)\b/i;

function stripAnsi(s: string): string {
  return s
    .replace(CURSOR_FORWARD_RE, (_, n: string) => ' '.repeat(parseInt(n, 10)))
    .replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, '')
    .replace(/\x1b\][^\x07]*\x07/g, '');
}

function cleanSnapshot(rawSnapshot: string): string {
  return stripAnsi(rawSnapshot).replace(/\r/g, '');
}

function tailLines(rawSnapshot: string, maxLines: number): string[] {
  const lines = cleanSnapshot(rawSnapshot).split('\n');
  while (lines.length > 0 && lines[lines.length - 1]!.trim() === '') lines.pop();
  return lines.slice(-maxLines).map((line) => line.trimEnd());
}

function hasGenericNumberedMenu(rawSnapshot: string): boolean {
  const lines = tailLines(rawSnapshot, 44);
  let optionCount = 0;
  let hasSelectionMarker = false;
  let hasPickerLanguage = false;
  for (const line of lines) {
    const trimmed = line.trim();
    if (GENERIC_NUMBERED_OPTION_RE.test(trimmed)) optionCount++;
    if (/^\s*[❯>]\s*\d+[\.)]\s+\S/.test(line)) hasSelectionMarker = true;
    if (/\b(?:enter to select|navigate|select|choose|resume|continue|esc to cancel)\b/i.test(trimmed)) {
      hasPickerLanguage = true;
    }
  }
  return optionCount >= 2 && (hasSelectionMarker || hasPickerLanguage);
}

export function classifySnapshotComposerState(rawSnapshot: string): SnapshotComposerState {
  const tail = tailLines(rawSnapshot, 44);
  const tailText = tail.join('\n');
  const fullText = cleanSnapshot(rawSnapshot);

  if (detectMultiChoice(rawSnapshot) || hasGenericNumberedMenu(rawSnapshot)) {
    return {
      kind: 'numbered-picker',
      title: 'Menu or picker is open',
      description: 'This session is showing a menu or picker, not a text box.'
    };
  }

  if (TRUST_PROMPT_RE.test(tailText) && TRUST_CONTEXT_RE.test(tailText)) {
    return {
      kind: 'trust-prompt',
      title: 'Trust prompt is open',
      description: 'This session is asking whether to trust a folder or workspace.'
    };
  }

  if (UPDATE_NOTICE_RE.test(tailText)) {
    return {
      kind: 'update/notice-banner',
      title: 'Notice banner is open',
      description: 'This session is showing an update or notice banner before it will accept chat input.'
    };
  }

  if (BLOCKING_PROMPT_RE.test(tailText) && fullText.trim().length > 0) {
    return {
      kind: 'unknown-blocking',
      title: 'Interactive prompt is open',
      description: 'This session appears to be waiting on a terminal prompt instead of accepting a chat message.'
    };
  }

  return {
    kind: 'normal-composer',
    title: 'Composer appears available',
    description: 'No blocking menu or prompt was detected in the terminal snapshot.'
  };
}

export function detectMultiChoice(rawSnapshot: string): MultiChoice | null {
  const lines = rawSnapshot.split(/\r?\n/);

  // Locate the footer near the end. Walk from the bottom up; bail if
  // we don't see it in the last ~12 non-empty lines (i.e. the footer
  // isn't currently visible → no picker active).
  let footerIdx = -1;
  let scanned = 0;
  for (let i = lines.length - 1; i >= 0 && scanned < 30; i--) {
    const plain = stripAnsi(lines[i]!).trim();
    if (plain.length === 0) continue;
    scanned++;
    if (FOOTER_RE.test(plain)) {
      footerIdx = i;
      break;
    }
  }
  if (footerIdx === -1) return null;

  // From just above the footer, walk upward collecting options. Options
  // come in two forms:
  //   "❯ 1. Title here"         (highlighted)
  //   "  2. Title here"         (not highlighted)
  // followed by optional indented description lines BELOW the option
  // (Claude's layout is title-then-description, indented further).
  //
  // Walking upward means we see description lines BEFORE the option
  // they belong to. So we buffer pending description lines and attach
  // them to the next option we hit when going up.
  //
  // Rule lines (─────) can appear between options (Claude sometimes
  // separates "Chat about this" / "Type something" from the rest).
  // Skip them.
  const optionByNum = new Map<number, { selected: boolean; text: string; descLines: string[]; lineIdx: number }>();
  let earliestOptionLine = footerIdx;
  let pendingDescs: string[] = [];

  for (let i = footerIdx - 1; i >= 0; i--) {
    const plain = stripAnsi(lines[i]!);
    const trimmed = plain.trim();
    if (trimmed.length === 0) {
      // Blank breaks the "this description belongs to the option just
      // above" chain — descriptions are contiguous indented lines
      // right under their title.
      pendingDescs = [];
      continue;
    }
    if (RULE_RE.test(plain)) {
      pendingDescs = [];
      continue;
    }

    const m = plain.match(OPTION_RE);
    if (m) {
      const glyph = m[2] ?? '';
      const num = parseInt(m[3]!, 10);
      const text = m[4]!.trim();
      if (!optionByNum.has(num)) {
        optionByNum.set(num, {
          selected: glyph === '❯',
          text,
          descLines: pendingDescs.slice(),
          lineIdx: i
        });
      }
      pendingDescs = [];
      earliestOptionLine = i;
      continue;
    }

    // Indented continuation — description for the option ABOVE (the
    // next option we'll hit going up). Buffer until then.
    if (/^\s{4,}\S/.test(plain)) {
      pendingDescs.unshift(trimmed);
      continue;
    }

    // Hit a non-option, non-description, non-rule line. That's the
    // question / title. Stop scanning further upward.
    if (optionByNum.size > 0) break;
  }

  if (optionByNum.size === 0) return null;

  // Walk a few lines above the first option, collect non-option /
  // non-rule lines as the question. Stop at the first rule line we
  // hit (separates from earlier conversation).
  const questionLines: string[] = [];
  for (let i = earliestOptionLine - 1; i >= Math.max(0, earliestOptionLine - 6); i--) {
    const plain = stripAnsi(lines[i]!);
    const trimmed = plain.trim();
    if (trimmed.length === 0) continue;
    if (RULE_RE.test(plain)) break;
    if (OPTION_RE.test(plain)) break;
    // Strip leading marker glyphs Claude sometimes prepends to the
    // title ("☐ Booking flow") — they aren't useful in our button UI.
    questionLines.unshift(trimmed.replace(/^[☐☑◻◼]\s+/, ''));
  }
  const question = questionLines.join(' — ').trim();

  // Build options array sorted by number.
  const numbers = Array.from(optionByNum.keys()).sort((a, b) => a - b);
  const options: MultiChoiceOption[] = numbers.map((n) => {
    const opt = optionByNum.get(n)!;
    return { number: n, text: opt.text, description: opt.descLines.join(' ') };
  });
  const selectedIndex = options.findIndex((o) => optionByNum.get(o.number)!.selected);

  return {
    question: question || '(choose one)',
    options,
    selectedIndex: selectedIndex >= 0 ? selectedIndex : 0
  };
}
