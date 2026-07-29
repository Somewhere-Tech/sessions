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
- Generated command reference: [`docs/CLI.md`](docs/CLI.md).
- Broad public direction: [`ROADMAP.md`](ROADMAP.md).

Internal orchestration state, dogfood notes, launch plans, commercial plans, and
private hosted-service designs do not belong in this repository.
`scripts/check-public-tree.sh` guards their reserved paths.

## Working rules

1. **Sessions are sacred.** Never kill, replace, mass-clean, or adopt a session
   you do not own.
2. **Isolate development.** Use a worktree and branch. For a scratch daemon, set
   both `SESSIONS_STATE_DIR` and `SESSIONS_PORT` so it cannot collide with
   personal state (`docs/DEV.md`).
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
10. **Keep private planning private.** Public documentation covers shipped
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
npm run build
```

When the CLI surface changes, run `runtime/scripts/gen-cli-docs.sh` and commit
the generated reference unchanged.
