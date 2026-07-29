// Deriving whether a Claude Code session is actively in a turn.
//
// Neither of the obvious signals works:
//   - byte-rate (recentBytes): a custom statusline (e.g. the user's
//     `/goal active (3d)◎`) repaints continuously while idle, so the PTY
//     never goes quiet — `working` would be pinned true forever.
//   - JSONL append activity: Claude writes nothing to its conversation
//     JSONL for *minutes* mid-turn (measured user→assistant gaps of
//     200s+), and idle sessions sit parked mid-tool-loop with no
//     terminal "turn closed" event. So JSONL is silent during long turns
//     (→ false idle) and never cleanly signals done (→ stuck working).
//
// The one honest signal is Claude's own footer: it renders
// "· esc to interrupt" in the bottom status line for the entire duration
// of a turn (quiet thinking AND streaming), and switches to "· ← for
// agents" the instant it returns to the prompt. We read it off the
// headless xterm mirror prettyd already feeds for snapshots.
//
// Why the middot anchor + footer scoping: the literal phrase "esc to
// interrupt" also shows up in conversation text (e.g. a dev session
// discussing this very code). The working footer always separates it
// with a middot ("(shift+tab to cycle) · esc to interrupt"), which prose
// doesn't; and we only look at the last few lines, where the footer
// lives.

// SGR / OSC / charset escape sequences emitted by SerializeAddon.
const ANSI = /\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07]*(?:\x07|\x1b\\)|\x1b[()][AB0]/g;

// PRIMARY signal — Claude's live working spinner: a gerund line ending in
// "…" immediately followed by a parenthesized running timer, e.g.
//   ✻ Honking… (3m53s · ↓ 15.4k tokens)
//   ✽ Canoodling… (2m 30s · almost done thinking)
// Present for the entire turn (quiet thinking AND streaming — the timer
// just keeps ticking) and absent when idle: a finished turn reads
// "✻ Cooked for 2m 33s" ("for", not "…("), and the bare prompt has no
// spinner at all. Crucially this lives ABOVE the input box, so it
// survives the frontend's "Remote Control active" footer overlay, which
// clobbers the "esc to interrupt" hint. The "(<n><unit>" guard (a digit
// then h/m/s right after the paren) rejects non-timer ellipsis lines like
// "Reading 1 file… (ctrl+o to expand)".
const WORKING_SPINNER = /(?:…|\.\.\.)\s*\(\s*\d+\s*[hms]/;

// SECONDARY signal — the footer interrupt hint. Catches the brief
// turn-start before the timer renders, and non-remote-controlled
// sessions. Middot-anchored + footer-scoped so the phrase appearing in
// conversation text (e.g. a dev session discussing this very code)
// doesn't read as working. Whitespace-tolerant for soft-wrap/reflow.
const WORKING_FOOTER = /[·•∙]\s*esc\s+to\s+interrupt/i;

export function stripAnsi(s: string): string {
  return s.replace(ANSI, '');
}

// True iff the serialized terminal snapshot shows Claude actively in a
// turn. `serialized` is expected to be the current viewport (the decay
// loop passes serialize({scrollback:0})).
export function claudeWorkingFromSnapshot(serialized: string): boolean {
  if (!serialized) return false;
  const clean = stripAnsi(serialized);
  // The spinner can sit anywhere in the lower viewport (queued input
  // pushes it up), so scan the whole current screen for it.
  if (WORKING_SPINNER.test(clean)) return true;
  // Footer hint: scope to the last few non-empty lines (the footer wraps
  // at narrow widths) so prose mentions don't count.
  const lines = clean
    .split('\n')
    .map((l) => l.replace(/\s+$/, ''))
    .filter((l) => l.length > 0);
  if (lines.length === 0) return false;
  return WORKING_FOOTER.test(lines.slice(-6).join('\n'));
}
