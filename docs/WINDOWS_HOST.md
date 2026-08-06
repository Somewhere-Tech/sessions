# Windows host architecture

Sessions for Windows follows the same lifetime and protocol contracts as the
macOS runtime. The native viewer is not the owner of the daemon, runners, or
provider processes. Closing or updating the viewer must leave durable work
alive.

The Windows host is a preview until a signed build passes the public hardware
matrix in [`WINDOWS_TEST.md`](WINDOWS_TEST.md). Source, CI compilation, hardware
validation, signing, publication, and installation are distinct delivery
states.

## Process ownership

Sessions runs in the signed-in user's desktop session. A per-user logon
supervisor owns only the daemon. Each `sessions-runner` independently owns one
provider or shell process and remains available for compatible daemon
re-adoption.

Running the host as LocalSystem is not supported: agent tools need the user's
profile, desktop session, environment, and credential scope.

## Platform adapters

| Unix/macOS contract | Windows implementation | Public invariant |
| --- | --- | --- |
| per-user launch agent | per-user logon supervisor | viewer exit does not end work |
| PTY | ConPTY | UTF-8 input/output, resize, and replay |
| process groups/signals | Job Objects and console control | explicit End owns process-tree termination |
| Unix runner socket | local Named Pipe | signed-in-user access and peer verification |
| user-scoped secret storage | DPAPI-protected local storage | no secrets in arguments, logs, or links |
| Developer ID updater | Authenticode plus pinned updater signature | verify before swap; preserve or roll back |

The daemon API, runner protocol, ledger, search, usage, hierarchy, lifecycle
reasons, and CLI JSON remain platform-neutral. Narrow adapters own Windows
process, transport, terminal, credential, and installation behavior.

## Security boundaries

- Named Pipes are local-only, use an owner-scoped access policy, and validate
  the connecting process identity.
- DPAPI protects persisted Sessions credentials from other users and offline
  copying. It is not isolation from arbitrary code already executing as the
  same signed-in user.
- Claude, Codex, Git, and Somewhere credentials remain provider-owned. Sessions
  does not copy them into its credential store.
- Provider children remain inside the runner-owned Job Object so explicit End
  can terminate the disposable tree. Management processes must not retain
  kill-on-close ownership of a runner.
- An incompatible or unverifiable runner is reported honestly; Sessions does
  not silently launch a replacement for lost work.

## Updates

The package stages immutable runtime bytes and verifies their manifest before
activation. Existing compatible runners keep the runtime with which they
started. New sessions use the newly staged runtime.

A host update must either:

1. preserve every live session ID and runner process through daemon
   re-adoption; or
2. refuse before changing the active installation.

The viewer uses a current-user installer. A public update additionally requires
Authenticode, the updater signature pinned by the application, exact artifact
hash verification, rollback rehearsal, and the hardware matrix.

A port change or version handoff no longer refuses merely because sessions are
live. The supervisor owns only the daemon and runners are started detached, so
that refusal protected nothing it claimed to. What protects a session is the
same rule macOS uses: capture the exact live baseline first, require every one
of those IDs back before calling the change good, and otherwise restore the
previous runtime on the previous port and say so. The discovery half of the old
gate stays, because a baseline captured while the daemon is still finding
runners is not a baseline.

## Removal

Uninstall removes the per-user integration Sessions wrote outside its own
package — the logon supervisor value and the managed PATH entry — and nothing
else. It stops no process: the daemon is the only thing that can still record
what a live runner produces, so ending it during an uninstall would convert
live sessions into orphans. Deleting the supervisor definition is enough,
because the daemon simply does not return after the next sign-out. It deletes
nothing of the user's either: session records, the ledger, the saved port,
paired-machine credentials, and the staged runtime bytes a live daemon is
executing all survive, and the removal reports what it kept rather than
implying it was thorough.

The read-only evidence collector in
[`scripts/collect-windows-host-evidence.ps1`](../scripts/collect-windows-host-evidence.ps1)
records package identity, signatures, process ownership, and runner preservation
without collecting session content, credentials, or process command lines.

## Source orientation

The platform-neutral runtime is under `runtime/`. Windows-specific process,
ConPTY, Named-Pipe, state, and credential adapters live beside the contracts
they implement. Native packaging and supervisor integration live under
`src-tauri/` and `scripts/`.

Read `runtime/CONTRACT/` before changing runner adoption or client
compatibility. Run the Windows-specific tests only against disposable state and
sessions.
