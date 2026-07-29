// Browser clipboard with a fallback path for plain-HTTP origins.
// `navigator.clipboard` is gated to secure contexts (HTTPS or localhost),
// so when sessions is served from a Tailscale IP over HTTP we fall
// back to a hidden textarea + execCommand('copy'). Returns true on
// success — caller is expected to flash UI feedback on the button.

export async function copyText(text: string): Promise<boolean> {
  // CRITICAL: in plain-HTTP origins (e.g. http://<tailnet-ip>:5273)
  // navigator.clipboard.writeText() is rejected as "NotAllowed" by
  // Chrome/Safari because they gate it to secure contexts. If we await
  // that rejection FIRST and fall through to execCommand, the
  // user-gesture frame is already over (we're in a microtask) and
  // execCommand also refuses. The whole copy silently fails — that's
  // the "click does nothing" the user reported.
  //
  // So in insecure contexts, run execCommand SYNCHRONOUSLY first
  // (still inside the click handler's gesture frame). Only when we're
  // in a secure context do we use the modern async API.
  const isSecure = typeof window !== 'undefined' && window.isSecureContext;

  if (isSecure && typeof navigator !== 'undefined' && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // fall through — secure context but the API still rejected
      // (browser permission, transient state). Try execCommand as a
      // last resort even though the gesture frame is fading.
    }
  }

  // execCommand fallback — works on insecure origins and older iOS
  // Safari. Synchronous: must run inside a user gesture, which it does
  // when called directly from the click handler's first turn.
  try {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.top = '0';
    ta.style.left = '0';
    ta.style.opacity = '0';
    ta.style.pointerEvents = 'none';
    document.body.appendChild(ta);
    // iOS requires explicit selection range — plain .select() is unreliable.
    ta.focus();
    ta.select();
    ta.setSelectionRange(0, text.length);
    const ok = document.execCommand('copy');
    document.body.removeChild(ta);
    return ok;
  } catch {
    return false;
  }
}

// "Copy on click anywhere in the box" handler. Used by chat bubbles in
// Remote and Grid view. Skips clicks on interactive children (links,
// buttons, inputs, code blocks, anything tagged data-no-copy) and bails
// when there's an active text selection so drag-to-copy still works.
//
// On success, pops a transient "Copied" badge at the click coordinates
// — follows the mouse, fades after ~900ms. Works on every bubble that
// wires this in without per-component CSS.
const ANSI_STRIP_RE = /\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07/g;

export function copyOnClickAtPointer(
  e: React.MouseEvent<HTMLElement>,
  text: string
): void {
  const target = e.target as HTMLElement;
  if (target.closest('a, button, input, pre, [data-no-copy]')) return;
  const sel = window.getSelection();
  if (sel && sel.toString().length > 0) return;
  const x = e.clientX;
  const y = e.clientY;
  const plain = text.replace(ANSI_STRIP_RE, '');
  void copyText(plain).then((ok) => {
    if (!ok) return;
    showCopiedBadge(x, y);
  });
}

// Spawn a "Copied" badge at the given viewport coordinates. Animated
// via a single CSS class; removed from the DOM after the animation
// finishes so we never accumulate orphaned elements.
function showCopiedBadge(x: number, y: number): void {
  const el = document.createElement('div');
  el.className = 'copy-flash';
  el.textContent = 'Copied';
  // Position-fixed so coords are viewport-relative. translate(-50%, …)
  // centers horizontally on the click point and lifts the label above
  // the pointer so it doesn't sit under the user's finger on touch.
  el.style.left = `${x}px`;
  el.style.top = `${y}px`;
  document.body.appendChild(el);
  // Auto-remove after the CSS animation completes.
  window.setTimeout(() => {
    try { el.remove(); } catch { /* ignore */ }
  }, 1000);
}
