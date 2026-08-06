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

## State locations

Two roots, and they are not the same root. The packaged runtime lives under the
bundle identifier, because that is what the viewer's own data directory resolves
to: `%LOCALAPPDATA%\tech.somewhere.sessions\runtime\<version>\`, with the
paired-machine credential vault beside it. Everything the Go runtime owns lives
under the product name: `%LOCALAPPDATA%\Sessions\state\` for session records,
runner logs, the ledger, uploads, and the transcript mirror, and
`%LOCALAPPDATA%\Sessions\config\` for the backup key, hooks, and settings.

Config is a sibling of state rather than a child of it so that resetting state
cannot discard the backup key. The saved port is not a file: it is an argument
inside the logon supervisor definition.

Windows has no `~`, no `.config`, and no `.local/state`, and a Unix path that is
joined rather than expanded produces a directory named literally `~` instead of
failing. Any code reaching for a home-relative path here is a bug with a
distinctive fingerprint, which is why the evidence collector looks for it.

The two roots are narrowed differently, and the asymmetry is deliberate to
record rather than to claim as complete. The runtime root and every versioned
directory under it are given a protected DACL naming only the signed-in user and
LocalSystem, matching the 0700 macOS gives the equivalent root. The Go state root
is created with a Unix mode Windows ignores, so it inherits whatever the profile
grants — usually the user, LocalSystem, and Administrators. The binary that runs
as the user is protected; the ledger and transcripts beside it are as protected
as the profile is.

## Updates

The package stages immutable runtime bytes and verifies their manifest before
activation. Existing compatible runners keep the runtime with which they
started. New sessions use the newly staged runtime.

The daemon is versioned and the runner is not. The logon supervisor definition
carries the runner path the daemon hands to every new session, so naming the
versioned copy there would tie every future session to a directory the next
update replaces, and would point a live runner at a file about to disappear. The
definition names one pinned path directly under the managed runtime root
instead, and the bytes behind it are swapped.

How that swap is performed is the Windows-specific part. Windows refuses to
replace or delete an executable image a process is currently running — which is
exactly the case that matters, because a live session is the thing being
protected. It does allow renaming that file: a rename changes the directory
entry, not the open image, so a live runner keeps executing the same bytes under
a new name and never notices. The swap is therefore rename-aside-then-rename-in,
and the retired copy is deleted later, once nothing is running it. A leftover
retired copy is the mechanism working, not a leak.

Readiness is a budget, not a constant: 30 seconds plus 15 per live session,
capped at 15 minutes. A flat timeout is a bet that recovery is fast, and losing
that bet made a false failure true — the cold-start handler stopped a daemon that
was still re-adopting runners. Waiting for a listener and waiting for a fleet are
separate questions with separate budgets.

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

The PATH entry is identified by derivation, never by pattern. The uninstaller
computes the managed runtime root the same way the installer did and removes only
components equal to it or beneath it, normalizing separators, quoting, trailing
separators, and case first. Every other component is written back byte-identical
and in order, because PATH is a value the user also edits by hand. If the
managed root cannot be derived, removal reports a problem and exits non-zero
rather than matching loosely, on the grounds that an uninstaller editing
components it cannot prove it owns is worse than one that leaves a line behind.

The `kept on purpose` report on Windows names the staged runtime, the saved
port, paired-machine credentials, and every session record. macOS additionally
names the ledger and the running processes; the Windows wording does not, though
the behaviour is the same. Verify surviving processes and the ledger directly.

## Reporting a Windows process

Windows has no `launchd` to supply a second opinion about a runner, and this
contract deliberately does not read process command lines, so the kernel PID
probe is the only corroborating answer available. The portable `Signal(0)` probe
cannot supply it: Go's Windows implementation returns an error for every signal
except kill, so that probe reports "dead" for live and dead PIDs alike. The
probe used instead opens the process and performs a zero-timeout wait on it: a
process object stays unsignalled until the process exits. `GetExitCodeProcess` is
avoided because its `STILL_ACTIVE` sentinel is indistinguishable from a real exit
code of 259.

Every ambiguous outcome resolves to "alive". Refusing to reap a dead runner costs
one stale record the next pass retries; reaping a live one destroys a session.
The same bias protects a conversation: reporting a live provider process dead
would let Sessions append to a transcript that process still owns.

PID reuse is answered separately, because "alive" is not "the same process".
The launcher pairs a PID with the process creation time captured while it still
held the start handle, and declines to terminate anything whose creation time has
since changed. Discovery, which has no such handle, falls back to the image path
behind the PID — every runner is the same image whatever provider it hosts, so
the image path carries the signal that matters without reading a command line.

The read-only evidence collector in
[`scripts/collect-windows-host-evidence.ps1`](../scripts/collect-windows-host-evidence.ps1)
records package identity, signatures, access policy, state locations, process
ownership, and runner and session preservation, without collecting session
content, credentials, state file names, or process command lines. Its comparison
mode requires a preserved process to match on creation time as well as PID, for
the same reason the launcher does.

## Source orientation

The platform-neutral runtime is under `runtime/`. Windows-specific process,
ConPTY, Named-Pipe, state, and credential adapters live beside the contracts
they implement. Native packaging and supervisor integration live under
`src-tauri/` and `scripts/`.

Read `runtime/CONTRACT/` before changing runner adoption or client
compatibility. Run the Windows-specific tests only against disposable state and
sessions.
