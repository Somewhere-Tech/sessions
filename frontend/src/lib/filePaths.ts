// Detects file paths in already-rendered HTML and wraps them in
// vscode://file/... anchor links so the user can click to open in VS Code.
//
// Runs as the LAST stage of the content rendering pipeline (after ANSI
// conversion and after markdown), so the input is HTML that may contain
// existing <a>, <span>, <code>, etc. We walk it as a token stream and only
// linkify text outside of <a>...</a> blocks.

const PATH_RE =
  // (boundary)(optional leading /)(at least one dir/)(filename.ext)(optional :line)
  /(^|[\s(\[<>"'])((?:\/|\.{1,2}\/)?(?:[\w@.-]+\/)+[\w@.-]+\.[A-Za-z][\w]{0,9})(?::(\d+))?/g;

// Patterns we should NOT treat as file paths even if they slip through.
const VERSION_RE = /^v?\d+\.\d+(?:\.\d+)?$/;

function escapeAttr(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;');
}

function linkifyText(text: string, cwd: string): string {
  return text.replace(PATH_RE, (_full, boundary: string, path: string, lineStr: string | undefined) => {
    // Strip leading "./" / "../" prefix for the version check
    const bare = path.replace(/^\.{1,2}\//, '').replace(/^\//, '');
    if (VERSION_RE.test(bare)) return _full;

    // Build the absolute filesystem path
    const absPath = path.startsWith('/') ? path : `${cwd}/${path.replace(/^\.\//, '')}`;
    const uri = lineStr
      ? `vscode://file${absPath}:${lineStr}`
      : `vscode://file${absPath}`;

    const display = lineStr ? `${path}:${lineStr}` : path;
    return `${boundary}<a href="${escapeAttr(uri)}" class="file-link" title="Open in VS Code">${display}</a>`;
  });
}

export function linkifyFilePaths(html: string, cwd: string): string {
  if (!cwd) return html; // no cwd → can't construct absolute paths
  let result = '';
  let i = 0;
  let insideAnchor = 0;
  while (i < html.length) {
    if (html[i] === '<') {
      const end = html.indexOf('>', i);
      if (end < 0) {
        result += html.slice(i);
        break;
      }
      const tag = html.slice(i, end + 1);
      if (/^<a\b/i.test(tag)) insideAnchor++;
      else if (/^<\/a\s*>/i.test(tag)) insideAnchor--;
      result += tag;
      i = end + 1;
    } else {
      const next = html.indexOf('<', i);
      const stop = next < 0 ? html.length : next;
      const text = html.slice(i, stop);
      result += insideAnchor > 0 ? text : linkifyText(text, cwd);
      i = stop;
    }
  }
  return result;
}
