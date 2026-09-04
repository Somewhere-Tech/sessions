import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { build } from 'esbuild';
import puppeteer from 'puppeteer';
import { closeBrowser } from './lib/smoke.mjs';
import { readStylesheetTree } from './lib/source-styles.mjs';

const source = async (path) => readFile(new URL(`../${path}`, import.meta.url), 'utf8');
const scratch = await mkdtemp(join(tmpdir(), 'sessions-android-mobile-'));

try {
  const bridgeOutput = join(scratch, 'tauri-bridge.mjs');
  await build({
    entryPoints: ['src/lib/tauriBridge.ts'],
    bundle: true,
    format: 'esm',
    outfile: bridgeOutput,
    platform: 'browser',
    target: 'es2022',
    logLevel: 'silent'
  });

  globalThis.window = { __TAURI_INTERNALS__: {} };
  Object.defineProperty(globalThis, 'navigator', {
    value: { userAgent: 'Mozilla/5.0 (Linux; Android 15) Mobile' },
    configurable: true
  });
  const { pairingCodeContent, registerAndroidBackHandler } = await import(
    `${pathToFileURL(bridgeOutput).href}?v=${Date.now()}`
  );
  const calls = [];
  const removeRoute = registerAndroidBackHandler(() => { calls.push('route'); return true; });
  assert.equal(window.__SESSIONS_ANDROID_BACK__(), true);
  assert.deepEqual(calls, ['route']);
  removeRoute();
  assert.equal(window.__SESSIONS_ANDROID_BACK__, undefined);
  assert.equal(
    pairingCodeContent({ content: 'sessions://pair?host=http%3A%2F%2F192.168.1.2%3A8787&t=ticket' }),
    'sessions://pair?host=http%3A%2F%2F192.168.1.2%3A8787&t=ticket'
  );
  assert.throws(() => pairingCodeContent({ content: 'https://example.com/not-pairing' }), /not a Sessions pairing link/);

  const [activity, manifest, app, appBack, mobileNav, connect, machineMark, newSession, terminalHook, terminalStyles, styles] = await Promise.all([
    source('../src-tauri/gen/android/app/src/main/java/tech/somewhere/sessions/MainActivity.kt'),
    source('../src-tauri/gen/android/app/src/main/AndroidManifest.xml'),
    source('src/App.tsx'),
    source('src/hooks/useAndroidBackNavigation.ts'),
    source('src/components/MobileNav.tsx'),
    source('src/components/ConnectScreen.tsx'),
    source('src/components/MachineMark.tsx'),
    source('src/components/NewSessionDialog.tsx'),
    source('src/hooks/useTerminal.ts'),
    source('src/styles/sections/42-terminal-interactions.css'),
    readStylesheetTree(new URL('../src/styles/globals.css', import.meta.url))
  ]);
  assert.match(activity, /OnBackPressedCallback/);
  assert.match(activity, /WindowInsetsCompat\.Type\.ime\(\)/);
  assert.match(activity, /window\.__SESSIONS_ANDROID_BACK__/);
  assert.match(activity, /Press back again to exit/);
  assert.match(activity, /EXIT_CONFIRMATION_MS = 2_000L/);
  assert.match(manifest, /android:windowSoftInputMode="adjustResize"/);
  assert.match(app, /useAndroidBackNavigation/);
  assert.match(app, /mobileSessionDetail[^\n]*setMobileSessionDetail\(false\)/);
  assert.match(app, /effectiveLayout !== 'home'[^\n]*setLayoutMode\('home'\)/);
  assert.match(appBack, /registerAndroidBackHandler/);
  assert.match(appBack, /mobile-more-heading button/);
  assert.match(mobileNav, /mobile-more-heading/);
  assert.match(connect, /visualViewport/);
  assert.match(connect, /scrollIntoView\(\{ block: 'center'/);
  assert.match(connect, /Scan a pairing code/);
  assert.match(connect, /scanPairingCode/);
  assert.doesNotMatch(machineMark, /\uF8FF|/);
  assert.match(machineMark, /platform === 'darwin' \|\| platform === 'macos'/);
  assert.match(terminalHook, /visualViewport\?\.addEventListener\('resize', onResize\)/);
  assert.match(terminalHook, /visualViewport\?\.removeEventListener\('resize', onResize\)/);
  assert.match(terminalHook, /Android[\s\S]*--interface-scale[\s\S]*term\.resize\(Math\.floor\(term\.cols \* scale\), term\.rows\)/);
  assert.match(terminalStyles, /\.terminal-host \.xterm \{[\s\S]*width: 100%;/);
  assert.match(newSession, /<\/div>\s*<span className="launcher-send-hint">Enter sends/,
    'the keyboard hint must be outside the bordered composer');

  const browser = await puppeteer.launch({ headless: true });
  try {
    const page = await browser.newPage();
    for (const width of [360, 390, 430]) {
      await page.setViewport({ width, height: 844 });
      await page.setContent(`
        <style>${styles}</style>
        <main class="new-session-surface">
          <form class="dialog dialog-wide new-session-launcher is-embedded">
            <div class="dialog-body">
              <div class="field launcher-task-field launcher-composer input-composer" id="composer">
                <textarea class="input-textarea" placeholder="Ask an agent to work…"></textarea>
                <div class="launcher-composer-footer input-composer-footer">
                  <div class="launcher-composer-context" aria-label="New session configuration">
                    <div class="launcher-model-control" data-label="Model"><div class="model-picker is-compact" id="model"><button class="model-picker-trigger"><span>◉</span><span>Codex default</span><span>⌄</span></button></div></div>
                    <label class="launcher-effort-chip" data-label="Effort" id="effort"><select><option>Default effort</option></select></label>
                    <label class="launcher-permissions-chip" data-label="Access" id="access"><select><option>Ask me</option></select></label>
                  </div>
                  <div class="launcher-composer-actions" id="actions"><button class="btn btn-primary launcher-composer-start">↑</button></div>
                </div>
              </div>
              <span class="launcher-send-hint" id="hint">Enter sends · Shift+Enter adds a line</span>
            </div>
          </form>
        </main>
      `);
      const layout = await page.evaluate(() => {
        const rect = (selector) => {
          const value = document.querySelector(selector).getBoundingClientRect();
          return { top: value.top, right: value.right, bottom: value.bottom, left: value.left, width: value.width, height: value.height };
        };
        return {
          composer: rect('#composer'), hint: rect('#hint'), actions: rect('#actions'),
          model: rect('#model .model-picker-trigger'), effort: rect('#effort select'), access: rect('#access select')
        };
      });
      for (const [name, control] of Object.entries({ model: layout.model, effort: layout.effort, access: layout.access })) {
        assert.ok(control.left >= 0 && control.right <= width, `${name} escaped the ${width}px viewport`);
        assert.ok(control.height >= 44, `${name} tap target was ${control.height}px at ${width}px`);
      }
      assert.ok(layout.model.bottom <= layout.effort.top, `model did not keep its own row at ${width}px`);
      assert.ok(layout.effort.right <= layout.access.left, `effort and access overlapped at ${width}px`);
      assert.ok(layout.effort.bottom <= layout.actions.top && layout.access.bottom <= layout.actions.top,
        `Start was not below configuration at ${width}px`);
      assert.ok(layout.hint.top >= layout.composer.bottom, `keyboard hint overlapped the composer border at ${width}px`);
    }
  } finally {
    await closeBrowser(browser);
  }
} finally {
  await rm(scratch, { recursive: true, force: true });
}

console.log('android mobile smoke: ok');
