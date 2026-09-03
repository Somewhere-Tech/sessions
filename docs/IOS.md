# iOS client

Sessions for iOS is a client-only Tauri 2 application. It uses the same React
application as macOS, Windows, and Android, but it does not install `sessionsd`,
start agent CLIs, or host runners on the phone. Work stays on a paired Sessions
computer and the iOS app talks directly to that host.

## Product shape

- **iPhone:** bottom navigation for Home, Sessions, Daily, Search, and More.
  Sessions opens the complete manager/child hierarchy; selecting a row pushes
  the existing Conversation / Terminal / Details view with a back action
  ([`frontend/src/App.tsx`](../frontend/src/App.tsx),
  [`frontend/src/components/MobileNav.tsx`](../frontend/src/components/MobileNav.tsx)).
- **iPad:** the same layout progressively becomes the desktop rail + session
  navigator + active-session workspace. There is no separate tablet codebase
  ([`frontend/src/styles/globals.css`](../frontend/src/styles/globals.css)).
- **Pairing now:** enable trusted-network access with `sessions lan enable`,
  then run `sessions pair` on a host and paste its one-time link in the iOS app.
  The ticket is consumed once and the device receives its own revocable
  credential
  ([`frontend/src/components/ConnectScreen.tsx`](../frontend/src/components/ConnectScreen.tsx),
  [`frontend/src/lib/hostedBootstrap.ts`](../frontend/src/lib/hostedBootstrap.ts)).
- **Transport:** direct LAN or Tailscale connectivity only. There is no Sessions
  relay, hosted terminal stream, analytics connection, or model API call
  ([`docs/NETWORK_SECURITY.md`](NETWORK_SECURITY.md)).

The iOS app declares local-network access and permits local resources through
App Transport Security because a user-approved LAN or Tailscale host can
intentionally use an `http://` daemon endpoint. The exception is scoped to
local networking; ATS remains enabled for public hosts. Tailscale still
encrypts its network transport, while raw LAN mode is explicitly unencrypted
and should only be used on a trusted network. The app only connects to a paired
or explicitly entered endpoint
([`src-tauri/gen/apple/project.yml`](../src-tauri/gen/apple/project.yml),
[Apple local-network privacy](https://developer.apple.com/documentation/technotes/tn3179-understanding-local-network-privacy),
[Apple ATS local-network exception](https://developer.apple.com/documentation/bundleresources/information-property-list/nsapptransportsecurity/nsallowslocalnetworking)).

## Build a simulator app

Tauri's current iOS prerequisites are Xcode, the three Rust iOS targets, and
CocoaPods ([Tauri iOS prerequisites](https://v2.tauri.app/start/prerequisites/)).
Tauri also installs XcodeGen and `libimobiledevice` through Homebrew when they
are missing.

```sh
export DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer
export PATH="$DEVELOPER_DIR/usr/bin:$PATH"

rustup target add \
  aarch64-apple-ios aarch64-apple-ios-sim x86_64-apple-ios
brew install cocoapods

xcodebuild -downloadPlatform iOS -architectureVariant arm64 # if no runtime is installed
npx tauri ios init --ci                                      # only if src-tauri/gen/apple is absent
npx tauri ios build --debug --target aarch64-sim --ci
```

The simulator bundle is:

```text
src-tauri/gen/apple/build/arm64-sim/Sessions.app
```

Install and launch it in an available simulator with:

```sh
xcrun simctl boot "iPhone 17 Pro" 2>/dev/null || true
open -a Simulator
xcrun simctl bootstatus "iPhone 17 Pro" -b
xcrun simctl install booted src-tauri/gen/apple/build/arm64-sim/Sessions.app
xcrun simctl launch --terminate-running-process booted tech.somewhere.sessions
xcrun simctl io booted screenshot /tmp/sessions-ios.png
```

A simulator build uses local ad-hoc signing. A physical-iPhone build additionally
needs an Apple developer team and an iOS development signing identity and
provisioning profile. Open the generated project in Xcode, choose the team for
the `sessions-app_iOS` target under **Signing & Capabilities**, select the phone,
and build there. Those account choices and credentials do not belong in this
repository.

## Pair a phone

On a Mac that already runs the current Sessions runtime and is on the same
trusted network as the phone:

```sh
sessions lan enable
sessions pair
```

Keep the phone on a network that can reach the endpoint printed in that link.
Open Sessions on iOS, paste the full link under **Connect with a one-time
link**, and tap **Connect this device**. If the link expires or was already
consumed, generate a new one. Revoke the phone later with `sessions devices`
([`runtime/cmd/sessions/pair.go`](../runtime/cmd/sessions/pair.go),
[`runtime/cmd/sessions/devices.go`](../runtime/cmd/sessions/devices.go)).
