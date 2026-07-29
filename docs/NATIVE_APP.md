# Native application contract

Sessions.app is the primary user interface and package for the local Sessions
runtime. Tauri supplies the native window, tray, permissions, secure client
storage, installer, and updater around the shared React interface.

The Go runtime remains independently useful through the `sessions` CLI and is
not owned by the viewer.

## Lifetime boundary

The package contains three runtime binaries:

- `sessions` — CLI and agent-facing JSON surface;
- `sessionsd` — per-user daemon and API;
- `sessions-runner` — independent owner of one provider, shell, or headless
  command.

The native process may install, inspect, and request an update of the daemon.
It never becomes the parent whose exit ends a runner. Closing a view, quitting
the app, or installing a compatible update must not silently terminate work.

Explicit lifecycle verbs stay literal:

- **Close view** removes only a UI surface.
- **End session** asks the runner to stop its process tree and preserves the
  record.
- **Continue** resumes supported provider history or creates an explicitly
  linked cross-provider continuation.
- **Archive** removes a retained row from the routine list without deleting
  provider history.

## Native and runtime responsibilities

The native shell owns:

- scoped desktop/mobile windows and platform navigation;
- tray and native notifications;
- OS permission prompts;
- secure storage for paired-machine credentials;
- runtime staging, installation status, and signed update UI;
- native LAN and tailnet discovery adapters.

The daemon owns:

- session creation, input, interruption, and termination;
- runner adoption and recovery;
- ledger, history, search, usage, hierarchy, and compatibility facts;
- authenticated HTTP/WebSocket contracts used by every client.

Operational settings and controls require CLI/JSON parity unless they are
inherently visual or an OS-owned prompt.

## Packaging and updates

Runtime binaries are staged as immutable versioned bytes with a manifest. The
managed daemon may advance while compatible existing runners keep their
original runtime until they exit. A package update must preserve the exact live
session baseline or refuse/roll back.

macOS releases require Developer ID signatures for the app and nested
binaries, notarization, stapling, Gatekeeper acceptance, a pinned updater
signature, immutable download identity, and checksum verification.

Windows releases require a current-user installer, Authenticode, the pinned
updater signature, manifest verification, and the hardware matrix in
[`WINDOWS_TEST.md`](WINDOWS_TEST.md).

The viewer checks for updates in the background but installation remains an
explicit user or authorized local-agent action. Update checks do not send
session content.

## Network and browser boundary

The native client may connect to:

- the local loopback daemon;
- an explicitly enabled trusted-LAN listener;
- a verified host on the user's tailnet;
- optional services the user deliberately configures.

The interactive daemon-served browser surface is deprecated and must not grow
new terminal or agent-control features. See
[`NETWORK_SECURITY.md`](NETWORK_SECURITY.md) for outbound and support-access
rules.

## Mobile clients

Android and future iOS builds are paired clients, not mobile daemon hosts. They
reuse the authenticated daemon contract, store revocable device credentials in
platform secure storage, and adapt the shared interface to phone and tablet
layouts. Mobile notifications use the explicit encrypted notification path and
do not grant broader host authority.

## Release gate

1. Build and test with isolated scratch state.
2. Verify the exact nested signatures, package identity, updater signature, and
   checksums.
3. Rehearse install/update against idle and disposable live-session baselines.
4. Confirm viewer exit and relaunch preserve runners.
5. Publish only the exact tested bytes and record truthful platform coverage.

Source-only, CI-tested, hardware-tested, signed, released, and installed are
different states and must never be reported as synonyms.
