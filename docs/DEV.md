# Development ground rules

1. **The production mini is hands-off.** Do not edit its checkout, start test
   daemons, run launchctl, rehearse cutover, or deploy to it. Its eventual
   Sessions.app first install is a separate joint operation with the user.
2. **Build from `main`.** Every change uses an isolated worktree and a
   focused branch from the current product branch. The old
   `pty-runner-architecture` branch is historical production state, not a base
   for new work.
3. **Isolate every test daemon.** All four of `HOME`, `SESSIONS_STATE_DIR`,
   `SESSIONS_LEDGER_PATH`, and `SESSIONS_PORT` are required, not optional
   extras. `SESSIONS_STATE_DIR` relocates the runner artifact directory, the
   `token`/`open` sentinels beside it, and the uploads, usage, and
   integration-error state derived from the same root
   (`runtime/internal/state/config.go` `stateRootsFromEnv`) — but not the user
   state root, which keeps settings, machine identity, approved machines, the
   search index, saved profiles, and idle sentinels. The ledger has its own
   override (`runtime/internal/ledger/store.go`), and the user state root,
   provider history, and `~/Library/LaunchAgents` all follow `HOME`. A daemon
   started with only the first two enumerates the daily driver's real provider
   sessions, writes lost-runner records into the real ledger, and offers the
   real runner plists to its discovery sweep for bounded retirement. Keep the
   scratch root short:

   ```sh
   HOME=/tmp/sX/home SESSIONS_STATE_DIR=/tmp/sX/runners \
   SESSIONS_LEDGER_PATH=/tmp/sX/lanes.sqlite3 SESSIONS_PORT=8899 sessionsd
   ```

   The runner socket is `<SESSIONS_STATE_DIR>/<uuid>.sock`, and macOS `sun_path`
   accepts at most 103 bytes. Exceeding it does not fail fast: the daemon retries
   the connect for the full 60 seconds and then reports `runner did not create
   socket within 60s: <path>: ... connect: invalid argument`, which names the
   timeout rather than the length (`runtime/internal/state/launcher.go`). The
   `<uuid>.sock` name and its separator cost 42 bytes, so `SESSIONS_STATE_DIR`
   itself must stay at or below 61 bytes. A scratch directory nested inside a
   worktree can exceed that; `/tmp/sX/runners` cannot.

   A fake provider on the daemon's `PATH` is not enough. Runners do not inherit
   that `PATH`: the daemon prepends `$HOME/.local/bin`, `.npm-global/bin`,
   `.bun/bin`, `.cargo/bin`, Homebrew and `/usr/local/bin`, dropping any already
   present (`runtime/internal/state/registry_runtime.go` `launchdPath`). An
   installed `codex` or `claude` therefore shadows a fake placed only on the
   daemon `PATH`, and the real provider may update itself or contact its
   backend. Put the fake in the scratch `$HOME/.local/bin`, keep that directory
   out of the daemon `PATH` (listing it there moves it behind Homebrew), and
   check the resolution before the first launch rather than by launching:

   ```sh
   HOME=/tmp/sX/home PATH=/tmp/sX/home/.local/bin:/opt/homebrew/bin:/usr/local/bin:$DAEMON_PATH \
     command -v codex   # must print /tmp/sX/home/.local/bin/codex
   cmp /tmp/sX/home/.local/bin/codex ./your-fake-codex   # and it must be your fake
   ```

   Rich Codex runners always resolve `codex` this way; they do not use an
   absolute `--cmd`.
4. **Protect the daily driver.** The manual development daemon label is
   `tech.somewhere.sessions.dev.daemon`. Record the live-session baseline before a
   reload and verify `soak-d2` plus the full baseline afterward. Isolated native
   acceptance fixtures instead generate unique, task-owned scratch labels.
5. **Keep app and daemon lifetimes separate.** Tauri development may open,
   close, rebuild, or replace Sessions.app. It must not terminate a daemon or
   runner as a side effect. Debug builds use the externally managed development
   daemon; release builds reconcile only `tech.somewhere.sessions.daemon`.
6. **Use explicit lifecycle commands.** `sessions kill` is the sanctioned way to
   close a selected session. Recovery and worktree cleanup remain opt-in and
   refuse ambiguous or unsafe operations.
7. **Verify lane output yourself.** Run the complete Go gate, relevant
   frontend/Tauri checks, and focused acceptance tests. Skipped tests are not
   passes.

The repository's active package direction is documented in
[`NATIVE_APP.md`](NATIVE_APP.md), and the public source topology and protocol
boundaries are documented in [`CODEBASE.md`](CODEBASE.md). Historical cutover
notes are intentionally kept out of the public source tree.

## Linux runtime acceptance

CI builds the candidate's three Go binaries on an Ubuntu AMD64 host, then runs
`scripts/linux-runtime-acceptance.mjs RUNTIME_DIR RECEIPT_JSON`. The receipt pins
binary hashes and records isolated shell creation, observed output, runner and
child process identity across a daemon restart, and the loopback/authentication
contract. All four state roots use a short temporary directory. No provider
credentials are required; this does not prove provider login, OS reboot,
systemd installation, or installation of a published npm release. The separate
npm wrapper lifecycle suite uses synthetic executables and local HTTPS fixtures.
The same job then runs `go build`, `go vet`, and `go test ./...` natively on
Linux, each with a scratch `HOME` scoped to that command, so Linux-only code paths execute rather than only
cross-compile. The systemd installer's recovery tests use a substitute service
manager; they do not prove behaviour against a real systemd user manager.

For harness development on another Unix host, `SESSIONS_ACCEPTANCE_FIXTURE=1`
allows running against existing local binaries. Its receipt explicitly records
the actual platform and cannot count as native Linux AMD64 acceptance.

## Development app artifacts

### Signed macOS runtime acceptance

The ignored native test below exercises production installer functions against
two independently verified signed app bundles. Keep the receipt outside the
checkout and use a new filename; the test never overwrites a prior receipt.

```sh
SESSIONS_ACCEPTANCE_OLD_APP=/absolute/path/old/Sessions.app \
SESSIONS_ACCEPTANCE_NEW_APP=/absolute/path/new/Sessions.app \
SESSIONS_ACCEPTANCE_RECEIPT=/absolute/path/evidence/runtime-acceptance.json \
cargo test --manifest-path src-tauri/Cargo.toml --lib \
  lifecycle::signed_acceptance::exact_signed_runtime_install_rollback_and_update \
  -- --ignored --exact --nocapture
```

It uses a short, complete four-root fixture, a unique launchd service, and one
owned shell. It verifies runtime hashes/signatures, first installation, failed
startup rollback, daemon version, and runner/child identity and literal output
across a successful update. No app or provider is launched. Scratch remote
automation is disabled and previewed so it cannot reconfigure shared Tailscale
Serve. Cleanup ends only the owned shell/job and unique daemon before removing
scratch state; failure retains the private fixture and a bounded log receipt.

Identical runtime inputs are explicitly reported as `same_version_dry_run`,
not release-upgrade acceptance. Older daemons that ignore Unix arguments use a
fixture-only smoke exit for that dry run; a real upgrade injects `--version`.
This does not establish fresh-user GUI onboarding, the GUI updater, provider
login, reboot recovery, or phone hardware acceptance.

### CI development artifacts

The CI workflow's optional `build_dev_app` dispatch input builds an Apple
Silicon app after the verification job passes. A separate read-only job checks
out the workflow's exact commit and uploads an ad-hoc signed ZIP, its SHA-256,
and `development-app.json`. It has no release credentials and does not publish
an updater, GitHub release, or npm package. Building does not launch the app.

After downloading, use `scripts/unpack-release-app.py` to extract the ZIP into
a new directory. Verify it against the source SHA, run ID, and run attempt from
GitHub, supplied independently of the downloaded receipt:

```sh
node scripts/development-app-manifest.mjs verify APP ZIP RECEIPT SHA RUN ATTEMPT VERSION
```

For local signing, retain the original receipt and first verify the original
artifact. Sign the three nested runtime binaries, then run `refresh-runtime`
with `APP RECEIPT SHA RUN ATTEMPT VERSION` before signing the outer app. Run
`verify-local` with those same arguments after the outer signature is complete.
The refresh derives new binary hashes while preserving
`vVERSION-dev.gSHA12-bin.FINGERPRINT`. It does not turn the artifact into a
stable release or notarize it. These helpers inspect files without launching
the app, daemon, or runners; running the app still requires the isolation above.

## Profiling sessionsd

CPU profiling is on by default, bound to `127.0.0.1` on a port the operating
system chooses; `sessions doctor` reads the address out of
`GET /api/health/deep` (`pprof.address`). It exposes the standard Go
`net/http/pprof` handlers — the runtime's own stack traces, heap and goroutine
counters — and nothing else: no session content, no conversation, no
credential, and nothing reachable from another machine.

Set `SESSIONS_PPROF` to pin the port, or to `off` to disable it:

```sh
SESSIONS_PPROF=127.0.0.1:6060 sessionsd   # a fixed port
SESSIONS_PPROF=off sessionsd              # no profile listener at all
```

Non-loopback, wildcard, and hostname addresses are refused. The profile
listener is deliberately separate from the product API and must not be exposed
through LAN or tailnet routing.

With profiling on, the daemon also profiles its own bursts. When process CPU
stays above 80% of one core for twenty seconds *after* startup has finished, it
writes one 30-second profile to `<state>/profiles/burst-<UTC timestamp>.pprof`
and logs what dominated it:

```text
[burst] 30.0s profile: top frames — api.(*Server).ServeHTTP 41%, integrations.(*HistoryStore).describe 22%, ledger.applyLaneEvent 9% (saved to /Users/you/.local/state/sessions/profiles/burst-20260912T030058Z.pprof)
```

The newest three profiles are kept under a total cap, at most one capture every
ten minutes. Open one with `go tool pprof <file>`. `GET /api/health/deep` also
carries `routes`: the busiest route shapes of the last five minutes with their
count, total and maximum wall time, so "the app asked for /api/sessions four
hundred times" is a number rather than a theory.

With profiling enabled, the local CLI can capture the daemon without needing a
Go toolchain. It writes the raw profile to the current directory and prints the
ten hottest symbolized frames:

```sh
sessions doctor --cpu-profile 30s
```

The duration must be a whole number of seconds from `1s` through `5m`. The raw
`.pprof` file remains compatible with `go tool pprof` for deeper analysis.

To reproduce large stale-runner discovery safely, generate a new `/tmp` fixture
root. The generator refuses existing roots and every path outside `/tmp`:

```sh
cd runtime
go run ./scripts/stale-state-fixture --root /tmp/sZ --sessions 450 --live 180 --pid 1
```

Then start the daemon with the complete isolation tuple from ground rule 3,
using `/tmp/sZ/home`, `/tmp/sZ/runners`, `/tmp/sZ/lanes.sqlite3`, and a
non-production port. The live fixture records are intentionally marked
runner-lost but not exited, with old metadata, refusing socket files, and launch
agent files. This matches the steady state after discovery loses contact while
preserving the conversations as recoverable records.
