export type TerminalRenderer = 'webgl' | 'dom';

// Full-screen provider TUIs redraw large grids for every keystroke. xterm's
// DOM renderer turns those frames into thousands of styled spans and layout
// work, which is not an acceptable fallback for an interactive terminal.
// WebGL is therefore the default on every native client, including WKWebView;
// xterm's built-in DOM renderer remains the no-GPU fallback.
export function terminalRenderer(_nativeClient: boolean, _userAgent: string): TerminalRenderer {
  return 'webgl';
}

// A retained GPU atlas can very occasionally show glyphs from the preceding
// full-screen view after a provider clears or swaps its alternate screen. Do
// not repaint every PTY frame to compensate: that was the source of macOS
// typing lag. Repair only the explicit reset/erase/buffer-switch sequences.
export function terminalNeedsGpuRepair(data: string): boolean {
  return (
    data.includes('\x1bc')
    || /\x1b\[(?:2|3)J/.test(data)
    || /\x1b\[\?(?:47|1047|1049)[hl]/.test(data)
  );
}
