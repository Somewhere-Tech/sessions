// Convert box-drawing tables in Claude/Codex output into GFM markdown
// tables. Runs BEFORE ansiToHtml in the content pipeline so the output
// markdown still contains raw ANSI codes inside cells; later stages
// convert those to spans.
//
// Detection is intentionally conservative — single-line cells, requires
// a ┬-bearing top rule (so plain rectangles or diagrams without column
// dividers stay as preformatted text). Anything that doesn't match
// cleanly is left untouched.

const ANSI_RE = /\x1b\[[0-?]*[ -/]*[@-~]/g;
function stripAnsi(s: string): string { return s.replace(ANSI_RE, ''); }

// Top:    ┌──┬──┬──┐  (must contain ┬ — this is what distinguishes a
//                       column-divider table from a plain rectangle).
// Mid:    ├──┼──┼──┤  (header separator OR between data rows)
// Bottom: └──┴──┴──┘
// Row:    │ … │ … │   (we just require a leading and trailing │)
const TOP_RE = /^\s*┌─+(?:┬─+)+┐\s*$/;
const MID_RE = /^\s*├─+(?:┼─+)+┤\s*$/;
const BOT_RE = /^\s*└─+(?:┴─+)+┘\s*$/;
const ROW_RE = /^\s*│.*│\s*$/;

const SINGLE_ANSI_RE = /^\x1b\[[0-?]*[ -/]*[@-~]/;

// Walk a row and split on `│`, preserving any ANSI codes inside cells.
// Returns cell strings (with ANSI codes intact, leading/trailing
// whitespace trimmed). Returns null if the row doesn't have at least
// two `│` separators (not a real row).
function splitRowPreservingAnsi(rawLine: string): string[] | null {
  const cells: string[] = [];
  let cur = '';
  let i = 0;
  while (i < rawLine.length) {
    if (rawLine[i] === '\x1b') {
      const m = SINGLE_ANSI_RE.exec(rawLine.slice(i));
      if (m) { cur += m[0]; i += m[0].length; continue; }
    }
    if (rawLine[i] === '│') {
      cells.push(cur);
      cur = '';
      i++;
      continue;
    }
    cur += rawLine[i];
    i++;
  }
  // Need at least 3 parts: leading-pad, ≥1 cell, trailing-pad.
  if (cells.length < 2) return null;
  // Drop leading whitespace-before-first-│ and the trailing chunk
  // (which is in `cur`, not pushed — fine, we just don't consider it).
  cells.shift();
  return cells.map((c) => c.trim());
}

interface ParsedTable {
  markdown: string[];
  endIndex: number;  // index AFTER the bottom rule
}

function tryParseTable(lines: string[], startIdx: number): ParsedTable | null {
  const top = stripAnsi(lines[startIdx]!);
  if (!TOP_RE.test(top)) return null;

  // Count columns from the top rule: ┌ + ┬'s + ┐. Cell count = (┬+1).
  const tCount = (top.match(/┬/g) ?? []).length;
  const expectedCols = tCount + 1;
  if (expectedCols < 2) return null;

  const dataRows: string[][] = [];
  let headerSepFound = false;
  let i = startIdx + 1;
  while (i < lines.length) {
    const plain = stripAnsi(lines[i]!);
    if (BOT_RE.test(plain)) {
      if (dataRows.length === 0) return null;
      // Build markdown table.
      const md: string[] = [];
      let header: string[];
      let body: string[][];
      if (headerSepFound && dataRows.length >= 1) {
        header = dataRows[0]!;
        body = dataRows.slice(1);
      } else {
        // No header separator detected — synthesize empty header.
        // GFM tables require a header row; an empty one renders as
        // a thin top divider, which is acceptable.
        header = Array(expectedCols).fill('');
        body = dataRows;
      }
      md.push('| ' + header.map(escapeCell).join(' | ') + ' |');
      md.push('| ' + Array(expectedCols).fill('---').join(' | ') + ' |');
      for (const row of body) {
        // Pad / truncate to expectedCols so column count stays
        // consistent — markdown is strict about that.
        const padded = row.slice(0, expectedCols);
        while (padded.length < expectedCols) padded.push('');
        md.push('| ' + padded.map(escapeCell).join(' | ') + ' |');
      }
      // Markdown needs a blank line before/after to recognize the
      // table when surrounded by other text.
      return { markdown: ['', ...md, ''], endIndex: i + 1 };
    }
    if (MID_RE.test(plain)) {
      // The first separator after a single data row is treated as the
      // header divider. Subsequent ones (between every row in some
      // Claude tables) are ignored.
      if (!headerSepFound && dataRows.length === 1) headerSepFound = true;
      i++;
      continue;
    }
    if (ROW_RE.test(plain)) {
      const cells = splitRowPreservingAnsi(lines[i]!);
      if (cells === null) return null;
      // Reject rows whose cell count doesn't match the top rule. A
      // mismatch usually means the line wasn't really a row of THIS
      // table (e.g., a nested/unrelated structure).
      if (cells.length !== expectedCols) return null;
      dataRows.push(cells);
      i++;
      continue;
    }
    // Anything else inside the table boundaries → bail. Better to
    // leave the original ASCII art alone than emit a broken table.
    return null;
  }
  return null;  // never found the bottom rule
}

function escapeCell(s: string): string {
  // Pipe is the GFM cell separator; escape it. Newlines shouldn't
  // appear in single-line cells but guard anyway.
  return s.replace(/\|/g, '\\|').replace(/\n/g, ' ');
}

// Public entry. Walks raw text linearly, replacing each well-formed
// box-drawing table with markdown. All other lines pass through
// untouched.
export function boxDrawingTablesToMarkdown(raw: string): string {
  if (!raw.includes('┌')) return raw;  // fast path
  const lines = raw.split('\n');
  const out: string[] = [];
  let i = 0;
  while (i < lines.length) {
    const parsed = tryParseTable(lines, i);
    if (parsed === null) {
      out.push(lines[i]!);
      i++;
      continue;
    }
    out.push(...parsed.markdown);
    i = parsed.endIndex;
  }
  return out.join('\n');
}
