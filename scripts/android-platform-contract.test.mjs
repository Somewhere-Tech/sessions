import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const root = new URL('../', import.meta.url);
const read = (path) => readFile(new URL(path, root), 'utf8');

test('Android local test packaging is explicit and leaves normal builds unchanged', async () => {
  const [gradle, manifest, workflow] = await Promise.all([
    read('src-tauri/gen/android/app/build.gradle.kts'),
    read('src-tauri/gen/android/app/src/main/AndroidManifest.xml'),
    read('.github/workflows/android-preview.yml')
  ]);

  assert.match(gradle, /gradleProperty\("sessionsTestApp"\)\.orNull == "true"/);
  assert.match(gradle, /applicationId = "tech\.somewhere\.sessions"/);
  assert.match(gradle, /applicationIdSuffix = "\.debug"/,
    'Tauri must retain the normal debug suffix it regenerates before a build');
  assert.match(gradle, /if \(sessionsTestApp\) \{\s*setApplicationIdSuffix\("\.local"\)/,
    'the opt-in suffix must use a setter that survives Tauri regeneration');
  assert.match(gradle, /manifestPlaceholders\["sessionsAppLabel"\] = "Sessions"/);
  assert.match(gradle, /if \(sessionsTestApp\) \{[\s\S]*?manifestPlaceholders\["sessionsAppLabel"\] = "Sessions Test"/);
  assert.equal((gradle.match(/applicationIdSuffix\s*=/g) ?? []).length, 1,
    'the existing debug build type must remain the only application ID suffix');
  assert.doesNotMatch(gradle, /productFlavors|flavorDimensions/,
    'a test package must not change Tauri Android task or artifact names');
  assert.equal((manifest.match(/android:label="\$\{sessionsAppLabel\}"/g) ?? []).length, 2,
    'the application and activity must show the same build-selected label');

  const baseID = gradle.match(/applicationId = "([^"]+)"/)?.[1];
  assert.equal(`${baseID}.debug`, 'tech.somewhere.sessions.debug');
  assert.equal(`${baseID}.local`, 'tech.somewhere.sessions.local');
  const afterTauriRewrite = gradle.replace(
    /applicationIdSuffix\s*=\s*[^\n]+/,
    'applicationIdSuffix = ".debug"'
  );
  assert.match(afterTauriRewrite, /setApplicationIdSuffix\("\.local"\)/,
    'Tauri rewriting its direct assignment must not remove the local override');
  assert.match(workflow, /npm run test:android-package/,
    'the Android packaging workflow must keep this contract gated');
});
