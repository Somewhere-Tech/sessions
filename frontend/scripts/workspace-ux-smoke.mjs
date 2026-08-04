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
assert.match(launcher, /'Start a new session'/);
assert.match(launcher, /launcher-setup-grid/);
assert.match(launcher, /launcher-setup-control is-workspace/);
assert.match(launcher, /aria-label="Agent"/);
assert.match(launcher, /aria-label="Computer"/);
assert.doesNotMatch(launcher, /already has \{liveOnSelectedMachine\} live sessions/);
assert.ok(launcher.indexOf('aria-label="Permissions"') < launcher.indexOf('launcher-advanced'),
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

  await page.setViewport({ width: 1100, height: 760 });
  await page.setContent(`
    <style>${styles}</style>
    <main id="launcher-surface" class="new-session-surface" style="width:1080px;height:740px">
      <form id="launcher" class="dialog dialog-wide new-session-launcher is-embedded">
        <header class="dialog-head launcher-compact-head"><div class="launcher-breadcrumb"><span class="workspace-folder-icon"></span><span>Sessions</span><span>/</span><strong>New session</strong></div></header>
        <div id="launcher-body" class="dialog-body">
          <section id="launcher-hero" class="launcher-hero"><span>Ready when you are</span><h2>Start a new session</h2><p>Choose where it runs, then tell the agent what you want to work on.</p></section>
          <section id="launcher-setup" class="launcher-setup-grid"><label class="launcher-setup-control"><small>Agent</small><span><select><option>Claude</option></select></span></label><label class="launcher-setup-control"><small>Computer</small><span><select><option>Mac mini (this machine)</option></select></span></label><button class="launcher-setup-control is-workspace"><small>Folder</small><span><span class="workspace-folder-icon"></span><strong>Sessions</strong><span class="launcher-setup-change">Change</span></span></button></section>
          <div id="launcher-composer" class="field launcher-task-field launcher-composer input-composer">
            <textarea class="input-textarea" rows="6" placeholder="Ask an agent to work"></textarea>
            <div class="launcher-composer-footer input-composer-footer"><div class="launcher-composer-context">
              <span class="model-picker is-compact"><button id="launcher-model" class="model-picker-trigger">Default</button></span>
              <label class="launcher-effort-chip"><select id="launcher-effort"><option>Default effort</option></select></label>
            </div><div class="launcher-composer-actions"><button class="btn btn-primary launcher-composer-start">↑</button></div></div>
          </div>
          <section id="launcher-workspace" class="launcher-workspace-shell"><div class="launcher-workspace-picker"><div class="launcher-directory-picker"><input value="/Users/example/Sessions"></div></div></section>
        </div>
      </form>
    </main>
  `);
  const launcherBounds = await page.evaluate(() => {
    const surface = document.querySelector('#launcher-surface').getBoundingClientRect();
    const launcher = document.querySelector('#launcher').getBoundingClientRect();
    const body = document.querySelector('#launcher-body').getBoundingClientRect();
    const hero = document.querySelector('#launcher-hero').getBoundingClientRect();
    const setup = document.querySelector('#launcher-setup').getBoundingClientRect();
    const composer = document.querySelector('#launcher-composer').getBoundingClientRect();
    const workspace = document.querySelector('#launcher-workspace').getBoundingClientRect();
    return {
      surfaceWidth: surface.width,
      launcherWidth: launcher.width,
      launcherHeight: launcher.height,
      borderRadius: getComputedStyle(document.querySelector('#launcher')).borderRadius,
      bodyWidth: body.width,
      heroBottom: hero.bottom,
      setupTop: setup.top,
      setupBottom: setup.bottom,
      composerTop: composer.top,
      composerHeight: composer.height,
      workspaceTop: workspace.top,
      workspaceRight: workspace.right,
      bodyRight: body.right,
      heroFontSize: Number.parseFloat(getComputedStyle(document.querySelector('#launcher-hero h2')).fontSize),
      modelFontSize: getComputedStyle(document.querySelector('#launcher-model')).fontSize,
      effortFontSize: getComputedStyle(document.querySelector('#launcher-effort')).fontSize
    };
  });
  assert.equal(launcherBounds.launcherWidth, launcherBounds.surfaceWidth, 'launcher must fill the conversation pane');
  assert.ok(launcherBounds.launcherHeight >= 740, 'launcher must fill the conversation pane height');
  assert.equal(launcherBounds.borderRadius, '0px', 'embedded launcher must not look like a modal card');
  assert.ok(launcherBounds.bodyWidth <= 880, `launcher content should stay focused, got ${launcherBounds.bodyWidth}px`);
  assert.ok(launcherBounds.heroBottom <= launcherBounds.setupTop, 'the setup controls must follow the invitation');
  assert.ok(launcherBounds.setupBottom <= launcherBounds.composerTop, 'the prompt must follow the setup controls');
  assert.ok(launcherBounds.heroFontSize <= 31, `launcher title should stay inviting rather than oversized, got ${launcherBounds.heroFontSize}px`);
  assert.equal(launcherBounds.modelFontSize, launcherBounds.effortFontSize, 'model and effort controls must use one font size');
  assert.ok(launcherBounds.composerHeight >= 135, `the prompt must remain the primary control, got ${launcherBounds.composerHeight}px`);
  assert.ok(launcherBounds.workspaceTop >= launcherBounds.composerTop + launcherBounds.composerHeight, 'the project picker must sit below the prompt');
  assert.ok(launcherBounds.workspaceRight <= launcherBounds.bodyRight, 'the project picker must not overflow the launcher');
} finally {
  await browser.close();
}

console.log('Sessions workspace UX smoke: ok');
