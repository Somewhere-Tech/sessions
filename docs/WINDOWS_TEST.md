# Windows host test matrix

This is the minimum public evidence for a Windows host release. Use disposable
Sessions state and disposable provider conversations. Never point destructive
tests at irreplaceable work or credentials.

Everything below has been compiled and cross-checked on Windows in CI. Almost
none of it has been *executed* on Windows hardware. Cross-compilation is not
runtime evidence, and a Windows CI job that runs `go test -run '^$'` is a
compiler check wearing a test's clothes.

## Before you start

Run every step as a normal signed-in user, unelevated. Elevation changes the
answers to the process, ACL, and registry questions this matrix asks, and the
evidence collector refuses to run a live host matrix elevated for that reason.

Two roots matter and they are not the same root:

| What | Where |
| --- | --- |
| Packaged runtime, staged and versioned | `%LOCALAPPDATA%\tech.somewhere.sessions\runtime\` |
| Pinned runner behind the version swap | `…\runtime\sessions-runner.exe` |
| Session records, runner logs, ledger, uploads, transcript mirror | `%LOCALAPPDATA%\Sessions\state\` |
| Backup key, hooks, settings | `%LOCALAPPDATA%\Sessions\config\` |
| Paired-machine credential vault (viewer) | `%LOCALAPPDATA%\tech.somewhere.sessions\credentials\` |
| Logon supervisor and saved port | `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` → `Somewhere Sessions` |
| Managed CLI entry | `HKCU\Environment\Path` |

The runtime root is derived from the bundle identifier, not the product name.
Anything in this repository that says the staged runtime lives under
`%LOCALAPPDATA%\Sessions` is describing the state root by mistake.

## First session at the machine

Ordered by what each step finds per minute spent, not by the shape of the
product. The ordering rule is: a step that invalidates every later step comes
first; among the rest, severity beats breadth, and a cheap probe beats an
elaborate one. Steps 1–5 are roughly an hour and cover the changes most likely
to be wrong, because they are the ones a Unix developer cannot feel.

Destructive steps are last on purpose: step 9 removes the installation.

### 1. Does the host exist at all — 5 minutes

Nothing below this line is interpretable if the runtime did not stage or the
CLI is not on PATH.

```powershell
sessions --version
sessions --json support --diagnostics | ConvertFrom-Json |
  Select-Object -ExpandProperty diagnostics

powershell -NoProfile -ExecutionPolicy Bypass `
  -File scripts/collect-windows-host-evidence.ps1 `
  -LiveHost `
  -SourceCommit (git rev-parse HEAD) `
  -OutputPath .\windows-host-01-baseline.json
```

**Pass:** the CLI answers, the daemon reports reachable and healthy, and the
collector prints `Windows host evidence: PASS`.

**A failure proves:** `sessions.exe is not available on the … PATH` means the
managed PATH entry was never written or the terminal predates it — open a new
terminal before concluding anything. A runtime-manifest digest mismatch means
staging copied bytes that do not match the manifest, which is a packaging fault,
not a host fault. `the persistent user PATH contains N Sessions-managed runtime
entries` with N above 1 means an update added an entry instead of replacing one,
and the CLI you are about to test may not be the runtime the daemon is running.

### 2. A live session survives being looked at — 15 minutes

This is the highest-severity item in the batch. `processAlive` returned false
for every live PID on Windows, so discovery's safety net — the code that removes
a lost runner's artifacts — fired on healthy sessions. Windows users lost live
work. The fix replaces the portable signal probe with `OpenProcess` plus a
zero-timeout `WaitForSingleObject`, in three separate packages.

The existing Windows test only exercises the genuinely-killed path, which the
broken probe got right by accident. The live path is the one to prove.

**2a. The probe itself** (2 minutes, needs the repository and a Go toolchain):

```powershell
cd .\runtime
go test .\internal\recovery -run '^TestProcessAliveAnswersForThisProcess$' -count=1 -v
go test .\internal\session  -run '^TestProcessAlive'  -count=1 -v
go test .\internal\watch    -count=1
go test .\internal\recovery -count=1
```

**Pass:** `TestProcessAliveAnswersForThisProcess` passes. It asks the probe about
the running test process, so it is the one assertion the old bug could not have
survived. The two whole-package runs are the first time `internal/watch` and
`internal/recovery` have executed on Windows at all.

**A failure proves:** the kernel probe is wrong for live PIDs, and every later
step in this document is untrustworthy. Stop and report it.

**2b. Unreachable but alive** (10 minutes, needs Sysinternals `PsSuspend`):

The reaping bug only fired when a runner was *unreachable*. Killing the runner
does not reproduce it; suspending it does, because the process stays alive while
its named pipe stops being serviced.

```powershell
$s = sessions --json new --tool shell --cwd $env:USERPROFILE | ConvertFrom-Json
$id = $s.id
sessions ls
$runner = Get-CimInstance Win32_Process -Filter "Name='sessions-runner.exe'" |
  Select-Object ProcessId, CreationDate
$runner

pssuspend $runner.ProcessId          # runner alive, pipe unserved
Start-Sleep -Seconds 90              # let a discovery sweep run
sessions ls
Get-ChildItem "$env:LOCALAPPDATA\Sessions\state\runners" -Filter "$id.*" |
  Select-Object Name, Length
pssuspend -r $runner.ProcessId
sessions ls
sessions attach $id
```

**Pass:** the session is still listed throughout, its `<id>.json` metadata is
still on disk, and the runner PID and creation time are unchanged after resume.
`sessions attach` reconnects.

**A failure proves:** if the session disappears from `sessions ls` and its
`<id>.json` is gone, `processAlive` is still answering "dead" for a live PID and
discovery reaped a live session — the original bug, unfixed. If the session
survives but `sessions ls` shows it as lost or exited, the probe is right and the
classification above it is wrong; capture `sessions --json recover --all` before
resuming. If a `MassKillError` appears naming `sessions ls -a`, the mass-kill
guard caught it — that is the blast-radius limiter working, and it still means
the probe failed.

**2c. Genuinely dead is still dead** (3 minutes) — the probe must not be biased
so far toward "alive" that a real loss is never reported:

```powershell
$s = sessions --json new --tool shell --cwd $env:USERPROFILE | ConvertFrom-Json
$p = (Get-CimInstance Win32_Process -Filter "Name='sessions-runner.exe'" |
  Sort-Object CreationDate | Select-Object -Last 1).ProcessId
Stop-Process -Id $p -Force
Start-Sleep -Seconds 90
sessions ls -a
sessions recover --all
```

**Pass:** exactly one durable lost/recovery record, with a reason, and no hidden
replacement session appears.

**A failure proves:** if the session still reads as live, the probe's
"every ambiguity is alive" bias has become "always alive" and lost work will
never be reported. If two records appear, the loss path is duplicating.

### 3. State went where Windows keeps state — 10 minutes

The ledger was writing to a literal `~/Library/…` path. Uploads, machine
credentials, the backup key, and hooks all carried Unix assumptions. Backup
returned 503 forever because its home check asserted the shape of a Unix path.
These are cheap to check and they gate everything that has to persist. Keep at
least one disposable session live from step 2, because `-RequireRunner` and
`-RequireRunnerLogs` both need one.

```powershell
powershell -NoProfile -ExecutionPolicy Bypass `
  -File scripts/collect-windows-host-evidence.ps1 `
  -LiveHost -RequireRunner -RequireRunnerLogs `
  -SourceCommit (git rev-parse HEAD) `
  -OutputPath .\windows-host-03-state.json

$e = Get-Content .\windows-host-03-state.json -Raw | ConvertFrom-Json
$e.liveHost.stateLocations   | Format-Table label, present, entryCount
$e.liveHost.unixHomeLeakCheck

sessions backup now
sessions --json backup status
```

**Pass:** the state root, the runner artifacts directory, and the config root all
exist; the collector found no path named literally `~`; and `sessions backup now`
either succeeds or reports that backup is not configured. Either answer means the
subsystem was constructed.

**A failure proves:** the collector failing with `a literal '~' path exists` is
an unexpanded Unix home path being joined rather than resolved — find which
subsystem created it from the parent directory it appeared in. `backup home is
unavailable` with a 503 means the home check is still asserting a Unix path
shape, so the backup subsystem was never constructed at all; note that
`sessions backup status` reads a local file and does *not* reach the daemon, so a
green `status` alongside a 503 from `now` is the exact fingerprint. An absent
`config` root means the backup key and hooks have nowhere to live, and a state
reset would take them with it.

### 4. `sessions doctor` works before you need it — 5 minutes

Deliberately early. Doctor ran a Unix PTY preflight against `/usr/bin/true` and
told Windows users to run `xcode-select --install`. It is the first thing anyone
reaches for when step 2 or 3 goes wrong, so it has to be trustworthy before
those steps need it.

```powershell
sessions doctor
sessions --json doctor | ConvertFrom-Json | Select-Object -ExpandProperty sessions
$LASTEXITCODE
```

**Pass:** a table with one row per live session, `QoS` and `SPAWN` both reading
`n/a`, `STATUS` `ok`, and a closing line `0 of N sessions need recreate — all
healthy (native runner)`. Exit code 0.

**A failure proves:** exit code 2 with `PTY preflight failed` is the original
bug — the Unix preflight is still running. Any mention of Xcode on Windows is the
same bug. `QoS` reading `no-plist` rather than `n/a` means the launchd probe is
running on a host that has no launchd and inventing a fault. A `SPAWN` value
other than `n/a` means something is reading process command lines, which this
platform contract says it does not do.

Known and accepted: because Windows does not read command lines, doctor cannot
detect a runner that is not the shipped `sessions-runner` here. `SPAWN` is `n/a`
by design, not by omission, and doctor is correspondingly weaker on Windows than
on macOS.

### 5. Runner output goes somewhere, and orphans get reaped — 10 minutes

A Windows runner's stdout and stderr went nowhere at all. A runner that died
before publishing its pipe left no diagnostic in the one case where the user most
needs one. Separately, `Reap` was literally `return nil`, so a failed launch left
an orphan holding a ConPTY and a job object, running invisibly.

```powershell
$s = sessions --json new --tool shell --cwd $env:USERPROFILE | ConvertFrom-Json
Get-ChildItem "$env:LOCALAPPDATA\Sessions\state\runners" -Filter "$($s.id).log" |
  Select-Object Name, Length

# A launch that cannot succeed: the command does not exist.
sessions new --tool shell --cmd C:\does\not\exist.exe
Start-Sleep -Seconds 5
Get-CimInstance Win32_Process -Filter "Name='sessions-runner.exe'" |
  Select-Object ProcessId, CreationDate

sessions kill $($s.id)
Start-Sleep -Seconds 5
Get-CimInstance Win32_Process -Filter "Name='sessions-runner.exe'" |
  Select-Object ProcessId, CreationDate
```

**Pass:** `<id>.log` exists for the live session — size zero is normal and fine,
its existence is the evidence. The failed launch is refused with a message
naming the command, and leaves no additional `sessions-runner.exe` behind. The
killed session's runner is gone and no `sessions-runner.exe` from it remains.

**A failure proves:** a missing `<id>.log` means the launcher is not opening the
log before starting the process, and the diagnostic path is still dead. A
`sessions-runner.exe` left behind after a refused launch means `Reap` is not
identifying the process it started — check whether the PID it recorded still
matches the process creation time, because `Reap` deliberately declines to
terminate anything whose creation time has changed. A runner left behind after
`kill` means the reap-on-exit path is not running, and a long session will
accumulate orphans holding job objects.

### 6. The runtime directory is not readable by the neighbours — 5 minutes

macOS narrows the equivalent root to 0700. On Windows a directory created
without an explicit policy inherits whatever the profile grants, and anyone who
can rewrite `sessionsd.exe` owns the daemon.

The collector asserts this, so step 3's run has already covered it; read the
result rather than re-running:

```powershell
$e = Get-Content .\windows-host-03-state.json -Raw | ConvertFrom-Json
$e.liveHost.accessPolicy | ForEach-Object {
  [pscustomobject]@{
    label = $_.label; protected = $_.protectedFromInheritance
    ownerScoped = $_.ownerScoped
    principals = ($_.rules | ForEach-Object { $_.principal }) -join ', '
  }
} | Format-Table -AutoSize

Get-Acl "$env:LOCALAPPDATA\tech.somewhere.sessions\runtime" |
  Select-Object -ExpandProperty Sddl
```

**Pass:** the managed runtime root and the active versioned runtime both report
`protected = True` and `ownerScoped = True`, with exactly two principals —
`LocalSystem` and `signed-in user`. The raw SDDL reads
`D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;S-1-5-21-…)` with no `AI` flag.

**A failure proves:** an `AI` in the SDDL, or `protected = False`, means the
protected DACL was never applied and the directory inherited the profile's
policy. An `Administrators` or `other` principal means someone besides the
signed-in user can rewrite the daemon binary.

Read but do not fail on the two `inherited by design` rows: the Go state root
and config root are created with a Unix mode Windows ignores, so they inherit the
profile policy. That asymmetry — the packaged runtime narrowed, the state holding
the ledger, machine tokens, and transcripts not — is a real finding to record,
not a regression this batch introduced.

### 7. A version handoff keeps its sessions — 20 minutes

A real update from a previously installed version cannot be rehearsed here (see
*What cannot be tested on one machine*). The port change is the same machinery:
capture the live baseline, stop the daemon, restart it, and require every one of
those session IDs back or restore the previous runtime on the previous port. It
is the only way to exercise that path from inside the product.

It also exercises the stable-runner indirection. The logon definition names one
pinned runner path forever and the bytes behind it are swapped by rename,
because Windows will not let anyone replace an image a live runner is executing.

```powershell
powershell -NoProfile -ExecutionPolicy Bypass `
  -File scripts/collect-windows-host-evidence.ps1 `
  -LiveHost -RequireRunner -RequirePinnedRunner `
  -SourceCommit (git rev-parse HEAD) `
  -OutputPath .\windows-host-07-before.json
```

Then, in the viewer: Connections → *This computer* → *Advanced port* → enter a
different port → **Change port**. There is no CLI verb for this.

```powershell
powershell -NoProfile -ExecutionPolicy Bypass `
  -File scripts/collect-windows-host-evidence.ps1 `
  -LiveHost -RequireRunner -RequirePinnedRunner `
  -SourceCommit (git rev-parse HEAD) `
  -CompareBaseline .\windows-host-07-before.json `
  -OutputPath .\windows-host-07-after.json

(Get-Content .\windows-host-07-after.json -Raw | ConvertFrom-Json).liveHost.supervisorRunner
```

**Pass:** the viewer reports `Windows background service moved safely to
localhost:<port>; N live sessions re-adopted`. The collector passes, meaning
every baseline runner and provider child is back with the same PID *and* the same
process creation time, and the daemon still reports every baseline session ID.
`supervisorRunner.shape` is `pinned` and the pinned runner's digest matches the
manifest.

**A failure proves:** `-RequirePinnedRunner was supplied but the logon definition
still names the versioned runner` means the indirection did not activate, and the
next real update will strand every live runner at a path that is about to
disappear. `runner/provider PID preservation failed` names exactly which
processes did not come back. `session preservation failed` with the PIDs intact
is the more interesting failure: the processes survived and the records that make
them reachable did not. A rollback message — `Sessions rejected the Windows
supervisor handoff and restored runtime … on localhost:<old>` — is a *pass* for
the safety property and a fail for the change; record which of the two it was.

Also worth reading afterwards: `supervisorRunner.asideCopies`. A
`.sessions-runner-retired-…` file left behind is the design working, not a leak —
Windows refused to delete an image a live runner was executing, and the next
launch sweeps it. A `.sessions-runner-staged-…` file older than an hour is a
swap that died in the middle.

### 8. The conversation surfaces, which have never run here at all — 25 minutes

The transcript mirror, `sessions history`, and the conversation browser have
zero executed Windows tests. They are late in this list because their failures
are recoverable and visible, not because they are safe. The mirror is on
unconditionally for Claude terminal sessions: there is no config key, env var, or
flag that turns it off.

The Windows-specific hazards are all in one place — how a workspace path becomes
a folder name.

```powershell
# Where the provider keeps conversations, and where Sessions keeps its copy.
Get-ChildItem "$env:USERPROFILE\.claude\projects" | Select-Object Name
Get-ChildItem "$env:LOCALAPPDATA\Sessions\state\runners" -Filter *.transcript.jsonl |
  Select-Object Name, Length, LastWriteTime

sessions history
sessions history --since today --preview -n 3
sessions --json history --since yesterday | ConvertFrom-Json | Select-Object -First 3
sessions transcripts                      # dry run; must not mutate
sessions transcripts --apply
sessions transcripts --apply              # idempotent: everything already kept
```

Case, separators, and drive letters — run all four and compare row counts:

```powershell
$w = "$env:USERPROFILE\work"
sessions history --cwd $w
sessions history --cwd $w.ToLower()
sessions history --cwd ($w -replace '\\','/')
Push-Location $w.ToLower(); sessions history --cwd . ; Pop-Location
```

A workspace path long enough to force the truncate-and-hash bucket name:

```powershell
$deep = "$env:USERPROFILE\" + (("w" * 60 + "\") * 5)
New-Item -ItemType Directory $deep -Force
# start a Claude session there, exchange one message, end it, then:
Get-ChildItem "$env:USERPROFILE\.claude\projects" |
  Select-Object Name, @{n='NameLen';e={$_.Name.Length}},
                      @{n='FullLen';e={$_.FullName.Length}} |
  Sort-Object NameLen -Descending | Select-Object -First 5
Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\FileSystem' -Name LongPathsEnabled
sessions history --cwd $deep
```

Mirror growth across restarts, which is where a line-ending mismatch would show:

```powershell
$m = "$env:LOCALAPPDATA\Sessions\state\runners\<id>.transcript.jsonl"
(Get-Item $m).Length
([IO.File]::ReadAllBytes($m) | Where-Object { $_ -eq 13 }).Count   # carriage returns
# restart the daemon twice with no new conversation, then re-measure:
(Get-Item $m).Length
```

**Pass:** every four case and separator spellings of the same directory return
the same rows. The long workspace resolves and `sessions history --cwd` finds it.
The mirror's byte count does not change across daemon restarts when no new
conversation happened. `sessions transcripts --apply` run twice reports
everything already kept the second time and copies nothing.

**A failure proves:** differing row counts between `C:\Users\…` and
`c:\users\…` means a path comparison is case-sensitive against a case-insensitive
filesystem — the two encodings name the same directory, the same transcript is
counted twice, and the conversation reads as ambiguous or missing while the file
is sitting there. A conversation that reads as unrecoverable while its `.jsonl`
is plainly present is the same fault. A mirror that grows on every daemon restart
with no new conversation means records are being re-appended because their
identity differs between the live tail and the reopen scan, which a CRLF
transcript would cause; left alone it grows toward the 512 MB cap. A
`sessions cat` that cannot open a file `Get-ChildItem` lists is the long-path
case: Go applies the `\\?\` prefix and the provider, in Node, may not — check
`LongPathsEnabled` before blaming Sessions.

Second-window detection, which depends on the same `processAlive` probe as step 2
and on a process-ancestry snapshot:

```powershell
# Terminal A, standard user: run plain `claude` in a workspace and leave it.
Get-ChildItem "$env:USERPROFILE\.claude\sessions" |
  ForEach-Object { Get-Content $_.FullName -Raw | ConvertFrom-Json } |
  Format-Table pid, sessionId, cwd, status
# Terminal B:
sessions --json resume <id> | ConvertFrom-Json | Select-Object alsoOpenIn
```

**Pass:** `alsoOpenIn` names the Terminal A process and its PID.

**A failure proves:** an empty `alsoOpenIn` while Terminal A is plainly alive
means either the liveness probe reported a live process dead — most likely if
Terminal A is elevated and Terminal B is not, because the probe asks for
`SYNCHRONIZE` access that is refused across integrity levels — or the ancestry
snapshot came back empty and ownership could not be proved. Both end with
Sessions writing to a conversation another process owns. Repeat the check with
Terminal A elevated; that split is common on Windows and is the case most likely
to break.

### 9. Uninstall, last, because it ends the run — 15 minutes

Uninstall removes the two things Sessions wrote outside its own package: the
`HKCU\…\Run` value `Somewhere Sessions` and the managed entry in
`HKCU\Environment\Path`. It stops no process and deletes no state.

Record what must survive first:

```powershell
Get-ItemPropertyValue 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run' `
  -Name 'Somewhere Sessions'
Get-ItemPropertyValue 'HKCU:\Environment' -Name Path
Get-CimInstance Win32_Process |
  Where-Object { $_.Name -in 'sessionsd.exe','sessions-runner.exe' } |
  Select-Object Name, ProcessId, CreationDate
Get-ChildItem "$env:LOCALAPPDATA\Sessions\state" -Recurse -File |
  Measure-Object -Property Length -Sum

# Add a PATH entry Sessions did not write, to prove it is left alone.
# (Do this through the Environment Variables UI, not by rewriting the value.)

# And find out where the installer actually put the program:
Get-ItemProperty 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\*' |
  Where-Object DisplayName -like '*Sessions*' |
  Select-Object DisplayName, InstallLocation, UninstallString
```

Then uninstall through Settings → Apps, and re-run every command above.

**Pass:** the `Somewhere Sessions` Run value is gone. The managed PATH entry is
gone and the entry you added by hand is byte-identical. The daemon, its runners,
and their provider children are all still alive with the same PIDs *and the same
creation times*. Everything under `%LOCALAPPDATA%\Sessions` is unchanged. The
removal report lists what it kept under `kept on purpose`, and an incomplete
removal exits non-zero naming what it left.

The `kept on purpose` line on Windows names the staged runtime, the saved port,
paired-machine credentials, and every session record. It does not name the
running processes or the ledger, which macOS does name. That is a wording gap,
not a behavioural one — verify the processes and the ledger directly, as above,
rather than expecting the report to mention them.

**A failure proves:** a surviving Run value means the pre-uninstall hook never
ran and Sessions will start again at the next sign-in — the NSIS safety net
deletes that value unconditionally, so its survival means the uninstaller itself
did not reach the hook. A damaged unrelated PATH entry means the managed-entry
predicate matched something it does not own. A stopped daemon means the uninstall
is ending processes, which converts every live session into an orphan with
nothing left to record its output.

**Check the install location against the state root.** A current-user NSIS
install and the Go state root both live under `%LOCALAPPDATA%`. If
`InstallLocation` turns out to be `%LOCALAPPDATA%\Sessions`, then the
uninstaller's own directory removal sits directly on top of
`%LOCALAPPDATA%\Sessions\state` and `\config`, and the promise that session
records, the ledger, and the backup key survive an uninstall depends on the NSIS
template removing only files it tracked. This has never been observed on
hardware. Confirm the byte count under `%LOCALAPPDATA%\Sessions\state` before and
after and treat any decrease as data loss, not as tidiness.

Remove the remaining `%LOCALAPPDATA%\Sessions` state and
`%LOCALAPPDATA%\tech.somewhere.sessions` runtime by hand before a clean-install
test.

## What cannot be tested on one machine

Say so rather than writing a step nobody can perform.

- **A real update from a previously installed version.** Nothing here has a
  published previous version to update *from*. Step 7 exercises the same
  baseline-capture, stop, restart, and re-adopt machinery through a port change,
  which is the closest available proxy, but it does not stage new bytes, does not
  swap the pinned runner while a runner is executing the old one, and does not
  exercise the updater's download, hash, or signature path.
- **Authenticode and the pinned updater signature.** The preview is deliberately
  unsigned. Every signature assertion in this document — publisher, timestamp,
  artifact hash, `-RequireSigned`, `-ExpectedSignerThumbprint` — needs a real
  signed build and cannot be rehearsed with the preview.
- **Rollback rehearsal of an interrupted update.** Same reason.
- **DPAPI failing closed for a different user.** Proving that protected state
  does not decrypt for another user needs a second Windows account, and proving
  it does not decrypt from a copied path needs a second machine. A single
  signed-in user can only show the positive case.
- **Named Pipe rejection of a wrong-user or remote client.** Same: the negative
  case needs a second principal. The peer-identity code is unit-tested on
  Windows in CI; the cross-user rejection is not.
- **Fleet behaviour across approved machines.** `sessions history` and
  `sessions search` resolve fleet-wide and degrade to partial results when a peer
  does not answer. With one machine there is no peer, so the partial-result path
  and the fleet merge are untestable here.
- **A restrictive parent Job Object making the viewer the hidden lifetime owner
  of a runner.** Constructing that parent Job is a deliberate hostile setup, not
  something an ordinary session produces.
- **Sign-out and sign-in as a lifetime test.** Signing out ends the user's
  processes by design, so it tests the supervisor's restart path, not runner
  survival. Do not read a lost session after a sign-out as a reaping bug.

## Build and package

- Build all Go packages and test binaries on `windows-2022`.
- Run Windows-specific process, transport, ConPTY, credential, state, and
  updater tests. Note what the Windows job does *not* run: outside its explicit
  `-run` list it compiles packages with `-run '^$'`, so `internal/watch`,
  `internal/recovery`, and `internal/migrate` have no executed Windows coverage.
  Step 2a is the first time they run.
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
- Confirm the supervisor definition names the pinned runner directly under the
  managed runtime root, not a versioned copy of it.
- Uninstall behaviour, what it keeps, and the install-location overlap are
  covered by step 9 above.

## Terminal and providers

Exercise PowerShell, `cmd.exe`, Claude, and Codex:

- Unicode input/output and paste;
- terminal resize and scrollback replay;
- normal provider exit;
- graceful interrupt followed by bounded hard termination;
- sleep, wake, sign-out messaging, and viewer relaunch.

Run the interactive surfaces in both Windows Terminal and legacy `conhost`. The
history picker and the conversation browser draw box characters and emoji, and a
legacy console on code page 437 or 1252 without `chcp 65001` will mangle them and
misalign the columns.

## Lifetime and recovery

- Closing the viewer preserves exact runner and provider PIDs.
- A daemon crash and restart re-adopts compatible runners and restores durable
  output before accepting new input.
- Explicit End terminates the runner-owned disposable process tree.
- Unexpected runner loss creates one durable lost/recovery record and never
  launches a hidden replacement.
- An update either preserves the complete live baseline or refuses before
  changing the active installation.
- With a large retained fleet, confirm the readiness budget scales: it is 30
  seconds plus 15 per live session, capped at 15 minutes. A flat budget expiring
  mid-recovery previously caused the failure handler to stop a daemon that was
  still re-adopting, making a false failure true.

## Local security

- Named Pipes reject remote and wrong-user access and verify peer identity.
- DPAPI-protected state decrypts for the owning user and fails closed for a
  different user, copied path, or corrupt envelope without silent rotation.
- Secrets do not appear in command lines, logs, links, support output, or
  browser storage.
- Provider credentials remain in provider-owned stores.
- A restrictive parent Job cannot make the viewer or installer the hidden
  lifetime owner of a runner.
- The managed runtime root and every versioned runtime under it keep a protected
  DACL naming only LocalSystem and the signed-in user.

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
  -RequirePinnedRunner `
  -SourceCommit (git rev-parse HEAD) `
  -CompareBaseline .\windows-host-before.json `
  -OutputPath .\windows-host-after.json
```

`-CompareBaseline` requires every baseline runner and provider-child PID to
remain present with the same process creation time, and requires the daemon to
still report every baseline session identifier. Matching PIDs alone would let a
recycled PID stand in for a runner that actually died, and surviving processes
alone would not prove the sessions they carry are still reachable.

Use `-RequirePinnedRunner` on the run after an update and omit it on the
baseline, so an installation that predates the stable-runner indirection can
still be captured as a starting point.

The collector does not stop processes, modify the installation, or collect
command lines, transcripts, credentials, or session content.

## Release evidence

Save only non-sensitive evidence: source revision, package hashes, signer,
platform version, test results, and before/after process identifiers from
disposable sessions. Do not collect transcripts, prompts, terminal contents,
credentials, environment variables, or unrelated process command lines.

The evidence file does record absolute paths under the signed-in user's profile,
because the runtime, PATH, state-location, and access-policy assertions are about
those exact locations, and it records the current user's SID. It records access
control identities as SIDs rather than account names, and state directories by
shape — which roots exist and how many artifacts of each kind they hold — never
by file name and never by content. Review the file before sharing it outside the
release record.
