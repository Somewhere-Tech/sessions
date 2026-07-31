import type { MouseEvent } from 'react';
import { openExternalURL } from './tauriBridge';

const EXTERNAL_SCHEMES = new Set(['http:', 'https:', 'mailto:', 'vscode:']);

export function externalLinkTarget(href: string, base = window.location.href): string | null {
  const value = href.trim();
  if (!value || value.startsWith('#')) return null;
  try {
    const target = new URL(value, base);
    return EXTERNAL_SCHEMES.has(target.protocol) ? target.href : null;
  } catch {
    return null;
  }
}

// React-level delegation keeps every rendered transcript, recap, file link,
// and product link on one rule: external destinations leave Sessions. Tauri
// opens them with the operating system; browser builds use a new tab. Internal
// buttons and hash navigation remain inside the app.
export function handleExternalLinkClick(event: MouseEvent<HTMLElement>): void {
  if (event.defaultPrevented || event.button !== 0) return;
  const target = event.target instanceof Element ? event.target : null;
  const anchor = target?.closest<HTMLAnchorElement>('a[href]');
  if (!anchor || !event.currentTarget.contains(anchor)) return;
  const destination = externalLinkTarget(anchor.getAttribute('href') ?? '');
  if (!destination) return;
  event.preventDefault();
  event.stopPropagation();
  void openExternalURL(destination).catch((error) => {
    console.error('Sessions could not open the external link', error);
  });
}
