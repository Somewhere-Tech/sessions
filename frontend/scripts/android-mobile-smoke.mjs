import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { build } from 'esbuild';

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

  const [activity, manifest, app, appBack, mobileNav, connect, machineMark, terminalHook, terminalStyles] = await Promise.all([
    source('../src-tauri/gen/android/app/src/main/java/tech/somewhere/sessions/MainActivity.kt'),
    source('../src-tauri/gen/android/app/src/main/AndroidManifest.xml'),
    source('src/App.tsx'),
    source('src/hooks/useAndroidBackNavigation.ts'),
    source('src/components/MobileNav.tsx'),
    source('src/components/ConnectScreen.tsx'),
    source('src/components/MachineMark.tsx'),
    source('src/hooks/useTerminal.ts'),
    source('src/styles/sections/42-terminal-interactions.css')
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
} finally {
  await rm(scratch, { recursive: true, force: true });
}

console.log('android mobile smoke: ok');
