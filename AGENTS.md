# Agent guide

This is the entry point for agents working in the public Sessions repository.
Check documentation claims against the implementation; when prose and code
disagree, the code wins.

## What Sessions is

- Sessions is a native product for durable Claude Code, Codex, shell, and
  headless command sessions.
- The native package bundles three Go runtime binaries: `sessions`, `sessionsd`,
  and `sessions-runner` (`runtime/cmd/`).
- A runner owns either a PTY, a headless pipe, or a structured provider
  conversation so daemon and UI restarts do not end the work.
- The daemon exposes the HTTP/WebSocket API used by the CLI and native clients.
  Interactive browser control is deprecated.
- Access is loopback by default and opt-in on a trusted LAN or user-owned
  tailnet.
- Sessions is a joint user/agent product. Operational GUI actions need stable
  CLI and JSON equivalents unless they are inherently visual or require an OS
  permission prompt.

## Repository map

- `runtime/` — Go daemon, CLI, runner, contracts, and internal packages.
- `frontend/` — shared React interface used by the native clients.
- `src-tauri/` — desktop and mobile native shells.
- `runtime/CONTRACT/` — versioned client and runner compatibility promises.
- `docs/` — public architecture, development, security, and user references.
- `skills/sessions/` — distributable Sessions agent skill.
- `site/` — hosted product and onboarding pages.
- `scripts/` and `Formula/` — build, release, and packaging automation.

## Route questions to the right truth

- Product principles: [`docs/PRINCIPLES.md`](docs/PRINCIPLES.md).
- Native package and process-lifetime contract:
  [`docs/NATIVE_APP.md`](docs/NATIVE_APP.md).
- Windows host architecture: [`docs/WINDOWS_HOST.md`](docs/WINDOWS_HOST.md).
- Network and outbound-traffic policy:
  [`docs/NETWORK_SECURITY.md`](docs/NETWORK_SECURITY.md).
- Open-source boundary:
  [`docs/OPEN_SOURCE_BOUNDARY.md`](docs/OPEN_SOURCE_BOUNDARY.md).
- Implementation orientation: [`docs/CODEBASE.md`](docs/CODEBASE.md).
- Process, protocol, and state topology: [`ARCHITECTURE.md`](ARCHITECTURE.md).
- Generated command reference: [`docs/CLI.md`](docs/CLI.md).
- Broad public direction: [`ROADMAP.md`](ROADMAP.md).

Internal orchestration state, dogfood notes, launch plans, commercial plans, and
private hosted-service designs do not belong in this repository.
`scripts/check-public-tree.sh` guards their reserved paths.

## Working rules

1. **Sessions are sacred.** Never kill, replace, mass-clean, or adopt a session
   you do not own.
2. **Isolate development.** Use a worktree and branch. `SESSIONS_STATE_DIR` plus
   `SESSIONS_PORT` is **not** isolation. `SESSIONS_STATE_DIR` moves the runner
   artifact directory, the `token`/`open` sentinels beside it, and the state
   that follows the derived root -- uploads, usage, integration errors
   (`runtime/internal/state/config.go` `stateRootsFromEnv`, and the callers
   that prefer `StateRoot` in `runtime/internal/api/server.go`); the lane ledger
   (`runtime/internal/ledger/store.go` `ResolvePath`), the user state root, and
   `~/Library/LaunchAgents` still resolve from `HOME`. A scratch daemon missing
   the rest reads the user's real provider history, writes lost-runner records
   into their real ledger, and treats their real runner plists as orphans to boot
   out and unlink. A scratch daemon needs all four, under a **short** root:

   ```sh
   HOME=/tmp/sX/home SESSIONS_STATE_DIR=/tmp/sX/runners \
   SESSIONS_LEDGER_PATH=/tmp/sX/lanes.sqlite3 SESSIONS_PORT=8899 sessionsd
   ```

   The runner socket is `<SESSIONS_STATE_DIR>/<uuid>.sock` and macOS `sun_path`
   accepts at most 103 bytes, so a long scratch root fails every session with
   `runner did not create socket within 60s: ...: connect: invalid argument`
   after a full 60-second wait (`runtime/internal/state/launcher.go`). Use the
   same complete isolation for integration and install tests.

   On Windows set `USERPROFILE` instead of `HOME`: `os.UserHomeDir` reads
   `USERPROFILE` there, so `HOME` alone leaves the scratch daemon pointed at the
   real user's state. The application root follows `%LOCALAPPDATA%` only for the
   signed-in user's own home, so a scratch `USERPROFILE` isolates without
   touching `LOCALAPPDATA` (`runtime/internal/state/config_windows_root.go`).
   See `docs/DEV.md`.
3. **Write ahead of destructive action.** Creation and termination intent are
   ledgered before process launch or control
   (`runtime/internal/session/manager.go`, `runtime/internal/ledger/store.go`).
4. **Keep errors instructional.** Explain the failed operation and the safe next
   action; do not hide ambiguous recovery or network failures behind a fallback.
5. **Verify acceptance yourself.** Run the complete gate in the worktree,
   inspect the diff, and report what actually ran.
6. **Keep protocol changes compatible.** Read `runtime/CONTRACT/` before
   changing frames, replay behavior, persisted state, or runner adoption.
7. **Keep the native shell above the service.** Quitting or updating a client
   must never terminate the daemon or a runner.
8. **Preserve agent/user parity.** Put operational authority in the daemon or a
   local native boundary, expose it through the CLI with JSON, and let the GUI
   consume the same contract.
9. **Keep routine states calm.** Finished, resumable, archived, and unavailable
   optional integrations are not security emergencies. Reserve danger
   treatment for meaningful irreversible actions.
10. **Scope instead of guarding.** Before adding a check that refuses an
    action, ask whether Sessions can actually guarantee what the check
    protects. If it can, that is an invariant: enforce it and pin it with a
    test. If it cannot, a refusal claims a guarantee that does not exist, and
    it will grow the surface a false guarantee grows -- an override flag, a
    set of not-sure states, and a slow accumulation of edge cases. Reduce the
    claim instead, or make the consequence survivable and report the fact
    rather than gating on it. Sessions cannot make a provider conversation
    exclusive, so it does not refuse a second one; it keeps an append-only
    copy so a collision is survivable, and says where else the conversation is
    open (`runtime/internal/recovery/adopt.go`,
    `runtime/internal/watch/transcript_mirror.go`). A guard that compensates
    for an inference the code cannot make reliably is a signal that the scope
    is too wide, not that another check is needed.
11. **Keep private planning private.** Public documentation covers shipped
    behavior, public contracts, contributor guidance, security/privacy, and
    broad roadmap themes. Run `scripts/check-public-tree.sh` before delivery.

## Build and test

From `runtime/`:

```sh
export PATH="$PATH:/opt/homebrew/bin"
GOFLAGS=-buildvcs=false go build ./...
GOFLAGS=-buildvcs=false go vet ./...
GOFLAGS=-buildvcs=false go test ./...
```

For the shared frontend:

```sh
cd frontend
npm ci
npm run typecheck
npm run lint
npm run build
npm run test:smoke
```

From the repository root, verify the source structure and public tree:

```sh
npm run check:structure
scripts/check-source-size.sh
scripts/check-public-tree.sh
node scripts/check-doc-links.mjs
```

`check:structure` parses handwritten production functions and the import graph.
Go functions may be at most 221 lines and TypeScript/TSX functions at most 678
lines. The five measured legacy exceptions for each language are named in
`scripts/function-length-exceptions.txt`; their recorded lengths are ceilings,
not exemptions from future growth. `scripts/import-boundaries.txt` is the exact
direct Go product-package graph across Darwin, Linux, and Windows. New or stale
edges require an explicit review of that file. Frontend modules below `src/lib`
and `src/api` may not import from `src/components` or `src/hooks`.

`scripts/check-source-size.sh` remains an informational report of the largest
handwritten files. It does not enforce a file-length cap; function length and
dependency direction are the structural gates.

When the CLI surface changes, run `runtime/scripts/gen-cli-docs.sh` and commit
the generated reference unchanged.
