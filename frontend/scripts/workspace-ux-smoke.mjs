import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import puppeteer from 'puppeteer';

const source = async (path) => readFile(new URL(`../${path}`, import.meta.url), 'utf8');
const [app, palette, picker, launcher, sessionView, preference, sidebar, navigator, styles] = await Promise.all([
  source('src/App.tsx'),
  source('src/components/CommandPalette.tsx'),
  source('src/components/ModelPicker.tsx'),
  source('src/components/NewSessionDialog.tsx'),
  source('src/components/SessionView.tsx'),
  source('src/lib/sessionViewPreference.ts'),
  source('src/components/ProductSidebar.tsx'),
  source('src/components/SessionNavigator.tsx'),
  source('src/styles/globals.css')
]);

assert.match(app, /event\.key\.toLowerCase\(\) === 'k'/);
assert.match(app, /event\.ctrlKey && !event\.metaKey && inTerminal/);
assert.match(app, /sessions\.length === 0 && !sessionsHydrated/);
assert.match(app, /<CommandPalette/);
assert.match(app, /<SessionsWorkspaceSkeleton/);
assert.match(palette, /role="dialog"/);
assert.match(palette, /aria-modal="true"/);
assert.match(palette, /sessionLabel\(session\)/);
assert.match(picker, /Search \$\{providerName\} models/);
assert.match(picker, /claude-fable-5/);
assert.match(picker, /Use exact model ID/);
assert.doesNotMatch(picker, /<button[\s\S]{0,1200}<button[\s\S]{0,300}★/);
assert.match(launcher, /launcher-composer-footer/);
assert.match(launcher, /<ModelPicker/);
assert.match(sessionView, /has-terminal-drawer/);
assert.match(sessionView, /Exact provider view/);
assert.match(sessionView, /terminal-drawer-expanded/);
assert.match(sessionView, /term\.fitTerminalRef\.current\(\)/);
assert.match(preference, /Terminal is an escape hatch/);
assert.match(sidebar, /Find or run…/);
assert.match(app, /onClickCapture=\{handleExternalLinkClick\}/,
  'the native app shell must delegate external links to the operating system');
assert.match(navigator, /aria-label="Connected computers"/);
assert.match(navigator, /selectMachine\(configured\.id\)/,
  'connected computers must be visible as one-click session filters');
assert.match(styles, /\.scroll-to-bottom-anchor\s*\{[^}]*justify-content:\s*center/s,
  'scroll-to-latest must stay centered away from the composer send controls');

const browser = await puppeteer.launch({ headless: true });
try {
  const page = await browser.newPage();
  await page.setViewport({ width: 1100, height: 760 });
  await page.setContent(`
    <style>${styles}</style>
    <main class="session-view view-terminal has-terminal-drawer" style="width:1000px;height:680px">
      <header class="session-active-header"><div class="session-active-copy"><div class="session-active-title-row"><h1>Drawer smoke</h1></div></div></header>
      <div class="session-toolbar"></div>
      <div class="session-body">
        <div id="drawer" class="session-terminal-pane"><header class="terminal-drawer-header"><span><strong>Terminal</strong>Exact provider view</span></header><div class="terminal-host"></div></div>
        <div id="conversation" class="session-remote-pane"></div>
      </div>
    </main>
  `);
  const bounds = await page.evaluate(() => {
    const drawer = document.querySelector('#drawer').getBoundingClientRect();
    const conversation = document.querySelector('#conversation').getBoundingClientRect();
    return {
      drawerHeight: drawer.height,
      drawerBottom: drawer.bottom,
      conversationHeight: conversation.height,
      viewportHeight: window.innerHeight
    };
  });
  assert.ok(bounds.drawerHeight > 160 && bounds.drawerHeight < 440, `terminal drawer should stay bounded, got ${bounds.drawerHeight}`);
  assert.ok(bounds.drawerBottom <= bounds.viewportHeight, 'terminal drawer must stay on screen');
  assert.ok(bounds.conversationHeight > bounds.drawerHeight, 'conversation must remain the primary surface behind the drawer');
} finally {
  await browser.close();
}

console.log('Sessions workspace UX smoke: ok');
