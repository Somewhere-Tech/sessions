#!/usr/bin/env node
// DOM probe — opens the sessions web app in headless Chrome, picks a
// session, and dumps the DOM text + computed styles for each view pane.
// This is the "stop pestering the user for screenshots" tool: every time
// I make a UI change I can run this and verify the panes actually have
// content, instead of asking the user to peek at their browser.
//
// Usage:
//   node scripts/dom-probe.mjs [--url=http://127.0.0.1:5273] [--session=<id>] [--mode=terminal|reflowed|sessions|split] [--all]
//   --all → run all four modes back-to-back and report each.
//
// Default: pick the first active session, run all four modes.

import puppeteer from 'puppeteer';

const args = Object.fromEntries(
  process.argv.slice(2)
    .filter(a => a.startsWith('--'))
    .map(a => {
      const eq = a.indexOf('=');
      if (eq === -1) return [a.slice(2), 'true'];
      return [a.slice(2, eq), a.slice(eq + 1)];
    })
);

const URL_BASE = args.url ?? 'http://127.0.0.1:5273';
const RUN_ALL = args.all === 'true' || (!args.mode && !args.session);
const REQUESTED_MODE = args.mode;

async function fetchSessionId() {
  if (args.session) return args.session;
  const res = await fetch(`${URL_BASE}/api/sessions`);
  const body = await res.json();
  if (!body.sessions?.length) throw new Error('no active sessions');
  return body.sessions[0].id;
}

const sessionId = await fetchSessionId();
console.log(`probing sessionId=${sessionId}`);

const browser = await puppeteer.launch({
  executablePath: '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  headless: 'new',
  args: ['--no-sandbox']
});

try {
  const page = await browser.newPage();
  // Match a typical iPhone viewport unless the caller overrides; the bug
  // the user is hitting is on phone, and the 100dvh / zoom-to-fit code
  // paths only kick in at narrow widths.
  const vpW = Number(args.width ?? 390);
  const vpH = Number(args.height ?? 844);
  await page.setViewport({ width: vpW, height: vpH });

  // Capture console + page errors so silent JS bugs surface here.
  const consoleErrors = [];
  page.on('pageerror', (e) => consoleErrors.push(`pageerror: ${e.message}\n  ${(e.stack || '').split('\n').slice(0, 6).join('\n  ')}`));
  page.on('console', (msg) => {
    if (msg.type() === 'error') consoleErrors.push(`console.error: ${msg.text()}`);
  });

  // Pre-seed localStorage so the SPA picks the active session before
  // first refresh. ACTIVE_KEY is stored as a raw string (not JSON).
  await page.evaluateOnNewDocument((sid) => {
    try {
      localStorage.setItem('sessions:active-session:v1', sid);
    } catch {}
  }, sessionId);

  await page.goto(URL_BASE, { waitUntil: 'domcontentloaded', timeout: 15000 });

  // Wait for the store to refresh — the SPA polls /api/sessions every 3s
  // and only sets activeId after that lands. Bump it manually by waiting
  // for the GET to resolve.
  await page.waitForResponse(
    (r) => r.url().endsWith('/api/sessions') && r.ok(),
    { timeout: 8000 }
  );

  // Wait for the SessionView to mount.
  await page.waitForSelector('.session-view', { timeout: 8000 });

  const modes = RUN_ALL ? ['terminal', 'reflowed', 'split', 'sessions'] : [REQUESTED_MODE ?? 'split'];

  for (const mode of modes) {
    // Click the matching toggle button.
    const clicked = await page.evaluate((m) => {
      const btns = [...document.querySelectorAll('.view-toggle-btn')];
      const btn = btns.find(b => b.textContent?.trim().toLowerCase() === m);
      if (!btn) return false;
      btn.click();
      return true;
    }, mode);
    if (!clicked) {
      console.log(`\n=== mode=${mode} (button not found, skipping) ===`);
      continue;
    }

    // Let the throttle (200ms) + a paint settle.
    await new Promise((r) => setTimeout(r, 800));

    const report = await page.evaluate(() => {
      const probe = (sel) => {
        const el = document.querySelector(sel);
        if (!el) return { exists: false };
        const cs = window.getComputedStyle(el);
        const rect = el.getBoundingClientRect();
        const text = (el.textContent ?? '').replace(/\s+/g, ' ').trim();
        return {
          exists: true,
          display: cs.display,
          visibility: cs.visibility,
          opacity: cs.opacity,
          width: Math.round(rect.width),
          height: Math.round(rect.height),
          textLen: text.length,
          textPreview: text.slice(0, 200)
        };
      };
      const xtermRows = (() => {
        const rows = document.querySelectorAll('.xterm-rows > div');
        if (!rows.length) return null;
        const all = [...rows].map(r => r.textContent ?? '');
        // Where is xterm actually positioned + sized? Big clue if it's
        // zoomed to ~0 or transformed off-screen.
        const xterm = document.querySelector('.xterm');
        const screen = document.querySelector('.xterm-screen');
        const probe = (el) => {
          if (!el) return null;
          const cs = window.getComputedStyle(el);
          const rect = el.getBoundingClientRect();
          return {
            w: Math.round(rect.width), h: Math.round(rect.height),
            x: Math.round(rect.x), y: Math.round(rect.y),
            opacity: cs.opacity, transform: cs.transform, zoom: cs.zoom,
            color: cs.color
          };
        };
        // Pull the LAST non-empty rows so we see the live viewport,
        // not just the top of the scrollback.
        const lastFew = all.slice(-6).map(s => s.trim()).filter(Boolean);
        return {
          rowCount: rows.length,
          nonEmptyRows: all.filter(s => s.trim().length > 0).length,
          firstRow: all.find(s => s.trim().length > 0) ?? '',
          lastRows: lastFew,
          totalChars: all.join('').replace(/\s+/g, '').length,
          xterm: probe(xterm),
          screen: probe(screen)
        };
      })();
      return {
        terminalPane: probe('.session-terminal-pane'),
        reflowedPane: probe('.session-reflowed-pane'),
        prettyPane: probe('.session-sessions-pane'),
        reflowedView: probe('.reflowed-view'),
        prettyView: probe('.sessions-view'),
        prettyEmpty: probe('.sessions-empty'),
        xterm: xtermRows
      };
    });

    console.log(`\n=== mode=${mode} ===`);
    for (const [k, v] of Object.entries(report)) {
      if (v && typeof v === 'object' && 'exists' in v) {
        if (!v.exists) { console.log(`  ${k.padEnd(14)} (not in DOM)`); continue; }
        console.log(`  ${k.padEnd(14)} display=${v.display} ${v.width}x${v.height} textLen=${v.textLen}${v.textLen ? ` "${v.textPreview.slice(0, 100)}…"` : ''}`);
      } else if (v && typeof v === 'object') {
        console.log(`  ${k.padEnd(14)}`);
        for (const [k2, v2] of Object.entries(v)) {
          let line = '';
          if (Array.isArray(v2)) line = v2.map(x => typeof x === 'string' ? x.slice(0, 80) : JSON.stringify(x)).join(' | ');
          else if (typeof v2 === 'object' && v2 !== null) line = JSON.stringify(v2);
          else line = String(v2);
          console.log(`    ${k2.padEnd(12)} ${line.slice(0, 200)}`);
        }
      } else {
        console.log(`  ${k.padEnd(14)} ${v}`);
      }
    }
  }

  if (consoleErrors.length) {
    console.log(`\n=== console errors (${consoleErrors.length}) ===`);
    consoleErrors.slice(0, 20).forEach((e) => console.log(`  ${e}`));
  }
} finally {
  await browser.close();
}
