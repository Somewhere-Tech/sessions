# Sessions roadmap

Sessions is a native operations surface for durable Claude Code, Codex, shell,
and command sessions. The Go daemon, CLI, and runners remain independent of the
desktop and mobile clients so updating or closing a view does not end work.

This public roadmap intentionally stays at the theme level. Internal launch
plans, unreleased implementation status, private hosted-service architecture,
commercial planning, and dogfood notes live outside the public repository.

## Available today

- Native macOS application plus standalone macOS and Linux runtime packages.
- Durable PTY, headless command, Claude, and Codex sessions.
- Conversation, terminal, details, search, daily activity, usage, and fleet
  views.
- CLI and JSON control surfaces suitable for people and agents.
- Direct loopback, trusted-LAN, and user-owned tailnet access.
- Signed update channels, explicit lifecycle controls, recovery records, and
  local usage tracking.
- Same-provider continuation and preview cross-provider continuation for
  portable authored conversation history.

See [`docs/NATIVE_APP.md`](docs/NATIVE_APP.md),
[`docs/CONTINUATION.md`](docs/CONTINUATION.md), and
[`runtime/CONTRACT/`](runtime/CONTRACT/) for public contracts.

## Near-term themes

### Find and resume any conversation

Improve title fidelity, provider/workspace/date filtering, conversation
grouping, message-range navigation, and local search ranking. Continuing useful
work should be faster and clearer than a provider's raw history picker.

### First-class Windows host

Bring the full local runtime to Windows with ConPTY, per-user supervision,
named-pipe or Windows local transport, user-scoped credential protection,
Job-Object lifecycle ownership, native signing, and safe updates. Compatibility
is protocol-based; a live runner is never replaced just to make display
versions equal.

### Android paired client

Ship a native mobile client that pairs with an existing Sessions host, adapts
to phones and tablets, stores a revocable device credential securely, and
supports attention notifications without hosting local desktop agents.

### Cross-machine continuation

Move a stopped provider conversation and its minimal safe resume recipe between
user-approved machines. The source remains intact, transfers are verified, and
credentials, arbitrary workspace files, attachments, and raw terminal history
are not silently copied.

### Clearer agent operations

Continue improving manager/child hierarchy, lane-to-lane authorship, attention
states, per-session usage, optional budgets, and compact fleet health. Controls
must remain equally usable from the native interface and CLI.

## Later themes

- Fork a conversation from a chosen point without modifying the source.
- iOS client with platform-appropriate notifications and widgets.
- Explicit session sharing with revocable, scoped access.
- Optional account-backed encrypted backup and hosted services with clearly
  labeled network effects.
- Inline child lifecycle cards and richer local usage insights.

## Explicit non-goals

- Hidden prompt queues.
- A browser-based terminal or agent-control product.
- Making the desktop process own daemon or runner lifetime.
- Requiring a Sessions account for local use.
- A hosted relay that silently creates reachability into a user's local
  machine.

The trust and repository boundary are documented in
[`docs/PRINCIPLES.md`](docs/PRINCIPLES.md) and
[`docs/OPEN_SOURCE_BOUNDARY.md`](docs/OPEN_SOURCE_BOUNDARY.md).
