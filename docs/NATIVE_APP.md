# Native application contract

Sessions.app is the primary user interface and package for the local Sessions
runtime. Tauri supplies the native window, tray, permissions, platform credential
adapters, installer, and updater around the shared React interface.

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
- user-facing OS permission context and the launch-agent responsibility link;
- platform-specific storage for paired-machine credentials, as scoped below;
- runtime staging, installation status, and signed update UI;
- native tailnet and mobile Bonjour discovery adapters.

The daemon owns:

- session creation, input, interruption, and termination;
- runner adoption and recovery;
- ledger, history, search, usage, hierarchy, and compatibility facts;
- host Bonjour discovery, LAN connection, and saved-machine fleet relay;
- authenticated HTTP/WebSocket contracts used by every client.

Operational settings and controls require CLI/JSON parity unless they are
inherently visual or an OS-owned prompt.

## Packaging and updates

Runtime binaries are staged as immutable versioned bytes with a manifest, and
the managed daemon runs from its versioned copy. The daemon may advance while
compatible existing runners keep running. On macOS, runner jobs launch one
stable `sessions-runner` path, which an update replaces with the new runner
once the updated daemon is ready. A runner already running keeps the process
image it started with, but macOS may then report that its code no longer
matches the file on disk, and any later launch, wake, or crash restart from
that path uses the bytes currently there. A package update must re-adopt every
baseline session that remains live. A session that exits during the readiness
check, or whose user-end boundary is already recorded, satisfies that
baseline; one that disappears or remains unreachable without either fact makes
the update refuse or roll back.

macOS releases require Developer ID signatures for the app and nested
binaries, notarization, stapling, Gatekeeper acceptance, a pinned updater
signature, immutable download identity, and checksum verification.
Release automation stages verified assets in a GitHub draft; it does not
publish the release, mark it latest, or promote the hosted updater. Maintainers
verify and install the exact signed draft bytes before explicitly authorizing
publication. Existing draft assets are not overwritten automatically, and
changing their bytes requires repeating acceptance. Public runtime-pin checks,
npm publication, and hosted download/updater promotion are separate steps.
Each Darwin runtime binary also carries a Mach-O `__TEXT,__info_plist` section
with its stable bundle identifier, Local Network usage description, and
`_sessions._tcp` Bonjour declaration. The app-managed sessionsd launch agent
declares `AssociatedBundleIdentifiers = [tech.somewhere.sessions]`, which
`launchd.plist(5)` documents as System Settings › Login Items attribution.
macOS Local Network privacy can still identify sessionsd by its own runtime
identifier, and that binary carries its own usage description.
Onboarding's Fleet step and Settings › Fleet › **Allow local network**
start the first daemon-owned browse so any macOS prompt appears in context;
the app does not fabricate or preflight a permission result.

New app-managed runner jobs carry the same association when their executable
comes from `~/Library/Application Support/Sessions/runtime/`. Standalone and
scratch runners do not borrow that association. It does not grant file access,
override a denied macOS privacy decision, or change the provider's
approval/sandbox policy, and macOS privacy logs for associated runners still
name `sessions-runner` as the responsible process. Existing runner jobs are not
restarted or rewritten by this change.
The app and runtime binaries also declare a removable-volume usage description
explaining project and shared Git metadata access when macOS requests consent.

Each runner is its own launchd job, so macOS may name `sessions-runner` in a
privacy prompt caused by anything its provider runs: the agent, shells, hooks,
or commands such as `find`. Approved folder and removable-volume access has
been observed to apply to later runners. "Would like to access data from other
apps" consent is different: Apple documents it as lasting only for the
requesting process, so a new or restarted runner can ask again. It is a known
issue that the same running runner can also ask again immediately after
approval; the cause is not yet established, and Sessions cannot make this
consent permanent or approve it on the user's behalf. Denying it makes that
protected read fail, which can stop or change the provider's work even though
the session stays running. Keeping agent searches inside the project reduces
unnecessary requests but does not guarantee none.

A Git worktree can keep its shared metadata on a different drive from its
working files. Failed worktree inspections retain the Git error and, when the
daemon cannot read the `.git` target or its `commondir`, name that inaccessible
path. Reconnect the source drive and review Sessions' Files and Folders access
in macOS Privacy & Security. A successful daemon inspection does not prove
access inside a provider sandbox; grant only the additional workspace access
that provider requests. Do not substitute copied source trees for recovery.

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

At a new boot Sessions automatically restores at most eight non-lane session
roots that were actually running before shutdown: pinned roots first, then the
roots a person spoke to within the last 24 hours, most recent first. It never
automatically repeats a headless lane. Other prior runners stay paused; their
metadata, event logs, transcripts, and launch records remain intact and a
`restore-pending` marker records why. The inbox lists them under "Not
connected" with that reason. Browsing saved history in the app stays read-only.
Explicit Resume continues the selected provider conversation and may create a
linked runtime. Sending a message or reading it live wakes a paused runner in
place with the same id, conversation, and folder. Daemon discovery on its own
never spawns a provider or deletes
that recovery evidence. When a wake fails, live commands fail with
`SESSION_NEEDS_RECREATE`, the failure as the reason, and a resume action
instead of returning an empty successful result.

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

Android and iOS builds are paired clients, not mobile daemon hosts. They reuse
the authenticated daemon contract and adapt the shared interface to phone and tablet
layouts. A phone does not run host onboarding or change host-owned runtime
choices; it reads those settings from the connected computer and presents them
read-only while keeping device-local choices editable.

## Paired-machine credential storage

The candidate implements protected native storage for saved machine tokens on
these platforms:

- **Windows:** user-scope DPAPI with an owner-restricted native credential file.
- **macOS:** login Keychain, without iCloud synchronization.
- **iOS:** device-only Keychain items available while the device is unlocked,
  without iCloud synchronization.
- **Android:** a bounded AES-256-GCM vault with a non-exportable Android Keystore
  key and verified write/readback. Local credential data is excluded from
  backup and device transfer; a replacement device must pair again. This does
  not guarantee hardware-backed key storage on every Android device.

On these platforms, migration removes legacy plaintext tokens from WebView
local storage only after saving and reading them back from the native store.
A locked, unreadable, or unverified protected store reports an error rather
than silently replacing it with an empty store or falling back to plaintext.

Linux native clients and browser clients do not yet have this
protected-store implementation in this candidate. Their saved tokens remain
in local client storage; do not describe that storage as an OS credential vault.
Interactive browser control is deprecated. Platform source and fixture tests
are not a substitute for native device acceptance.

This boundary covers Sessions pairing tokens, not Claude, Codex, or other
provider logins. Provider account authentication remains provider-owned and
separate from client-to-daemon pairing.

## Release gate

1. Build and test with isolated scratch state.
2. Verify the exact nested signatures, package identity, updater signature, and
   checksums.
3. Rehearse install/update against idle and disposable live-session baselines.
4. Confirm viewer exit and relaunch preserve runners.
5. Publish only the exact tested bytes and record truthful platform coverage.

Source-only, CI-tested, hardware-tested, signed, released, and installed are
different states and must never be reported as synonyms.
