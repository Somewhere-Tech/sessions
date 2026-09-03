# Android client

Sessions for Android is a client-only Tauri 2 application. It uses the same
React application as macOS and Windows, but it does not install `sessionsd`,
start agent CLIs, or host runners on the phone. Work stays on Sessions computers;
the Android app talks to its paired host, which can relay the host's approved fleet.

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
- **Pairing now:** enable trusted-network access with `sessions lan enable`.
  While the connect screen is open, the Android app browses `_sessions._tcp` on
  the local network and lists compatible hosts. Choose one, approve the request
  on that host, and the phone claims its own revocable credential. A one-time
  link from `sessions pair` remains available when multicast discovery cannot
  find the host
  ([`frontend/src/components/ConnectScreen.tsx`](../frontend/src/components/ConnectScreen.tsx),
  [`frontend/src/lib/hostedBootstrap.ts`](../frontend/src/lib/hostedBootstrap.ts)).
- **Transport:** direct LAN or Tailscale connectivity to the paired host. That
  user-owned host can relay to machines it already has approval for; there is no
  Somewhere-hosted relay, hosted terminal stream, analytics connection, or model API call
  ([`docs/NETWORK_SECURITY.md`](NETWORK_SECURITY.md)).

Android release builds permit cleartext network traffic because a user-approved
LAN or Tailscale host can intentionally use an `http://` daemon endpoint.
Tailscale still encrypts its network transport; raw LAN mode is explicitly
unencrypted and should only be used on a trusted network. The app never scans
the public internet and only connects to a discovered, paired, or explicitly
entered endpoint
([`src-tauri/gen/android/app/build.gradle.kts`](../src-tauri/gen/android/app/build.gradle.kts),
[`src-tauri/gen/android/app/src/main/AndroidManifest.xml`](../src-tauri/gen/android/app/src/main/AndroidManifest.xml)).

Android performs multicast DNS discovery only while the connect screen is in
the foreground; it does not scan addresses or provide automatic Tailscale
discovery. Request/accept onboarding and foreground session control work
without FCM, while background FCM delivery is not represented as shipped
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

Every reviewed push to `main` also runs the **Android client preview**
workflow and retains the same sideloadable APK plus its SHA-256 checksum for
14 days. That artifact is a debug-signed test build, not a Play Store release.

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
```

Keep the app in the foreground while it searches, tap the host under **Your
Sessions machines**, then approve **This phone** in Sessions.app on the host.
The equivalent host CLI is:

```sh
sessions access requests
sessions access accept <request-id>
```

The phone polls the approval, claims its credential, and opens that host. If
multicast discovery is blocked by the network, run `sessions pair` on the host,
paste the full link under **One-time-link fallback**, and tap **Connect this
device**. A link is consumed once; generate another if it expires. Revoke the
phone later with `sessions devices`
([`runtime/cmd/sessions/pair.go`](../runtime/cmd/sessions/pair.go),
[`runtime/cmd/sessions/devices.go`](../runtime/cmd/sessions/devices.go)).

Pair with one Mac and the phone also sees that Mac's approved Sessions fleet.
The paired Mac stays first, and its other saved machines appear in Fleet and the
all-machines inbox; opening or acting on one is relayed through the Mac with the
Mac's own independently revocable credential. An unreachable machine stays
visible as offline. The phone never receives the other machine's credential,
and each other machine can revoke the Mac exactly as before.
