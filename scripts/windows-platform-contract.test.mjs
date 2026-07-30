import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const root = new URL('../', import.meta.url);
const read = (path) => readFile(new URL(path, root), 'utf8');

test('Windows uses the signed app updater and appears as a versioned Fleet host', async () => {
  const [
    appConfigText,
    windowsBundleText,
    windowsReleaseText,
    sidebar,
    bridge,
    fleet,
    health,
    workflow,
    frontendPackageText,
    windowsRuntimeBuild,
    windowsReleaseScript
  ] = await Promise.all([
    read('src-tauri/tauri.conf.json'),
    read('src-tauri/tauri.windows.conf.json'),
    read('src-tauri/tauri.windows.release.conf.json'),
    read('frontend/src/components/ProductSidebar.tsx'),
    read('frontend/src/lib/tauriBridge.ts'),
    read('frontend/src/components/FleetView.tsx'),
    read('runtime/internal/api/server.go'),
    read('.github/workflows/windows-preview.yml'),
    read('frontend/package.json'),
    read('scripts/build-app-runtime.ps1'),
    read('scripts/release-windows.ps1')
  ]);

  const appConfig = JSON.parse(appConfigText);
  const windowsBundle = JSON.parse(windowsBundleText);
  const windowsRelease = JSON.parse(windowsReleaseText);

  assert.equal(appConfig.plugins.updater.endpoints[0], 'https://sessions.somewhere.tech/releases/latest.json');
  assert.match(appConfig.plugins.updater.pubkey, /\S/);
  assert.equal(windowsBundle.bundle.windows.nsis.installMode, 'currentUser');
  assert.equal(windowsRelease.bundle.createUpdaterArtifacts, true);
  assert.equal(windowsRelease.plugins.updater.windows.installMode, 'passive');
  assert.equal(windowsRelease.bundle.windows.digestAlgorithm, 'sha256');
  assert.match(windowsRelease.bundle.windows.timestampUrl, /^https?:\/\//);

  assert.match(sidebar, /setTimeout\(\(\) => void automaticCheck\(\), 1_500\)/);
  assert.match(sidebar, /checkForNativeUpdate\(\)/);
  assert.match(sidebar, /notifyNativeUpdate\(available\)/);
  assert.match(sidebar, /'Update app'/);
  assert.doesNotMatch(sidebar, /darwin|macos|target_os/);
  assert.match(bridge, /pendingUpdate\.downloadAndInstall/);
  assert.match(bridge, /await relaunch\(\)/);

  assert.match(health, /goruntime\.GOOS/);
  assert.match(health, /goruntime\.GOARCH/);
  assert.match(fleet, /reported\.includes\('windows'\)/);
  assert.match(fleet, /if \(platform === 'windows'\) return 'This PC'/);
  assert.match(fleet, /machineVersionState/);
  assert.match(fleet, /Sessions \$\{version\}/);

  assert.match(workflow, /npm run test:updater-release/);
  assert.match(workflow, /npm --prefix frontend run test:smoke/);
  const frontendPackage = JSON.parse(frontendPackageText);
  assert.match(frontendPackage.scripts['test:smoke'], /npm run test:fleet-clarity/);
  assert.match(workflow, /runs-on: windows-2022/);
  assert.match(windowsRuntimeBuild, /package\.json/);
  assert.match(windowsRuntimeBuild, /v\$AppVersion-dev\.g\$SourceCommit/);
  for (const script of [windowsRuntimeBuild, windowsReleaseScript]) {
    assert.match(script, /\[System\.Security\.Cryptography\.SHA256\]::Create\(\)/);
    assert.doesNotMatch(script, /Get-FileHash/);
  }
});
