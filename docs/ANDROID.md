# Android client

Sessions for Android is a client-only Tauri 2 application. It uses the same
React application as macOS and Windows, but it does not install `sessionsd`,
start agent CLIs, or host runners on the phone. Work stays on a paired Sessions
computer and the Android app talks directly to that host.

## Product shape

- **Phone:** bottom navigation for Home, Sessions, Daily, Search, and More.
  Sessions opens the complete manager/child hierarchy; selecting a row pushes
  the existing Conversation / Terminal / Details view with a back action
  ([`frontend/src/App.tsx`](../frontend/src/App.tsx),
  [`frontend/src/components/MobileNav.tsx`](../frontend/src/components/MobileNav.tsx)).
- **Tablet and foldable:** the same layout progressively becomes the desktop
  rail + session navigator + active-session workspace. There is no separate
  tablet codebase
  ([`frontend/src/styles/globals.css`](../frontend/src/styles/globals.css)).
- **Pairing now:** enable trusted-network access with `sessions lan enable`,
  then run `sessions pair` on a host and paste its one-time link in the Android
  app. The ticket is consumed once and the device receives its own revocable
  credential
  ([`frontend/src/components/ConnectScreen.tsx`](../frontend/src/components/ConnectScreen.tsx),
  [`frontend/src/lib/hostedBootstrap.ts`](../frontend/src/lib/hostedBootstrap.ts)).
- **Transport:** direct LAN or Tailscale connectivity only. There is no Sessions
  relay, hosted terminal stream, analytics connection, or model API call
  ([`docs/NETWORK_SECURITY.md`](NETWORK_SECURITY.md)).

Android release builds permit cleartext network traffic because a user-approved
LAN or Tailscale host can intentionally use an `http://` daemon endpoint.
Tailscale still encrypts its network transport; raw LAN mode is explicitly
unencrypted and should only be used on a trusted network. The app never scans
the public internet and only connects to a paired or explicitly entered
endpoint
([`src-tauri/gen/android/app/build.gradle.kts`](../src-tauri/gen/android/app/build.gradle.kts),
[`src-tauri/gen/android/app/src/main/AndroidManifest.xml`](../src-tauri/gen/android/app/src/main/AndroidManifest.xml)).

Automatic Android Bonjour/Tailscale discovery, request/accept onboarding, and
background FCM delivery are not represented as shipped. Trusted-LAN pair-link
onboarding and foreground session control work without them
([`frontend/src/components/ConnectScreen.tsx`](../frontend/src/components/ConnectScreen.tsx)).

## Build a sideloadable test APK

Tauri's current Android prerequisites are JDK 17, the Android SDK platform and
build tools, platform tools, a side-by-side NDK, and the four Rust Android
targets ([Tauri Android prerequisites](https://v2.tauri.app/start/prerequisites/)).
On this repository's development Mac they are installed through Homebrew under
`/opt/homebrew`.

```sh
export JAVA_HOME=/opt/homebrew/opt/openjdk@17/libexec/openjdk.jdk/Contents/Home
export ANDROID_HOME=/opt/homebrew/share/android-commandlinetools
export NDK_HOME="$ANDROID_HOME/ndk/29.0.14206865"

rustup target add \
  aarch64-linux-android armv7-linux-androideabi \
  i686-linux-android x86_64-linux-android

npx tauri android init --ci        # only when src-tauri/gen/android is absent
npx tauri android build --debug --apk --target aarch64 --ci
```

The installable debug APK is:

```text
src-tauri/gen/android/app/build/outputs/apk/universal/debug/app-universal-debug.apk
```

Install it over USB debugging with:

```sh
"$ANDROID_HOME/platform-tools/adb" install -r \
  src-tauri/gen/android/app/build/outputs/apk/universal/debug/app-universal-debug.apk
```

The debug APK is signed automatically by the Android development toolchain.
A public release must use the Somewhere production Android keystore or Play App
Signing; those credentials do not belong in this repository
([`src-tauri/gen/android/app/build.gradle.kts`](../src-tauri/gen/android/app/build.gradle.kts)).

## Pair a phone

On a Mac that already runs the current Sessions runtime and is on the same
trusted network as the phone:

```sh
sessions lan enable
sessions pair
```

Keep the phone on a network that can reach the endpoint printed in that link.
Open Sessions on Android, paste the full link under **Connect with a one-time
link**, and tap **Connect this device**. If the link expires or was already
consumed, generate a new one. Revoke the phone later with `sessions devices`
([`runtime/cmd/sessions/pair.go`](../runtime/cmd/sessions/pair.go),
[`runtime/cmd/sessions/devices.go`](../runtime/cmd/sessions/devices.go)).
