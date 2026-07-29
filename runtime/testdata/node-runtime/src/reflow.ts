// ANSI-aware soft-wrap.
//
// The PTY runs at a fixed wide width (config.defaultCols, e.g. 300). Each
// browser client has its own viewport width — phone is ~50 cols, mac
// split-pane is ~120, mac fullscreen is ~200. The Pretty/Reflowed views
// fetch a snapshot reflowed to their own width so visible-column counts
// are right and prose wraps cleanly without horizontal scroll.
//
// Why server-side: the alternative — reflow in the browser — means every
// client carries its own xterm-headless mirror just to re-flow text, and
// the parser sees layouts that depend on viewport. Server-side reflow
// gives every client the same canonical text just at different widths.
//
// Constraints we respect:
//   • Box-drawing lines (└ ├ ─ │ etc.) are NEVER wrapped — they're
//     drawn deliberately, and word-wrapping them shreds the diagram.
//   • Tables (multi-column lines with 2+ space-runs) are preserved.
//     A user squinting at narrow output can horizontal-scroll; that
//     beats columns being interleaved into prose.
//   • SGR (color/style) state survives line breaks: each output line
//     ends with \x1b[0m and continuation lines re-apply the active SGR.

const SGR_RE = /^\x1b\[[0-9;]*m/;
// CSI parameter+intermediate+final byte. Final byte is the operation.
const CSI_RE = /^\x1b\[[0-?]*[ -/]*[@-~]/;
// OSC strings (title, hyperlink, etc.) terminated by BEL.
const OSC_RE = /^\x1b\][^\x07]*\x07/;

const BOX_DRAWING_RE = /[─-╿▀-▟]/;

export interface ReflowOptions {
  width: number;
  // When true (default), wrapped lines align under the original line's
  // leading whitespace. List bullets feel right with this on.
  preserveIndent?: boolean;
  tabWidth?: number;
}

// Strip every escape sequence so visible-column math works on plain text.
export function stripAnsi(s: string): string {
  let out = '';
  let i = 0;
  while (i < s.length) {
    if (s[i] === '\x1b') {
      const tail = s.slice(i);
      const m = CSI_RE.exec(tail) ?? OSC_RE.exec(tail);
      if (m) { i += m[0].length; continue; }
      i++;
      continue;
    }
    out += s[i++];
  }
  return out;
}

// Expand `\x1b[<N>C` (cursor-forward) into N spaces. SerializeAddon emits
// these for runs of empty cells — without expansion the parser sees the
// next non-space chunk butted right up against the previous one.
export function expandCursorForward(s: string): string {
  return s.replace(/\x1b\[(\d+)C/g, (_m, n) => ' '.repeat(parseInt(n, 10)));
}

// Visible width of a single code point. Approximation good enough for
// terminal output: ASCII = 1, CJK / wide ranges = 2, combining = 0.
function charWidth(ch: string): number {
  const cp = ch.codePointAt(0);
  if (cp === undefined) return 0;
  if (cp === 0) return 0;
  // C0 / C1 controls — should not appear, but skip width-wise.
  if (cp < 0x20) return 0;
  if (cp >= 0x7f && cp < 0xa0) return 0;
  // Combining marks (a tiny but common subrange).
  if (cp >= 0x0300 && cp <= 0x036f) return 0;
  // Wide ranges (CJK + Hangul + emoji bulk). Coarse: don't bother
  // pulling in a full East-Asian-Width table.
  if (
    (cp >= 0x1100 && cp <= 0x115f) ||
    (cp >= 0x2e80 && cp <= 0x303e) ||
    (cp >= 0x3041 && cp <= 0x33ff) ||
    (cp >= 0x3400 && cp <= 0x4dbf) ||
    (cp >= 0x4e00 && cp <= 0x9fff) ||
    (cp >= 0xa000 && cp <= 0xa4cf) ||
    (cp >= 0xac00 && cp <= 0xd7a3) ||
    (cp >= 0xf900 && cp <= 0xfaff) ||
    (cp >= 0xfe30 && cp <= 0xfe4f) ||
    (cp >= 0xff00 && cp <= 0xff60) ||
    (cp >= 0xffe0 && cp <= 0xffe6) ||
    (cp >= 0x1f300 && cp <= 0x1faff) ||
    (cp >= 0x20000 && cp <= 0x3fffd)
  ) return 2;
  return 1;
}

function visibleWidth(s: string): number {
  let w = 0;
  for (const ch of s) w += charWidth(ch);
  return w;
}

// Heuristic: should this line be left alone?
//   • Already fits within the target width.
//   • Box-drawing chars → deliberate diagram / border.
//   • Pipe-table row → row of `| col | col | col |` cells. Wrapping
//     these mid-cell shreds the table; preserving them lets the user
//     horizontal-scroll a wide table instead of seeing garbled cells.
//
// We previously tried a broader "columnar" heuristic (multiple ≥2-space
// gaps) but it false-fired on Claude's centered prose and made
// everything stay at 300 cols. The pipe-row check is much narrower:
// real prose almost never has `| col | col | col |`-shaped lines.
export function shouldPreserveLine(plain: string, width: number): boolean {
  if (visibleWidth(plain) <= width) return true;
  if (BOX_DRAWING_RE.test(plain)) return true;
  if (isPipeTableRow(plain)) return true;
  return false;
}

// True when the line looks like a pipe-separated table row. Claude's
// security/review tables come in two shapes:
//   • Markdown:   `| Brand | Format | Size |` (encloses both ends)
//   • Code review: `  361  | 5 | issue text | severity | owner |` (line
//                   number first, no leading `|`, but always closes with `|`)
// Heuristic: trim trailing whitespace, require ≥3 `|`, and the line
// must end with `|`. That keeps prose with a single inline `|` (like
// shell pipes) from getting protected.
function isPipeTableRow(plain: string): boolean {
  const trimmed = plain.replace(/\s+$/, '');
  if (!trimmed.endsWith('|')) return false;
  let count = 0;
  for (let i = 0; i < trimmed.length; i++) {
    if (trimmed[i] === '|') count++;
    if (count >= 3) return true;
  }
  return false;
}

// Tokenize a single line (no \r\n) into a flat stream of glyphs with each
// glyph carrying the SGR state active when it was drawn. We drop non-SGR
// escapes — they're either cursor-positioning (already consumed by the
// pre-pass) or harmless.
interface Glyph {
  sgr: string; // accumulated SGR escapes, '' = default attributes
  ch: string;
  w: number;
}

function tokenizeLine(line: string): { glyphs: Glyph[]; finalSgr: string } {
  const glyphs: Glyph[] = [];
  let sgr = '';
  let i = 0;
  while (i < line.length) {
    const c = line[i]!;
    if (c === '\x1b') {
      const tail = line.slice(i);
      const ms = SGR_RE.exec(tail);
      if (ms) {
        const inner = ms[0].slice(2, -1);
        // \x1b[m and \x1b[0m both reset. Other codes accumulate; we always
        // emit a full \x1b[0m before re-applying so order doesn't matter.
        if (inner === '' || inner === '0') sgr = '';
        else sgr += ms[0];
        i += ms[0].length;
        continue;
      }
      const m = CSI_RE.exec(tail) ?? OSC_RE.exec(tail);
      if (m) { i += m[0].length; continue; }
      i++; // lone ESC, skip
      continue;
    }
    // Use code-point iteration so surrogate pairs stay intact.
    const cp = line.codePointAt(i)!;
    const ch = String.fromCodePoint(cp);
    glyphs.push({ sgr, ch, w: charWidth(ch) });
    i += ch.length;
  }
  return { glyphs, finalSgr: sgr };
}

// Reverse of tokenize: a glyph stream → an ANSI-coded segment of one line.
// Re-applies SGR transitions and ends with a reset if any SGR is active.
function renderGlyphs(glyphs: Glyph[]): string {
  let out = '';
  let active = '';
  for (const g of glyphs) {
    if (g.sgr !== active) {
      if (active !== '') out += '\x1b[0m';
      out += g.sgr;
      active = g.sgr;
    }
    out += g.ch;
  }
  if (active !== '') out += '\x1b[0m';
  return out;
}

// Wrap a glyph stream into chunks each ≤ width. Prefers spaces as break
// points; falls back to a hard break mid-word if a single token exceeds
// width (rare in real output, but possible — long URLs, hashes).
function wrapGlyphs(glyphs: Glyph[], width: number, indentWidth: number): Glyph[][] {
  if (glyphs.length === 0) return [[]];
  const lines: Glyph[][] = [];
  let cur: Glyph[] = [];
  let curW = 0;
  let lastBreak = -1; // index in `cur` of last space we could break at
  let firstLine = true;

  const limit = (): number => firstLine ? width : Math.max(1, width - indentWidth);

  for (const g of glyphs) {
    if (curW + g.w > limit() && cur.length > 0) {
      // Break. Prefer the last space in cur.
      if (lastBreak >= 0) {
        const head = cur.slice(0, lastBreak);
        const tail = cur.slice(lastBreak + 1); // drop the space
        lines.push(head);
        cur = tail;
        curW = tail.reduce((a, b) => a + b.w, 0);
        lastBreak = -1;
        for (let j = 0; j < cur.length; j++) if (cur[j]!.ch === ' ') lastBreak = j;
      } else {
        lines.push(cur);
        cur = [];
        curW = 0;
      }
      firstLine = false;
    }
    cur.push(g);
    curW += g.w;
    if (g.ch === ' ') lastBreak = cur.length - 1;
  }
  lines.push(cur);
  return lines;
}

// Reflow one logical line into one or more output lines. Returns the joined
// result with \r\n separators (no trailing newline).
function reflowLine(line: string, opts: ReflowOptions): string {
  // Floor at 4 — anything smaller is degenerate. Real callers pass ≥30.
  const width = Math.max(4, opts.width | 0);
  const preserveIndent = opts.preserveIndent !== false;

  if (line === '') return '';

  // Handle tabs by expanding to spaces — terminal apps usually don't emit
  // raw tabs, but be defensive.
  const tabW = opts.tabWidth ?? 8;
  if (line.includes('\t')) {
    line = line.replace(/\t/g, ' '.repeat(tabW));
  }

  const plain = stripAnsi(line);

  // PTY blank rows are 300 spaces. Don't bloat them into N copies of
  // the wrap width — collapse trailing whitespace first so they become
  // empty lines.
  if (plain.trim() === '') return '';

  const visibleW = visibleWidth(plain);

  // Lines we leave structurally intact — wider than viewport AND have a
  // visual structure padding/wrapping would shred. They horizontal-
  // scroll. Don't pad — padding would push their right edge further
  // into the scroll region without helping bg rendering for these
  // (they're already wider than the viewport, no gap to fill).
  if (visibleW > width) {
    if (BOX_DRAWING_RE.test(plain)) return line;
    if (isPipeTableRow(plain)) return line;
  }

  // Cap indent capture: a line whose "indent" is wider than half the
  // wrap target isn't really indented, it's centered or padded. Re-applying
  // 200 spaces of "indent" to every continuation produces lines wider than
  // the original.
  const indentMatch = /^(\s*)/.exec(plain);
  let indentText = preserveIndent && indentMatch ? indentMatch[1]! : '';
  if (visibleWidth(indentText) > Math.floor(width / 2)) indentText = '';
  const indentW = visibleWidth(indentText);

  const { glyphs } = tokenizeLine(line);
  const wrapped = wrapGlyphs(glyphs, width, indentW);

  // Pad each chunk out to its target width with space-glyphs that
  // inherit the trailing SGR. Two reasons:
  //   1. Wrap drops the trailing space at break points, so most chunks
  //      are `width - 1` chars; the final chunk can be much shorter.
  //   2. Lines that already fit are also passed through here (single
  //      chunk shorter than width) so they get padded too.
  // Without padding, an ANSI background-color run on a row only fills
  // the rendered text — the rest of the row shows the default
  // background, producing visible "stripes" where the bg should reach
  // the right edge.
  for (let i = 0; i < wrapped.length; i++) {
    const seg = wrapped[i]!;
    const segMax = i === 0 ? width : Math.max(1, width - indentW);
    let segW = 0;
    for (const g of seg) segW += g.w;
    if (segW < segMax && seg.length > 0) {
      const trailingSgr = seg[seg.length - 1]!.sgr;
      while (segW < segMax) {
        seg.push({ sgr: trailingSgr, ch: ' ', w: 1 });
        segW += 1;
      }
    }
  }

  const out: string[] = [];
  for (let i = 0; i < wrapped.length; i++) {
    const seg = renderGlyphs(wrapped[i]!);
    out.push(i === 0 ? seg : indentText + seg);
  }
  return out.join('\r\n');
}

export function reflowAnsi(text: string, opts: ReflowOptions): string {
  // SerializeAddon emits cursor-forward escapes for runs of empty cells.
  // Expand them so the wrap sees real spaces it can break on.
  const normalized = expandCursorForward(text);
  // Split on \r\n preferentially — SerializeAddon uses CRLF — but accept
  // bare \n too so we don't ruin lines that came from a different source.
  const lines = normalized.split(/\r?\n/);
  const out: string[] = [];
  for (const line of lines) {
    out.push(reflowLine(line, opts));
  }
  return out.join('\r\n');
}
