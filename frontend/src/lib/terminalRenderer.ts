export type TerminalRenderer = 'webgl' | 'canvas' | 'dom';

// Tauri uses WKWebView on Apple platforms. xterm's WebGL renderer is fast
// there, but full-screen TUIs that erase and repaint rows can leave stale
// glyph textures composited over the new frame. Canvas avoids the WebGL
// texture path but still paints through a retained canvas layer, which can
// exhibit the same stale-row composition during Claude's settings/login
// screens. Use xterm's correctness-first DOM renderer in native Apple
// WebViews. Chromium/WebView2 clients keep the faster WebGL path.
export function terminalRenderer(nativeClient: boolean, userAgent: string): TerminalRenderer {
  const appleWebView = nativeClient
    && /(?:Macintosh|Mac OS X|iPhone|iPad|iPod)/i.test(userAgent)
    && /AppleWebKit/i.test(userAgent)
    && !/(?:Chrome|Chromium|Edg)\//i.test(userAgent);
  return appleWebView ? 'dom' : 'webgl';
}
