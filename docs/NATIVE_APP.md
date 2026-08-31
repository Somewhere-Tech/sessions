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
It never becomes the parent whose exit ends a runner. Closing a tab, quitting
the app, or installing a compatible update must not silently terminate work.

Explicit lifecycle verbs stay literal:

- **Close tab** removes only the open UI tab. The session remains in Live and
  keeps running.
- **Set aside** closes its open tab and moves the session out of the default
  Live working set. It does not stop the agent and remains reversible.
- **End session** asks the runner to stop its process tree and preserves the
  record.
- **Continue** resumes supported provider history or creates an explicitly
  linked cross-provider continuation.
- **Archive** removes a retained row from the routine list without deleting
  provider history.
- **Remove integration** (`--remove-integration`, run by the Windows
  uninstaller and by hand on macOS) removes the per-user integration points the
  package wrote outside itself: the login service definition, Sessions-managed
  `sessions` symlinks, and on Windows the logon supervisor value and managed
  PATH entry. It stops no process and deletes no user data, and it reports what
  it kept rather than implying it was thorough.

That last verb is the lifetime boundary applied to the last thing the viewer
does. Ending the daemon during a removal would orphan every live runner, and
the daemon is the only process that can still record what a runner produces —
so deleting the definition, which simply stops the daemon returning at the next
login, is the whole of it. Session records, the ledger, the saved port, paired
credentials, and the runtime bytes a live daemon is executing all survive.

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

## Login and crash recovery

macOS runner supervision is boot-scoped. A per-runner permit lets launchd
restart an unexpected runner crash during the same boot, but retained history
does not imply permission to launch every provider again after login.

At a new boot Sessions automatically restores at most eight explicitly pinned,
non-lane session roots that were actually running before shutdown. It never
automatically repeats a headless lane. Other prior runners stay stopped; their
metadata, event logs, transcripts, and launch records remain intact and a
`restore-pending` marker records why automatic restoration paused. Daemon
discovery reconciles those records without spawning providers or deleting that
recovery evidence.

This is a safety ceiling, not a retention ceiling. It limits the process fanout
that one login can cause while keeping every durable conversation available for
explicit inspection or recovery.

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
