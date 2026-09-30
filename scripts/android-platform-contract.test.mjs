import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const root = new URL('../', import.meta.url);
const read = (path) => readFile(new URL(path, root), 'utf8');

test('Android SDK setup skips retired tools without weakening license acceptance', async () => {
  const workflow = await read('.github/workflows/android-preview.yml');
  const setup = workflow.match(/- name: Set up Android SDK[\s\S]*?(?=\n      - name:|$)/)?.[0];
  assert.ok(setup, 'the reviewed setup action must remain explicit');
  assert.match(setup, /android-actions\/setup-android@9fc6c4e9069bf8d3d10b2204b1fb8f6ef7065407/);
  assert.match(setup, /packages: platform-tools\s*\n/,
    'override the action default that requests the unavailable tools package');
  assert.match(setup, /accept-android-sdk-licenses: true/);
  assert.match(setup, /log-accepted-android-sdk-licenses: false/,
    'suppress license text, not license acceptance or failures');
  const install = workflow.match(/- name: Install reviewed Android toolchain[\s\S]*?(?=\n      - name:|$)/)?.[0];
  assert.ok(install);
  for (const packageName of ['platform-tools', 'platforms;android-36', 'build-tools;36.0.0', 'ndk;${ANDROID_NDK_VERSION}']) {
    assert.ok(install.includes(`"${packageName}"`), `${packageName} stays in the reviewed install step`);
  }
});

test('Android local test packaging is explicit and leaves normal builds unchanged', async () => {
  const [gradle, manifest, workflow, androidDocs] = await Promise.all([
    read('src-tauri/gen/android/app/build.gradle.kts'),
    read('src-tauri/gen/android/app/src/main/AndroidManifest.xml'),
    read('.github/workflows/android-preview.yml'),
    read('docs/ANDROID.md')
  ]);

  assert.match(gradle, /gradleProperty\("sessionsTestApp"\)\.orNull == "true"/);
  assert.match(gradle, /applicationId = "tech\.somewhere\.sessions"/);
  assert.match(gradle, /applicationIdSuffix = "\.debug"/,
    'Tauri must retain the normal debug suffix it regenerates before a build');
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
  assert.match(androidDocs,
    /debugApplicationIdSuffix\\?"\s*:\s*\\?"\.local\\?"/,
    'the side-by-side recipe must ask Tauri to generate the local suffix');
  assert.match(workflow, /npm run test:android-package/,
    'the Android packaging workflow must keep this contract gated');
});

test('Android paired-machine protection is wired through the native vault and excludes backups', async () => {
  const [lib, commands, bridge, plugin, storage, proguard, manifest, backups, extraction] = await Promise.all([
    read('src-tauri/src/lib.rs'), read('src-tauri/src/native_commands.rs'),
    read('src-tauri/src/android_credentials.rs'),
    read('src-tauri/gen/android/app/src/main/java/tech/somewhere/sessions/MachineCredentialsPlugin.kt'),
    read('src-tauri/gen/android/app/src/main/java/tech/somewhere/sessions/AndroidMachineVault.kt'),
    read('src-tauri/gen/android/app/proguard-rules.pro'),
    read('src-tauri/gen/android/app/src/main/AndroidManifest.xml'),
    read('src-tauri/gen/android/app/src/main/res/xml/backup_rules.xml'),
    read('src-tauri/gen/android/app/src/main/res/xml/data_extraction_rules.xml'),
  ]);
  assert.match(lib, /app\.handle\(\)\.plugin\(android_credentials::init\(\)\)/);
  assert.match(commands, /android_credentials::load/);
  assert.match(commands, /android_credentials::save/);
  assert.match(bridge, /register_android_plugin\("tech\.somewhere\.sessions", "MachineCredentialsPlugin"\)/);
  assert.match(bridge, /stored\.credentials != expected/);
  assert.match(plugin, /Executors\.newSingleThreadExecutor/);
  assert.match(plugin, /MachineVaultTransaction\(AndroidMachineVault/);
  assert.match(storage, /context\.noBackupFilesDir/);
  assert.match(storage, /KeyStore\.getInstance\("AndroidKeyStore"\)/);
  assert.match(storage, /setRandomizedEncryptionRequired\(true\)/);
  assert.doesNotMatch(storage, /SharedPreferences|MODE_WORLD_READABLE|\.claude|\.codex/);
  for (const name of ['MachineCredentialsPlugin', 'MachineCredentialSaveArgs']) {
    assert.ok(proguard.includes(`-keep class tech.somewhere.sessions.${name} { *; }`));
  }
  assert.match(manifest, /android:allowBackup="false"/);
  assert.match(manifest, /android:fullBackupContent="@xml\/backup_rules"/);
  assert.match(manifest, /android:dataExtractionRules="@xml\/data_extraction_rules"/);
  for (const domain of ['root', 'file', 'database', 'sharedpref', 'external']) {
    const rule = `<exclude domain="${domain}" path="." />`;
    assert.ok(backups.includes(rule));
    assert.equal(extraction.split(rule).length - 1, 2, `${domain} excluded from backup and device transfer`);
  }
});
