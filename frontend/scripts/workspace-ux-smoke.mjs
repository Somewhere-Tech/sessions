import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import puppeteer from 'puppeteer';
import { closeBrowser } from './lib/smoke.mjs';
import { readStylesheetTree } from './lib/source-styles.mjs';

const source = async (path) => readFile(new URL(`../${path}`, import.meta.url), 'utf8');
const [app, palette, picker, launcher, sessionView, preference, sidebar, navigator, fleetSessions, styles] = await Promise.all([
  source('src/App.tsx'),
  source('src/components/CommandPalette.tsx'),
  source('src/components/ModelPicker.tsx'),
  source('src/components/NewSessionDialog.tsx'),
  source('src/components/SessionView.tsx'),
  source('src/lib/sessionViewPreference.ts'),
  source('src/components/ProductSidebar.tsx'),
  source('src/components/SessionNavigator.tsx'),
  source('src/hooks/useFleetSessions.ts'),
  readStylesheetTree(new URL('../src/styles/globals.css', import.meta.url))
]);

assert.match(app, /event\.key\.toLowerCase\(\) === 'k'/);
assert.match(app, /event\.ctrlKey && !event\.metaKey && inTerminal/);
assert.match(app, /sessions\.length === 0 && !sessionsHydrated/);
assert.match(app, /<CommandPalette/);
assert.match(app, /<SessionsWorkspaceSkeleton/);
assert.match(palette, /role="dialog"/);
assert.match(palette, /aria-modal="true"/);
assert.match(palette, /resolvedSessionLabel\(session\)/);
assert.match(picker, /Search \$\{providerName\} models/);
assert.match(picker, /claude-fable-5/);
assert.match(picker, /Use exact model ID/);
assert.doesNotMatch(picker, /<button[\s\S]{0,1200}<button[\s\S]{0,300}★/);
assert.match(launcher, /launcher-composer-footer/);
assert.match(launcher, /<ModelPicker/);
assert.match(launcher, /What would you like to work on/);
assert.match(launcher, /aria-label="Session setup"/);
assert.match(launcher, /launcher-intent-control is-workspace/);
assert.match(launcher, /aria-label="Agent"/);
assert.match(launcher, /aria-label="Computer"/);
assert.doesNotMatch(launcher, /already has \{liveOnSelectedMachine\} live sessions/);
assert.ok(launcher.indexOf('aria-label="Access"') < launcher.indexOf('launcher-advanced'),
  'permissions belong in the primary composer before Advanced');
assert.match(app, /<NewSessionDialog\s+embedded/,
  'the global launcher must render inside the conversation workspace');
assert.doesNotMatch(launcher, /worktree|Developer isolation|Git copy/);
assert.match(sessionView, /has-terminal-drawer/);
assert.match(sessionView, /Exact provider view/);
assert.match(sessionView, /terminal-drawer-expanded/);
assert.match(sessionView, /term\.fitTerminalRef\.current\(\)/);
assert.match(preference, /Terminal is an escape hatch/);
assert.match(sidebar, /Find or run…/);
assert.match(app, /onClickCapture=\{handleExternalLinkClick\}/,
  'the native app shell must delegate external links to the operating system');
assert.match(navigator, /aria-label="Connected computers"/);
assert.match(navigator, />All machines</,
  'the navigator must expose one aggregate fleet scope');
assert.match(navigator, /selectMachineScope\(configured\.id\)/,
  'connected computers must remain visible as one-click session filters');
assert.match(navigator, /onOpenMachineSession\(snapshot\.server\.id, session\.id\)/,
  'an aggregate row must retain its machine scope when opened');
assert.match(fleetSessions, /listServerSessions\(server, controller\.signal\)/,
  'the aggregate inbox must query each already-configured machine directly');
assert.match(fleetSessions, /Slow\/offline machines poll[\s\S]*independently/,
  'one unreachable computer must not block the aggregate inbox');
assert.match(styles, /\.scroll-to-bottom-anchor\s*\{[^}]*justify-content:\s*center/s,
  'scroll-to-latest must stay centered away from the composer send controls');

const browser = await puppeteer.launch({ headless: true });
try {
  const page = await browser.newPage();
  // Launcher layout is exercised with the real React component in launcher-layout-smoke.

  await page.setViewport({ width: 420, height: 320 });
  await page.setContent(`
    <style>${styles}</style>
    <aside class="session-navigator" style="--session-nav-w:360px;width:360px;height:300px">
      <header id="nav-head" class="session-navigator-head"><div><span>Operations inbox</span><strong>Sessions</strong></div><div class="session-navigator-actions"><button>Resume</button><button>New</button></div></header>
      <div id="machine-filter" class="session-machine-filter"><button class="is-active"><span class="session-all-machines-mark"><i></i><i></i><i></i></span><span>All machines</span></button><button><span>Mac mini</span></button></div>
      <div id="nav-search" class="session-nav-search"><span>⌕</span><input placeholder="Filter sessions"></div>
      <div id="nav-filters" class="session-filter-row"><button class="is-active">All</button><button>Needs you</button><button>Working</button></div>
      <div id="nav-tree" class="session-tree"><div style="height:800px">many sessions</div></div>
    </aside>
  `);
  const navigatorBounds = await page.evaluate(() => {
    const head = document.querySelector('#nav-head').getBoundingClientRect();
    const machines = document.querySelector('#machine-filter').getBoundingClientRect();
    const search = document.querySelector('#nav-search').getBoundingClientRect();
    const filters = document.querySelector('#nav-filters').getBoundingClientRect();
    const tree = document.querySelector('#nav-tree').getBoundingClientRect();
    return {
      headBottom: head.bottom,
      machineTop: machines.top,
      machineBottom: machines.bottom,
      machineHeight: machines.height,
      searchTop: search.top,
      searchBottom: search.bottom,
      filtersTop: filters.top,
      filtersBottom: filters.bottom,
      treeTop: tree.top,
      treeHeight: tree.height
    };
  });
  assert.ok(navigatorBounds.machineHeight >= 30, `machine selector must not shrink out of view, got ${navigatorBounds.machineHeight}px`);
  assert.ok(navigatorBounds.machineTop >= navigatorBounds.headBottom, 'machine selector must sit below the inbox header');
  assert.ok(navigatorBounds.machineBottom <= navigatorBounds.searchTop, 'machine selector must not hide behind search');
  assert.ok(navigatorBounds.searchBottom <= navigatorBounds.filtersTop, 'search must not overlap status filters');
  assert.ok(navigatorBounds.filtersBottom <= navigatorBounds.treeTop, 'status filters must remain above the scrollable tree');
  assert.ok(navigatorBounds.treeHeight > 0, 'the session tree must absorb remaining height instead of shrinking controls');
} finally {
  // Bounded: `browser.close()` waits on the browser's own shutdown, and a
  // wedged Chromium on a loaded machine turns a finished suite into a hang with
  // no assertion to report. Fail, never wait.
  await closeBrowser(browser);
}

console.log('Sessions workspace UX smoke: ok');
