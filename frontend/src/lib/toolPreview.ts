// Canonical tool-input preview helper.
//
// Used by both claudeEvents.ts (chat chips) and useSessionSidebar.ts
// (currentTask label). A single source of truth avoids behavioral drift
// between the two render paths.
//
// Field-priority rationale:
//   • description-first for Bash — Claude's description is a human-readable
//     summary ("Run tests"); the raw shell command is often noisy. Both sites
//     benefit from the readable form.
//   • All Claude Code built-in tools get an explicit best-field guess;
//     unknown tools fall through to description → command → query → path,
//     then the full JSON serialisation (so *something* appears rather than
//     nothing for future/MCP tools).

export function previewToolInput(
  name: string,
  input: Record<string, unknown> | undefined
): string {
  if (!input) return '';
  const tryField = (k: string): string | null => {
    const v = input[k];
    return typeof v === 'string' && v.length > 0 ? v : null;
  };

  let preview: string | null = null;
  switch (name) {
    case 'Read':
    case 'Write':
    case 'Edit':
    case 'NotebookEdit':
      preview = tryField('file_path') ?? tryField('notebook_path');
      break;
    case 'Bash':
    case 'BashOutput':
    case 'KillBash':
      // description is more human-readable than the raw shell command
      preview = tryField('description') ?? tryField('command');
      break;
    case 'Glob':
    case 'Grep':
      preview = tryField('pattern') ?? tryField('path');
      break;
    case 'WebFetch':
    case 'WebSearch':
      preview = tryField('url') ?? tryField('query');
      break;
    case 'Task':
    case 'Agent':
      preview = tryField('description') ?? tryField('prompt');
      break;
    case 'TaskCreate':
    case 'TaskUpdate':
    case 'TaskStop':
      preview = tryField('title') ?? tryField('description');
      break;
    default:
      preview = tryField('description') ?? tryField('command') ?? tryField('query') ?? tryField('path');
  }

  if (!preview) {
    // Unknown / future tools: fall back to the full JSON so the user
    // sees *something* rather than a blank chip or empty task label.
    try { preview = JSON.stringify(input); } catch { preview = ''; }
  }

  // Normalise to a single line and truncate to 80 chars.
  preview = preview.replace(/\s+/g, ' ').trim();
  if (preview.length > 80) preview = preview.slice(0, 79) + '…';
  return preview;
}
