// Translate a browser KeyboardEvent into the byte sequence a terminal
// app (xterm-256color) expects. Used by the Reflowed view + Grid cells
// so they can capture keystrokes directly and forward them to the PTY,
// matching the experience of typing into a real terminal — no
// intermediate "input box" widget for those views.
//
// Returns null when the event has no PTY equivalent (e.g. function
// keys we don't translate, modifier-only presses, OS-level shortcuts);
// the caller should NOT preventDefault in that case so the browser
// keeps its native behavior (cmd-C copy, cmd-W close window, etc.).

import type { KeyboardEvent as ReactKeyboardEvent } from 'react';

type AnyKeyEvent = KeyboardEvent | ReactKeyboardEvent;

export function keyToBytes(e: AnyKeyEvent): string | null {
  const { key } = e;
  if (!key) return null;

  // Modifier-only events: ignore. The next non-modifier press will pick
  // up the modifier state via e.ctrlKey / etc.
  if (key === 'Shift' || key === 'Control' || key === 'Alt' || key === 'Meta') {
    return null;
  }

  // Cmd + a few keys have terminal-level meaning that overrides the OS
  // shortcut. macOS Terminal.app does the same thing — Cmd-Backspace
  // means "kill to start of line" inside the shell, not a system
  // function. Map these BEFORE the general meta-bypass below.
  if (e.metaKey) {
    if (key === 'Backspace') return '\x15';      // Ctrl-U: kill to start-of-line
    if (key === 'ArrowLeft') return '\x01';      // Ctrl-A: cursor to BOL
    if (key === 'ArrowRight') return '\x05';     // Ctrl-E: cursor to EOL
    // Every other Cmd combo (cmd-C / cmd-V / cmd-W / cmd-T / cmd-Q)
    // hands off to the browser/OS — much more useful as native
    // shortcuts than as Ctrl-mapped PTY bytes.
    return null;
  }

  // Option-Backspace deletes the previous word (Ctrl-W in readline).
  // Option-arrows do word-by-word cursor motion. Same conventions as
  // macOS Terminal and most modern shells.
  if (e.altKey) {
    if (key === 'Backspace') return '\x17';      // Ctrl-W: kill word backward
    if (key === 'ArrowLeft') return '\x1bb';     // Esc-b: word backward
    if (key === 'ArrowRight') return '\x1bf';    // Esc-f: word forward
    if (key === 'Delete') return '\x1bd';        // Esc-d: kill word forward
  }

  // Special keys.
  switch (key) {
    case 'Enter': return '\r';
    case 'Backspace': return '\x7f';        // DEL — what most shells expect
    case 'Tab': return e.shiftKey ? '\x1b[Z' : '\t';
    case 'Escape': return '\x1b';
    case 'ArrowUp': return '\x1b[A';
    case 'ArrowDown': return '\x1b[B';
    case 'ArrowRight': return '\x1b[C';
    case 'ArrowLeft': return '\x1b[D';
    case 'Home': return '\x1b[H';
    case 'End': return '\x1b[F';
    case 'PageUp': return '\x1b[5~';
    case 'PageDown': return '\x1b[6~';
    case 'Delete': return '\x1b[3~';
    case 'Insert': return '\x1b[2~';
  }

  // Function keys F1-F12.
  if (/^F([1-9]|1[0-2])$/.test(key)) {
    const n = parseInt(key.slice(1), 10);
    const F: Record<number, string> = {
      1: '\x1bOP', 2: '\x1bOQ', 3: '\x1bOR', 4: '\x1bOS',
      5: '\x1b[15~', 6: '\x1b[17~', 7: '\x1b[18~', 8: '\x1b[19~',
      9: '\x1b[20~', 10: '\x1b[21~', 11: '\x1b[23~', 12: '\x1b[24~'
    };
    return F[n] ?? null;
  }

  // Ctrl + letter → control char (Ctrl-A = 0x01, Ctrl-Z = 0x1a).
  if (e.ctrlKey && key.length === 1) {
    const c = key.toLowerCase().charCodeAt(0);
    if (c >= 97 && c <= 122) return String.fromCharCode(c & 0x1f);
    // Common Ctrl-symbol mappings.
    if (key === ' ') return '\x00';                 // Ctrl-Space → NUL
    if (key === '[') return '\x1b';
    if (key === ']') return '\x1d';
    if (key === '\\') return '\x1c';
  }

  // Alt + letter → ESC + letter (xterm convention for "meta" key).
  if (e.altKey && key.length === 1) {
    return '\x1b' + key;
  }

  // Printable single character.
  if (key.length === 1) return key;

  return null;
}
