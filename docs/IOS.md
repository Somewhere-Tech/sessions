# iOS client

Sessions for iOS is a client-only Tauri 2 application. It uses the same React
application as macOS, Windows, and Android, but it does not install `sessionsd`,
start agent CLIs, or host runners on the phone. Work stays on Sessions computers;
the iOS app talks to its paired host, which can relay the host's approved fleet.

## Product shape

- **iPhone:** bottom navigation for Home, Sessions, Daily, Search, and More.
  Sessions opens the complete manager/child hierarchy; selecting a row pushes
  the existing Conversation / Terminal / Details view with a back action
  ([`frontend/src/App.tsx`](../frontend/src/App.tsx),
  [`frontend/src/components/MobileNav.tsx`](../frontend/src/components/MobileNav.tsx)).
- **iPad:** the same layout progressively becomes the desktop rail + session
  navigator + active-session workspace. There is no separate tablet codebase
  ([`frontend/src/styles/globals.css`](../frontend/src/styles/globals.css)).
- **Pairing now:** run `sessions pair` on a host with LAN or Tailscale
  reachability, then use **Scan a pairing code**. The QR carries every endpoint
  kind and grants the phone its own revocable credential immediately; pasting
  the link is the non-camera fallback
  ([`frontend/src/components/ConnectScreen.tsx`](../frontend/src/components/ConnectScreen.tsx),
  [`frontend/src/lib/hostedBootstrap.ts`](../frontend/src/lib/hostedBootstrap.ts)).
- **Host-owned setup:** the phone does not present the host's first-run
  onboarding. It inherits the connected computer's machine-level choices and
  shows host runtime, connection, provider, delegated-access, and AI
  settings as read-only; appearance and other device-local choices remain
  editable ([`frontend/src/App.tsx`](../frontend/src/App.tsx),
  [`frontend/src/components/SettingsView.tsx`](../frontend/src/components/SettingsView.tsx)).
- **Transport:** direct LAN or Tailscale connectivity to the paired host. That
  user-owned host can relay to machines it already has approval for; there is no
  Somewhere-hosted relay, hosted terminal stream, analytics connection, or model API call
  ([`docs/NETWORK_SECURITY.md`](NETWORK_SECURITY.md)).

The iOS app declares `_sessions._tcp` Bonjour browsing and local-network access,
and permits local resources through
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

Enable LAN access or sign into Tailscale on the host, run `sessions pair`, tap
**Scan a pairing code**, and scan the QR. The camera permission is requested
only by that action. The paste-link field is the fallback when scanning is
inconvenient. A code is consumed once and expires in ten minutes by default;
the phone claims its credential and opens the host without another approval.
Revoke the phone later in Settings › Fleet with **Forget**, or with `sessions
devices`
([`runtime/cmd/sessions/pair.go`](../runtime/cmd/sessions/pair.go),
[`runtime/cmd/sessions/devices.go`](../runtime/cmd/sessions/devices.go)).

Alternatively, tap **Sign in**, enter the emailed Somewhere code, and choose a
machine from the account fleet. The phone registers an endpoint-free signing
identity and each same-account host verifies that identity through its own
owner-scoped directory token before issuing the phone a device credential.
Other accounts still require a pairing code or an accepted access request.

Without account sign-in, pair with one Mac and the phone also sees that Mac's approved Sessions fleet.
The paired Mac stays first, and its other saved machines appear in Fleet and the
all-machines inbox; opening or acting on one is relayed through the Mac with the
Mac's own independently revocable credential. An unreachable machine stays
visible as offline. The phone never receives the other machine's credential,
and each other machine can revoke the Mac exactly as before.
