// ANSI escape handling for tmux capture-pane -e output.
//
// Two responsibilities:
//  1. stripAnsi() — remove ANSI codes for pattern matching / clean clipboard.
//  2. ansiToHtml() — turn ANSI codes into HTML <span class="ansi-…"> markup
//     so the rendered Claude Code colors (file paths blue, errors red, etc.)
//     show up in the web UI.

import Anser from 'anser';

// Matches CSI (e.g. \x1b[1;31m), OSC (\x1b]…\x07) and a few other escape
// sequences tmux can leave in capture output.
const ANSI_RE = /\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07/g;

export function stripAnsi(s: string): string {
  return s.replace(ANSI_RE, '');
}

// Convert raw ANSI-encoded text into HTML using `anser`.
//
// We use INLINE styles (use_classes:false) instead of CSS classes because
// real tmux capture from Claude Code is dominated by truecolor (\x1b[38;2;…)
// and 256-color (\x1b[38;5;…) escapes from syntax highlighting — anser only
// emits CSS class names for the 16 named colors. With inline styles every
// color format just works, and the colors stay faithful to whatever the
// source program emitted instead of being remapped to a theme palette.
//
// `anser` HTML-escapes &, <, > internally, so it's safe to set the result via
// dangerouslySetInnerHTML even when the source contains markup-like text.
export function ansiToHtml(text: string): string {
  return Anser.ansiToHtml(text, {
    use_classes: false,
    remove_empty: true
  });
}
