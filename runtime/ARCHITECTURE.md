# runtime — the shipped runtime (daemon + runner + CLI)

The port is complete and this is the current product runtime. The native app
bundles these binaries; standalone archives remain available for headless and
developer use. Pure Go keeps cross-compilation predictable, runners small, and
the runtime independent of npm, install scripts, and node-gyp.

## Non-negotiable pins (every lane obeys)
1. **The compatibility contract is stable.** The HTTP/WS API, runner Unix-socket frame protocol, state layout, and runner launchd label scheme stay compatible. `runtime/testdata/node-runtime/src/*.ts` is retained as read-only mini-cutover and rollback evidence, not as the home for new product work.
2. **Interop remains proven:** Go runners are attachable by the TypeScript daemon and vice versa — same `~/.local/state/sessions/` layout (`{id}.sock/.json/.events/.log`), frame protocol, and `tech.somewhere.sessions.runner.<id>` plists.
3. **Pure Go, CGO_ENABLED=0.** creack/pty for PTYs, coder/websocket for WS, modernc.org/sqlite (NOT mattn — no cgo) for the ledger. A cross-compilable static binary is the whole point.
4. **The lane ledger is built-in from day one** (board tsk_64772bd2): write-ahead `created` before launch, tombstone before kill, `sessions recover --reopen`.
5. Dumb pipe: zero LLM calls, observation never interpretation. Sessions are sacred: no code path may mass-remove runners without an explicit forced flag (the mass-kill guard is IN the design here).

## Module layout
module github.com/somewhere-tech/sessions/runtime
  cmd/sessionsd/   — daemon main
  cmd/sessions-runner/    — runner main
  cmd/sessions/    — CLI main
  internal/proto/    — runner frame protocol (shared)
  internal/mirror/   — terminal emulation, snapshot, serialize, reflow
  internal/state/    — state-dir layout, discovery
  internal/ledger/   — lane ledger (sqlite)
  internal/watch/    — claude JSONL + codex rollout watchers/resolver
  internal/api/      — HTTP/WS handlers
