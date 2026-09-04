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
4. **Protect the daily driver.** The only development daemon label is
   `tech.somewhere.sessions.dev.daemon`. Record the live-session baseline before a
   reload and verify `soak-d2` plus the full baseline afterward.
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

## Profiling sessionsd

CPU profiling is disabled by default. Set `SESSIONS_PPROF` to a loopback IP and
port before starting `sessionsd` to expose the standard Go `net/http/pprof`
handlers on a separate listener:

```sh
SESSIONS_PPROF=127.0.0.1:6060 sessionsd
```

Non-loopback, wildcard, and hostname addresses are refused. The profile
listener is deliberately separate from the product API and must not be exposed
through LAN or tailnet routing.

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
