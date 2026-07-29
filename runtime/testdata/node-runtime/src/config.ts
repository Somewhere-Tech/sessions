import os from 'node:os';
import fs from 'node:fs';
import nodePath from 'node:path';
import crypto from 'node:crypto';

export const config = {
  host: process.env.PRETTYD_HOST ?? '127.0.0.1',
  port: Number(process.env.PRETTYD_PORT ?? 8787),
  defaultShell: process.env.SHELL ?? '/bin/bash',
  defaultCwd: process.env.HOME ?? os.homedir(),
  // Fixed PTY size, never client-resized. The PTY is the canonical
  // wide buffer; every client fetches snapshots reflowed (server-side,
  // ANSI-aware) to its own viewport width. 300 cols is wide enough
  // that Claude Code / Codex draw their TUIs without compromise (long
  // file names, wide tables, ascii diagrams) and the reflow engine
  // wraps prose down to whatever the client actually has on screen.
  // 50 rows gives a generous live viewport without bloating snapshots.
  defaultCols: 300,
  defaultRows: 50
};

export const PRETTYD_STATE_DIR = nodePath.join(os.homedir(), '.local', 'state', 'pretty-PTY');

// ~/.local/state/pretty-PTY/token — 64 hex chars (32 random bytes).
const TOKEN_PATH = nodePath.join(PRETTYD_STATE_DIR, 'token');

// Operator escape hatch: `touch ~/.local/state/pretty-PTY/open` disables token
// auth (trusted-network / Tailscale-only mode — how it ran before auth
// shipped). Honored by BOTH the HTTP gate and the WS upgrade. The Origin
// allowlist still applies, so cross-site (CSWSH) protection is unaffected.
// Reversible: delete the file to re-enable tokens.
export function isAuthOpen(): boolean {
  return fs.existsSync(nodePath.join(PRETTYD_STATE_DIR, 'open'));
}

/**
 * Returns the daemon's auth token, creating it on first call.
 *
 * The token is 32 cryptographically random bytes encoded as 64 lowercase
 * hex characters, stored at ~/.local/state/pretty-PTY/token with mode
 * 0600. The directory is created (mode 0700) if missing. Every HTTP
 * route except /api/health and /api/health/deep, and every WS upgrade,
 * requires this token — either as an `Authorization: Bearer <t>` header
 * or a `?token=<t>` query parameter.
 */
export function getAuthToken(): string {
  try {
    const existing = fs.readFileSync(TOKEN_PATH, 'utf8').trim();
    // Validate format so a truncated/corrupted file doesn't become the token.
    if (/^[0-9a-f]{64}$/.test(existing)) return existing;
  } catch { /* missing or unreadable — fall through to create */ }
  // mkdir -p so the first run on a fresh machine doesn't fail.
  const dir = nodePath.dirname(TOKEN_PATH);
  fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
  const token = crypto.randomBytes(32).toString('hex');
  fs.writeFileSync(TOKEN_PATH, token, { mode: 0o600 });
  return token;
}

/**
 * True when the request origin is allowed to reach this daemon.
 *
 * Non-browser clients (CLI, curl) send no Origin header and are always
 * allowed. Browser requests are allowed when the origin is loopback
 * (127.0.0.1, localhost, ::1) OR the origin hostname matches the
 * configured bind host — enabling the web UI when prettyd is bound to a
 * Tailscale address without opening it to arbitrary cross-origin sites.
 */
// The hosted onboarding/setup page on somewhere. Matched as an EXACT serialized
// origin (scheme+host+port) — never a hostname — so plain http://, another port,
// or a look-alike subdomain (pretty-pty.somewhere.tech.evil.test) is rejected.
// Lets the hosted walkthrough's "is my daemon reachable?" check call this daemon.
const HOSTED_SHELL_ORIGINS = new Set([
  'https://pretty-pty.somewhere.tech',
  // 2026-07-16: the somewhere platform migrated hosted domains — .tech now
  // 308-redirects to .site, so the page's actual origin is .site. Allow both.
  'https://pretty-pty.somewhere.site'
]);

export function isAllowedOrigin(origin: string | undefined, host: string): boolean {
  // No origin = non-browser client (curl, CLI) — always allow.
  if (!origin) return true;

  let parsed: URL;
  try {
    parsed = new URL(origin);
  } catch {
    return false; // malformed origin — reject
  }

  // Fixed hosted origin — exact serialized-origin match, not a hostname rule.
  if (HOSTED_SHELL_ORIGINS.has(parsed.origin)) return true;

  const oh = parsed.hostname;
  // Loopback: standard browser localhost variants.
  if (oh === '127.0.0.1' || oh === 'localhost' || oh === '::1') return true;
  // Configured bind host (e.g. a Tailscale IP like 100.x.x.x).
  if (oh === host) return true;

  return false;
}
