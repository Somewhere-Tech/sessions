import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import puppeteer from 'puppeteer';
import { closeBrowser } from './lib/smoke.mjs';
import { readStylesheetTree } from './lib/source-styles.mjs';
import { readSessionsdSource } from './lib/source-api.mjs';

const source = async (path) => readFile(new URL(`../${path}`, import.meta.url), 'utf8');
const [fleet, servers, api, styles, status] = await Promise.all([
  source('src/components/FleetView.tsx'),
  source('src/lib/servers.ts'),
  readSessionsdSource(),
  readStylesheetTree(new URL('../src/styles/globals.css', import.meta.url)),
  source('src/lib/sessionStatus.ts')
]);

// The state vocabulary lives in exactly one module. Fleet asks for it; it
// must not carry its own label map or its own precedence order, which is how
// it came to say "Needs you" about a session the navigator called "Ended".
assert.match(status, /'needs-you': 'Needs you'/);
assert.match(fleet, /classifySession/);
assert.doesNotMatch(fleet, /'Needs you'/);
assert.doesNotMatch(fleet, /function fleetSessionState/);
assert.match(fleet, /sortFleetSessions\(candidateSessions\)/);
assert.doesNotMatch(fleet, /quiet sessions/);
assert.doesNotMatch(fleet, /Open Sessions to see and manage everything\./);
assert.match(fleet, /className="fleet-session-context"/);
assert.doesNotMatch(fleet, /className="fleet-session-cwd"/);
assert.doesNotMatch(fleet, /className="fleet-session-summary"/);
assert.doesNotMatch(fleet, /className="fleet-session-tags"/);
assert.match(fleet, /reported\.includes\('windows'\)/);
assert.match(fleet, /Sessions \$\{version\}/);
assert.match(fleet, /Name this machine/);
assert.match(fleet, /updateServer\(server\.id, \{ name, customName: name \}\)/);
assert.match(fleet, /serverDisplayName\(server, true\)/);
assert.match(servers, /if \(custom\) return custom;/,
  'a custom Fleet label must override a renamed system hostname');
assert.match(servers, /return 'This Mac';/,
  'the local machine needs a short human label instead of its system hostname');
assert.match(api, /\/api\/machine/,
  'clients must refresh the authenticated stable machine identity');
assert.match(styles, /grid-template-columns:\s*repeat\(auto-fit,\s*minmax\(min\(420px,\s*100%\),\s*1fr\)\)/);
assert.match(styles, /\.fleet-session-state\s*\{[\s\S]*?white-space:\s*nowrap;/);
assert.doesNotMatch(styles, /\.fleet-session-state\s*\{[^}]*font-family:\s*var\(--font-mono\)/);

const browser = await puppeteer.launch({ headless: true });
try {
  const page = await browser.newPage();
  await page.setViewport({ width: 700, height: 800 });
  await page.setContent(`
    <style>${styles}</style>
    <main class="operations-shell" style="width:700px;height:800px">
      <section class="fleet-view">
        <div class="fleet-machine-grid">
          <section class="fleet-server-group is-local" id="local-card">
            <header class="fleet-machine-header">
              <span class="fleet-platform-mark is-macos"></span>
              <div class="fleet-server-identity">
                <div class="fleet-machine-title"><h2>This Mac</h2></div>
                <span class="fleet-machine-status is-reachable"><i class="fleet-reachability-dot is-reachable"></i>Reachable</span>
              </div>
              <span class="fleet-machine-count"><strong>11 live</strong><span>44 total</span></span>
            </header>
            <div class="fleet-machine-meta">
              <span>macOS</span><span>arm64</span><span class="is-version">Sessions 0.2.6</span><span>2 accounts</span>
            </div>
            <div class="fleet-version-notice is-newer">
              <strong>Newer version</strong><span>0.2.6 here · 0.2.5 locally</span>
            </div>
            <div class="fleet-session-list">
              <button class="fleet-session-row">
                <span class="fleet-session-icon">●</span>
                <span class="fleet-session-main">
                  <span class="fleet-session-name">sw-data-discussion with an intentionally long title</span>
                  <span class="fleet-session-context">somewhere-tech</span>
                </span>
                <span class="fleet-session-state is-needs-you"><i class="fleet-session-state-dot"></i>Needs you</span>
              </button>
              <button class="fleet-session-row">
                <span class="fleet-session-icon">●</span>
                <span class="fleet-session-main">
                  <span class="fleet-session-name">landing page</span>
                  <span class="fleet-session-context">landing-page</span>
                </span>
                <span class="fleet-session-state is-finished"><i class="fleet-session-state-dot"></i>Finished</span>
              </button>
            </div>
          </section>
          <section class="fleet-server-group" id="remote-card">
            <header class="fleet-machine-header">
              <span class="fleet-platform-mark is-windows" aria-label="Windows"></span>
              <div class="fleet-server-identity">
                <div class="fleet-machine-title"><h2>This PC</h2></div>
                <span class="fleet-machine-status is-reachable"><i class="fleet-reachability-dot is-reachable"></i>Reachable</span>
              </div>
              <span class="fleet-machine-count"><strong>2 live</strong><span>5 total</span></span>
            </header>
            <div class="fleet-machine-meta">
              <span>Windows</span><span>amd64</span><span class="is-version">Sessions 0.2.7</span>
            </div>
            <div class="fleet-version-notice is-newer">
              <strong>Newer version</strong><span>0.2.7 here · 0.2.6 locally</span>
            </div>
          </section>
        </div>
      </section>
    </main>
  `);

  const layout = await page.evaluate(() => {
    const card = document.querySelector('#local-card');
    const remote = document.querySelector('#remote-card');
    const status = document.querySelector('.fleet-session-state.is-needs-you');
    const context = document.querySelector('.fleet-session-context');
    return {
      cardWidth: card.getBoundingClientRect().width,
      cardOverflow: card.scrollWidth - card.clientWidth,
      remoteTop: remote.getBoundingClientRect().top,
      cardBottom: card.getBoundingClientRect().bottom,
      statusHeight: status.getBoundingClientRect().height,
      statusWhiteSpace: getComputedStyle(status).whiteSpace,
      statusFont: getComputedStyle(status).fontFamily,
      contextOverflow: context.scrollWidth > context.clientWidth,
      windowsCardText: remote.textContent.replace(/\s+/g, ' ').trim(),
      windowsCardOverflow: remote.scrollWidth - remote.clientWidth,
      windowsPlatformMark: remote.querySelector('.is-windows')?.getAttribute('aria-label')
    };
  });

  assert.ok(layout.cardWidth > 600, `narrow Fleet should use one wide card, got ${layout.cardWidth}px`);
  assert.ok(layout.remoteTop >= layout.cardBottom, 'machine cards should stack rather than squeeze side by side');
  assert.ok(layout.cardOverflow <= 0, `machine card overflowed by ${layout.cardOverflow}px`);
  assert.ok(layout.statusHeight < 20, `Needs you wrapped to multiple lines (${layout.statusHeight}px)`);
  assert.equal(layout.statusWhiteSpace, 'nowrap');
  assert.doesNotMatch(layout.statusFont.toLowerCase(), /mono/);
  assert.equal(layout.contextOverflow, false, 'compact workspace context should fit without overflowing');
  assert.match(layout.windowsCardText, /This PC/);
  assert.match(layout.windowsCardText, /Windows/);
  assert.match(layout.windowsCardText, /Sessions 0\.2\.7/);
  assert.equal(layout.windowsPlatformMark, 'Windows');
  assert.ok(layout.windowsCardOverflow <= 0, `Windows machine card overflowed by ${layout.windowsCardOverflow}px`);
} finally {
  // Bounded: `browser.close()` waits on the browser's own shutdown, and a
  // wedged Chromium on a loaded machine turns a finished suite into a hang with
  // no assertion to report. Fail, never wait.
  await closeBrowser(browser);
}

console.log('fleet clarity smoke: ok');
