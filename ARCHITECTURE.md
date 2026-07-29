# Sessions architecture

Sessions is a native client around a local, durable agent-session runtime. The
macOS app is the primary package; the Go daemon, runner, and CLI remain separate
processes inside that package so an app quit or update cannot destroy work.

## Product and process model

```text
Sessions.app / browser / sessions CLI
              |
              | local HTTP + multiplexed WebSocket
              v
           sessionsd  (independent per-user launchd service)
              |
              | one Unix socket per session
              v
runner <id> -> structured provider / PTY / headless command
runner <id> -> structured provider / PTY / headless command
```

| Component | Lifetime | Responsibility |
| --- | --- | --- |
| `Sessions.app` | user interface | Native windows, tray, install/update control, and health reporting |
| `sessions` | one command | API client, orchestration, diagnostics, install, pairing, and remote setup |
| `sessionsd` | background service | HTTP/WS, discovery, session coordination, lane ledger, notifications, and embedded browser UI |
| `sessions-runner` | one per session | Owns provider state or PTY, local socket, replay log, and child process |

The app manages the installed runtime but does not own its lifetime. launchd
owns the daemon, and each runner is supervised separately below it. Quitting or
updating Sessions.app must leave both running. Replacing the daemon must preserve
the live-runner baseline and let the new daemon rediscover every session before
the update reports success.

The v1 Tauri 2 shell already provides tray status, persistent window geometry,
and scoped windows. The v2 release gate bundles the three Go binaries, installs
or upgrades the per-user daemon, rolls back failed upgrades, and adds a signed,
notarized updater. See [`docs/NATIVE_APP.md`](docs/NATIVE_APP.md).

All production Go builds use `CGO_ENABLED=0`. The daemon embeds the React UI,
so the browser and native shells share one frontend and end users do not need
Node, npm, Vite, or a separate static-file server.

## Compatibility boundary

The shipped Go implementation owns current product behavior. The retired
TypeScript implementation in `runtime/testdata/node-runtime/` is retained only
as protocol and mini-cutover evidence until that later migration is complete. The stable
external contracts are documented in [`runtime/CONTRACT/`](runtime/CONTRACT/):

- [`http-api.md`](runtime/CONTRACT/http-api.md)
- [`ws.md`](runtime/CONTRACT/ws.md)
- [`runner-protocol.md`](runtime/CONTRACT/runner-protocol.md)
- [`state-dir.md`](runtime/CONTRACT/state-dir.md)

The frontend continues to work against either daemon while the production mini
remains on Node. New product work belongs in the Go runtime, React frontend, or
Tauri client—not in the TypeScript daemon.

## Session lifecycle

1. A client sends `POST /api/sessions`.
2. Before launch, the daemon records durable creation intent in the lane ledger.
3. The daemon writes a per-session launchd plist on macOS and starts `sessions-runner`.
4. The runner starts the selected session kind, binds `<id>.sock`, and persists
   metadata plus an append-only event log.
5. The daemon connects, receives `HELLO`, requests replay, and exposes the
   session over HTTP and WebSocket.
6. Clients reconnect from their last raw and structured sequence positions.
7. `sessions kill` records an explicit tombstone before terminating only the
   selected session. Unexpected loss remains visible to `sessions recover`.

PTY sessions preserve terminal bytes. Structured Codex and Claude sessions use
their provider event contracts as the authoritative history. Watchers add
structured facts to legacy PTY sessions without making LLM calls.

## Runner and browser protocols

Runner traffic uses length-prefixed binary frames over a Unix socket:

```text
uint32-be frame length | uint8 type | payload
```

Runner-to-daemon frames carry `HELLO`, sequenced output or structured events,
exit state, snapshots, and replay completion. Daemon-to-runner frames carry
input, resize, snapshot/replay requests, and explicit kill. Replay is ordered
before live traffic, and sequence numbers expose gaps.

The frontend normally opens one multiplexed WebSocket at `/ws?mux=1`. It
attaches only the sessions it needs and can request raw replay, structured
replay, and live streams independently. The daemon keeps a bounded in-memory
window while the runner event log provides durable replay after daemon restart.

## State and recovery

The default runtime layout is:

```text
~/.local/state/sessions/
├── token
├── settings.json
├── search-index.db
├── uploads/
└── runners/
    ├── <id>.json
    ├── <id>.sock
    ├── <id>.events
    └── <id>.log

~/Library/LaunchAgents/
├── <configured-sessionsd-label>.plist
└── tech.somewhere.sessions.runner.<id>.plist

~/Library/Application Support/sessions/ledger/lanes.sqlite3
```

`SESSIONS_STATE_DIR` relocates runner and daemon state for interoperability and
tests; isolated work also uses a scratch `HOME`. `SESSIONS_LEDGER_PATH`
separately relocates the append-only lane ledger.

The ledger writes launch intent before process creation and a tombstone before
requested termination. It distinguishes live managed, closed, external, and
unexpectedly lost lanes. `sessions recover` is read-only; `--reopen` is the
explicit mutation.

## Security and trust boundaries

- The default listener is `127.0.0.1:8787`; wildcard binds are refused.
- Protected remote HTTP and WebSocket routes use bearer or paired-device
  credentials.
- Browser origins are restricted independently from API authentication.
- Same-Wi-Fi and Tailscale access are opt-in. Sessions runs no relay.
- Browser push and encrypted transcript backup are opt-in and locally
  configured.
- Sessions launches installed agent CLIs but contains no model client and makes
  no model API requests.
- Install and update code may never perform broad runner cleanup.

## Distribution sequence

1. Ship the signed and notarized macOS app with the bundled Go runtime and safe
   updater.
2. Build the Android paired client; it connects to a Mac daemon and does not run
   the daemon or runners itself.
3. Revisit the production mini only in a later joint maintenance window.

Standalone Go archives and Homebrew remain secondary developer/headless
channels. The retired Node deployment path is non-mutating.

See [installation](docs/INSTALL.md), [release procedure](docs/RELEASE.md), and
the [codebase guide](docs/CODEBASE.md).

## Verification

```sh
cd runtime
GOFLAGS=-buildvcs=false go build ./...
go vet ./...
go test ./...
cd ..
npm --prefix frontend run typecheck
npm --prefix frontend run build
npm run tauri:build
```

Use scratch state for interoperability or install tests. None of these gates
requires touching the production mini.
