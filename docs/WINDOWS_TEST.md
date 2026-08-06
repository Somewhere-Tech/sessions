# Windows host test matrix

This is the minimum public evidence for a Windows host release. Use disposable
Sessions state and disposable provider conversations. Never point destructive
tests at irreplaceable work or credentials.

## Build and package

- Build all Go packages and test binaries on `windows-2022`.
- Run Windows-specific process, transport, ConPTY, credential, state, and
  updater tests.
- Build the shared React frontend and native Tauri tests.
- Assemble the current-user installer and portable client from the same source
  revision.
- Verify the installer renders the reviewed Sessions header, sidebar artwork,
  and installer/uninstaller icons without clipping or color conversion defects.
- Record the source revision, package hashes, runtime manifest, and signing
  state.

Unix behavior tests remain required for shared packages, but cross-compilation
is not Windows runtime evidence.

## Clean install

- Install as a standard user without administrator elevation.
- Install over the previous preview and confirm Windows keeps one Sessions
  entry rather than creating a side-by-side duplicate.
- Verify one per-user supervisor definition and one managed Sessions PATH entry.
- Confirm the CLI, daemon, and viewer agree on version and state location.
- Confirm uninstall does not end or delete unrelated work.
- Uninstall now removes Sessions' per-user integration. The NSIS
  pre-uninstall hook runs `Sessions.exe --remove-integration`
  (`src-tauri/windows/installer-hooks.nsh`,
  `src-tauri/tauri.windows.conf.json`), which clears the
  `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` value `Somewhere
  Sessions` and the managed Sessions entry in `HKCU\Environment\Path`
  (`src-tauri/src/windows_runtime.rs`, `src-tauri/src/windows_cli_path.rs`).
  Verify both are gone afterwards, and that a PATH entry Sessions did not
  write is untouched.
- Verify what it deliberately keeps, because thoroughness here would cost work:
  no process is stopped, so the running daemon, its runners, and their provider
  children must all still be alive with their original PIDs after the
  uninstall completes, and everything under `%LOCALAPPDATA%\Sessions` — session
  records, ledger, saved port, paired-machine credentials, and the staged
  runtime bytes a live daemon is executing — must survive. Confirm the removal
  report lists those under `kept on purpose` rather than claiming a clean
  sweep, and that an incomplete removal exits non-zero naming what it left.
- Remove the remaining `%LOCALAPPDATA%\Sessions` state by hand before a
  clean-install test.

## Terminal and providers

Exercise PowerShell, `cmd.exe`, Claude, and Codex:

- Unicode input/output and paste;
- terminal resize and scrollback replay;
- normal provider exit;
- graceful interrupt followed by bounded hard termination;
- sleep, wake, sign-out messaging, and viewer relaunch.

## Lifetime and recovery

- Closing the viewer preserves exact runner and provider PIDs.
- A daemon crash and restart re-adopts compatible runners and restores durable
  output before accepting new input.
- Explicit End terminates the runner-owned disposable process tree.
- Unexpected runner loss creates one durable lost/recovery record and never
  launches a hidden replacement.
- An update either preserves the complete live baseline or refuses before
  changing the active installation.

## Local security

- Named Pipes reject remote and wrong-user access and verify peer identity.
- DPAPI-protected state decrypts for the owning user and fails closed for a
  different user, copied path, or corrupt envelope without silent rotation.
- Secrets do not appear in command lines, logs, links, support output, or
  browser storage.
- Provider credentials remain in provider-owned stores.
- A restrictive parent Job cannot make the viewer or installer the hidden
  lifetime owner of a runner.

## Signed update

- Verify Authenticode publisher and timestamp on every executable and the
  installer.
- Verify the pinned updater signature and exact SHA-256 for the downloaded
  artifact.
- Rehearse interruption and rollback.
- Confirm the update UI reports signed, preview, available, compatible, and
  installed states literally.
- Confirm no update path can be authorized solely by an ordinary paired-device
  credential.

### Read-only live-update comparison

With at least one disposable runner active, use the read-only evidence collector
to record an unelevated baseline before starting the update:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass `
  -File scripts/collect-windows-host-evidence.ps1 `
  -LiveHost `
  -RequireRunner `
  -SourceCommit (git rev-parse HEAD) `
  -OutputPath .\windows-host-before.json
```

After the update completes, run the same read-only collector against the new
runtime and compare it with the baseline:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass `
  -File scripts/collect-windows-host-evidence.ps1 `
  -LiveHost `
  -RequireRunner `
  -SourceCommit (git rev-parse HEAD) `
  -CompareBaseline .\windows-host-before.json `
  -OutputPath .\windows-host-after.json
```

`-CompareBaseline` requires every baseline runner and provider-child PID to
remain present. The collector does not stop processes, modify the installation,
or collect command lines, transcripts, credentials, or session content.

## Release evidence

Save only non-sensitive evidence: source revision, package hashes, signer,
platform version, test results, and before/after process identifiers from
disposable sessions. Do not collect transcripts, prompts, terminal contents,
credentials, private paths, environment variables, or unrelated process
command lines.
