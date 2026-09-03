# Codebase guide

This guide describes the current Go product from its implementation. Paths are
relative to the repository root, and the cited files are the place to re-check
each claim when behavior changes. Protocol compatibility requirements live in
[`runtime/CONTRACT/`](../runtime/CONTRACT/), while product principles live in
[`PRINCIPLES.md`](PRINCIPLES.md).

## Native application

`src-tauri/` is the native shell for macOS, Windows, and the Android and iOS
paired clients. It uses Tauri 2 around the shared React build. Android's
generated Gradle/Kotlin project lives in `src-tauri/gen/android`, and iOS's
generated Xcode project lives in `src-tauri/gen/apple`; mobile builds are
client-only and never bundle or start the Go daemon or runners.
`src-tauri/src/lib.rs` owns native window and
tray behavior on desktop: scoped server/tool/session windows, persisted window geometry,
local status polling, native LAN/remote/pairing commands, configurable daemon
port state, Somewhere CLI version/update discovery, and lifecycle status exposed
to the frontend. The Somewhere command is read-only; its card only copies
explicit install/update/docs commands (`frontend/src/components/SomewhereCard.tsx`).
`scripts/build-app-runtime.sh` builds and signs the three arm64 Go binaries,
while `src-tauri/src/lifecycle.rs` verifies their manifest, stages immutable
runtime versions, installs `tech.somewhere.sessions.daemon`, waits for daemon
health, and verifies that every baseline session is reachable, exited, or has
a recorded user-end boundary. A missing or still-unreachable baseline session
without either end fact rolls the update back; unrelated runner discovery may
continue.
It also maintains a non-destructive `sessions` symlink in the first writable
standard command directory, updating only links that already point into a
Sessions-managed runtime and leaving unrelated executables untouched.
The signed app-bundle updater is configured in `src-tauri/tauri.conf.json` and
exposed through the native-only settings flow in
`frontend/src/lib/tauriBridge.ts`; the bridge serializes update discovery and
delivers once-per-version native notifications. `frontend/src/components/DailyView.tsx`
renders the preloaded local work journal, while `frontend/src/lib/dailyCache.ts`
warms the current day and adopts it
without a blank navigation state. `frontend/src/components/ProductSidebar.tsx`
owns the always-visible signed-update action, while
`frontend/src/components/ConnectionsView.tsx` presents loopback, LAN, Tailscale,
multi-transport machine discovery/request state, the LAN pairing fallback, and safe
port migration. `frontend/src/components/TailnetAccessInbox.tsx` polls the
local daemon even while the window is viewing a remote machine, then renders
the host's explicit Accept/Deny decision for Tailscale or nearby requests.
`frontend/src/lib/hostedBootstrap.ts` deliberately keeps browser pairing
same-origin while routing a pasted native link through the Tauri command in
`src-tauri/src/lib.rs`; the device credential is then stored as a normal
machine entry. The native shell also synchronizes that approved identity into
the CLI registry through standard input, never argv. Unix keeps the device
credential in a private file; Windows applies signed-in-user DPAPI protection.
The one-time-link pairing fallback probes `/api/health` before consuming its
single-use ticket. The claim returns the daemon identity persisted in
`~/.local/state/sessions/machine-id`; `frontend/src/lib/hostedBootstrap.ts`
uses that identity to update an existing machine even when its access endpoint
changes. `frontend/src/components/SearchView.tsx`
fans keyword, exact, regex, or explicitly submitted AI-planned searches across
the configured fleet; persists query, role, provider, date, session-name,
workspace, and ordering state locally; renders best-first message results; and
opens the exact stable message index in a read-only transcript reader. The
reader initially requests only a server-side window, then can deliberately
request everything after the match, user messages only, the full transcript,
or a bookmarked range between two user messages. That surface is labelled
**History** in `ProductSidebar.tsx` and `MobileNav.tsx`, because its no-query
state is not an empty screen: it renders
`frontend/src/components/ConversationBrowser.tsx`, the app's equivalent of
`sessions history` — every recorded Claude and Codex conversation on every
configured machine, whoever started it, newest first, with its last-spoken
time, message count, provider, machine, folder, and whether it began outside
Sessions. Typing narrows the same set into message results. Resume eligibility
mirrors the CLI's precedence exactly — live, moved, unreadable, unavailable,
resumable (`frontend/src/lib/conversationBrowser.ts`) — and a live conversation
offers attach rather than resume, because the daemon refuses a second runtime
for one conversation. Rows order on `conversation_updated_at`, when something
was actually said, rather than on record activity, so a shutdown sweep that
touches a dozen finished runners cannot float them above yesterday's real work.
A machine that did not answer is named, every count becomes a lower bound,
hidden rows are counted out loud, and an empty result says how many
conversations exist behind the filters. Provider badges
reuse the Claude and Codex product icons through
`frontend/src/components/ProviderBadge.tsx`. `scripts/release-app.sh` validates the
version, signing key, notarization credentials, nested signatures, stapling,
Gatekeeper assessment, and renders the static Tauri manifest. Its release
contract lives in [`NATIVE_APP.md`](NATIVE_APP.md).

The desktop workspace begins in `frontend/src/App.tsx`. `ProductSidebar.tsx`
owns the permanent Home/Sessions/Daily/Search/Fleet/Usage/Settings rail;
`HomeView.tsx` summarizes the operational inbox; and `SessionNavigator.tsx`
builds the manager/child tree from normalized `SessionInfo` provenance fields.
The global `NewSessionDialog.tsx` launcher is embedded as the empty state of
the right-hand conversation pane rather than mounted as a modal. Its prompt
composer shares the live conversation composer's visual structure; starting a
session replaces that surface with `SessionView.tsx` in place. Linked-session
creation remains a deliberate overlay because it is scoped to its visible
parent.
`FleetView.tsx` independently polls every configured daemon, uses the optional
`system.os`/`system.arch` health metadata to choose a platform mark, reports
each daemon version, persists a user-defined local alias for each configured
machine, and keeps older daemons compatible with a conservative
client-side fallback. It compares release versions only to render advisory
older/newer/different-build notices; a different version is not itself an API
failure. An advertised `compatibility.api` range is authoritative: native
discovery and `frontend/src/api/sessionsd.ts` reject the endpoint only when the
client protocol is explicitly outside it. Its native `Find machines` panel runs
verified tailnet and Bonjour discovery independently and uses the host-approved
claim commands from Connections, sharing the
durable requester ID through `frontend/src/lib/tailnetClient.ts`. The current
computer is visually primary, an unreachable machine fades as a complete
card. macOS Local Network purpose and
`_sessions._tcp` declarations live in `src-tauri/Info.plist`.
The navigator never derives parentage from cwd or timestamps. Manager pins and
open-tab IDs are bounded local UI preferences; the main list explicitly requests
exited sessions so completed children and ended parents remain visible. Dragging
a row calls `PUT /api/sessions/:id/display-parent`; `state.Metadata` persists
that display-only override and `session.Manager` rejects cycles before
`state.Registry` writes it. Creator kind, parent ID, ancestry, and provenance
status remain separate daemon/ledger truth. `SessionView.tsx` keeps Conversation
as the primary agent surface; terminal-backed agents open the exact provider
terminal as a bounded drawer, while shell sessions and single-session pop-outs
retain the full terminal. Details stays a separate inspector.
The navigator's **All machines** scope uses
`frontend/src/hooks/useFleetSessions.ts` to poll each already-configured daemon
independently and groups live and ended rows by computer. It does not introduce
a relay or share credentials between hosts: selecting a row switches to that
row's authenticated machine before opening the session. Individual machine
chips retain the full hierarchy and management controls.
`SessionDetails.tsx` renders
runtime, workspace, recovery, relationship, usage, and destructive controls;
closing `SessionTabs.tsx` only closes a view. The navigator's row menu keeps
that boundary literal: Close tab leaves the session in Live, Set aside moves it
out of the default working set without stopping it, and ended-session
cross-provider or cross-machine actions route through the existing audited
continuation dialogs. `SessionHistoryView.tsx` is the
explicit exited-session path: it fetches the bounded history preview and never
mounts xterm or a live WebSocket; it initially renders only the latest 60
messages and expands older history on request. Its Resume button immediately
adopts that exact conversation through the audited recovery contract. The
separate global Resume entry point owns the provider-neutral picker:
`runtime/internal/api/resumable.go` merges provider files, prompt-index history,
and Sessions ledger records into one row per Claude/Codex conversation with a
linked continuation chain. `ContinueElsewhereButton.tsx` runs the bundled CLI's
saved-machine dry run and confirmed ended-only transfer through native commands;
credentials and transcript bytes do not enter WebView state. `RemoteView.tsx` renders
timestamped Codex-style user cards and full-width provider answers, while
`InputBar.tsx` owns the single Attach composer action. Terminal quick keys are
scoped to the mobile Terminal pane. Grid and mobile navigation receive only active
sessions, while the full navigator retains lineage history. `CommandPalette.tsx`
provides the global Command-K session/action finder, but only invokes existing
App callbacks and adds no runtime authority. Initial loads retain their workspace
and conversation geometry through `LoadingShell.tsx`; cached real state is never
replaced by a skeleton. `ModelPicker.tsx` is the shared searchable Claude/Codex
model control used by the composer and launcher. `NewSessionDialog.tsx` handles two
different flows: a global recent-workspace launcher and a delegated child
launcher. Its common path is a compact composer footer over the existing Agent,
Machine, and Folder sequence; Advanced keeps accounts, worktree, integrations,
and troubleshooting out of the default path. The latter sends its parent via the trusted HTTP creator header, then
uses wait-ready plus the composer's bracketed-paste/separate-Enter input contract
for an optional initial task. Profiles inherit only while the child keeps the
same provider; switching providers visibly resets to that provider's default login.
A newly created profile never receives task input during its provider login flow.
`SettingsView.tsx` provides native light/dark appearance and smart-feature
preferences with rollback and stale-request protection, profile visibility,
signed update checks/install, Connections, and the existing encrypted
Somewhere backup surface.

The native process is a management plane, not the owner of session work. Its
installer writes and kickstarts the per-user daemon service, but launchd owns
that service afterward and independently supervised runners stay alive through
app quits, daemon reloads, and app upgrades. Android and iOS are paired clients
and do not host the Go runtime.

## Process model

The runtime ships three binaries. `sessionsd` opens the ledger, restores discoverable
runners, starts the API, and periodically rediscovers sessions
(`runtime/cmd/sessionsd/main.go`). `sessions-runner` is the durable per-session process:
it can own an interactive PTY, a pipe-backed headless lane, a Codex app-server
conversation, or a Claude `-p` conversation (`runtime/cmd/sessions-runner/main.go`,
`runtime/cmd/sessions-runner/codex_app.go`, `runtime/cmd/sessions-runner/claude_p.go`). `sessions`
is the human- and agent-facing HTTP client; its command registry and help are a
single table in `runtime/cmd/sessions/help.go`, and dispatch resolves through that
registry in `runtime/cmd/sessions/app.go`.

The runner, not the daemon, owns the work. For PTY sessions it persists framed
events, serves the local runner socket, sends HELLO before any client request,
and replays history atomically (`runtime/cmd/sessions-runner/main.go`,
`runtime/internal/proto/proto.go`). That separation is why a daemon reload can
reattach to a living session instead of restarting it.

## Command binaries

### `cmd/sessionsd`

The daemon validates its bind address, refuses wildcard hosts, opens the
append-only ledger, restores LAN settings for normal installs, and starts discovery before serving
HTTP. An explicitly isolated `SESSIONS_STATE_DIR` scratch daemon does not restore the user's LAN listener
(`runtime/cmd/sessionsd/main.go`). Its assembly point makes the ownership
boundaries visible: session state is delegated to `internal/session`, runner
plumbing to `internal/state`, and transport to `internal/api`.

### `cmd/sessions-runner`

The runner is a durable session host selected by session kind. Ordinary tools
get a PTY, lanes get noninteractive pipes and an exit manifest, and structured
providers get their own event streams (`runtime/cmd/sessions-runner/main.go`,
`runtime/cmd/sessions-runner/codex_app.go`, `runtime/cmd/sessions-runner/claude_p.go`). A PTY
runner ignores terminal hangup/interrupt signals, preserves explicit
termination, and waits for its post-exit client grace period before cleanup
(`runtime/cmd/sessions-runner/main.go`).

### `cmd/sessions`

The CLI talks to the daemon API and centralizes command discovery, aliases, and
usage in `runtime/cmd/sessions/help.go`. Lifecycle commands are split into focused
files such as `runtime/cmd/sessions/commands.go`, `runtime/cmd/sessions/run.go`, and
`runtime/cmd/sessions/recover.go`; `runtime/cmd/sessions/app.go` owns global flags
and dispatch. `runtime/cmd/sessions/machines.go` owns Bonjour discovery,
host-approved connection, private saved credentials, native-app registry sync,
the access inbox, and global saved-machine resolution.
`runtime/cmd/sessions/fleet.go` resolves machine-qualified history references
and constructs the approved fleet used by `search`/`grep`; `history_cat.go`
streams one exact source conversation. `runtime/cmd/sessions/history.go` is
`sessions history`: it browses every recorded Claude and Codex conversation
across that same fleet with the query optional, resolves whether each one is
live, moved, unreadable, gone, or resumable, and prints beside every row the
command that actually works for it — `sessions attach` for a live conversation,
`sessions resume` for one Sessions can reopen including from its own mirror
copy, and no command with the reason for one nothing still holds. `--preview`
reads the stored tail over GET only and creates nothing.
`sessions docs` renders the complete offline Markdown reference,
and [`CLI.md`](CLI.md) is generated by that command, so both track the executable
command table rather than a copied list.

## Internal packages

There are 26 production packages under `runtime/internal/`.

The sections below describe the packages that carry product behavior. Five
supporting packages have no section of their own: `discovery` (Bonjour
advertisement and browsing), `ipc` (the local runner socket, including the
Windows named-pipe owner check), `tokenstore` (master-token and device
credential storage), and the Windows-only `winconpty` and `winprocess`. The
Unix implementation in `tokenstore/store_unix.go` currently has no test file
while its Windows counterpart does; `.github/workflows/windows-preview.yml`
covers the Windows side.

### `api`

`api` serves health, authenticated API/WebSocket routes, LAN controls, the
factual Daily journal, and the
SPA (`runtime/internal/api/server.go`, `runtime/internal/api/ws.go`). Loopback
peers bypass token authentication unless a forwarding header makes the peer
ambiguous; non-loopback clients use the configured bearer or query token unless
the explicit `open` sentinel enables the compatibility escape hatch
(`runtime/internal/api/auth.go`, `runtime/internal/api/server.go`).
QR pairing lives here too: single-use five-minute tickets are claimed by an
unauthenticated, rate-limited `POST /api/pair/claim`, which mints per-device
tokens stored as SHA-256 hashes with list/revoke management
(`runtime/internal/api/pair.go`); device tokens authorize anywhere the master
token does. The native claimant validates the link transport and shape, refuses
redirects, sends the ticket in the POST body rather than the URL, bounds the
response, and never exposes the master token (`src-tauri/src/lib.rs`). Device
tokens remain bearer credentials in this release; narrower scopes, protected
native at-rest storage, and short-lived WebSocket tickets remain required
hardening before adding less-trusted ingress.

The normal Tailscale pairing path is request/accept, implemented in
`runtime/internal/api/tailnet_access.go`. The native Rust layer reads the local
Tailscale peer list, accepts only `.ts.net` HTTPS endpoints, concurrently
health-probes bounded candidates, sends the request, and polls its in-memory
secret. The daemon accepts the two unauthenticated bootstrap requests only when
the immediate peer is loopback and Tailscale Serve supplied a bounded login
identity (`runtime/internal/api/auth.go`). Host list/decision routes require
normal daemon authentication. Public bootstrap routes reject every browser
`Origin` and non-JSON content type. Acceptance does not itself expose a token:
the same Tailscale identity must claim the decision. Claims are idempotent, and
the resulting durable credential remains pending until its first authenticated
API use; if a 201 response is lost, retries return the same token, while a token
the client never receives cannot authorize after its separate two-minute
acknowledgement deadline. Expired pending records are purged when the device
store reloads.
Tailscale's headers identify the user account, not the node, so the approval UI
labels the caller's device name as self-reported. A process already running
locally can fabricate those display headers, but local processes already have
the daemon's loopback control authority.
The same service also implements nearby request/accept with a different
identity boundary. `runtime/internal/discovery/bonjour.go` advertises and
browses `_sessions._tcp`; on macOS
`runtime/internal/discovery/advertise_darwin.go` registers the selected address
through Apple's system Bonjour responder, while other platforms use the
bundled mDNS server. `runtime/internal/api/lan.go` owns the advertisement for
exactly the lifetime of the explicit LAN listener. Nearby bootstrap routes
are accepted only through that listener, reject browser Origins, require a
private IPv4 peer, and bind claim polling to its observed source address.
The approval view states that its device name is self-reported and that LAN
HTTP is unencrypted. Both transports mint the same revocable device records.
The Daily route combines local usage totals with compact factual activity
from both managed lanes and locally observed, still-outside Claude/Codex
conversations. The latter are streamed only from provider logs that contributed
usage in the selected day; child-agent context snapshots are excluded
(`runtime/internal/api/daily_handlers.go`).
Smart-feature settings and natural-language search planning live at
`GET/PUT /api/ai/settings` and `POST /api/search/plan`; the planner receives
only the user's bounded query, while the existing `/api/search` route applies
the generated FTS5 query locally (`runtime/internal/api/search_handlers.go`).
Browser-origin checks are a separate CORS and WebSocket boundary, not a second
authentication factor.

Machine-level onboarding and Claude launch defaults live at
`GET/PUT /api/onboarding` and `GET/PUT /api/claude/settings`, then are resolved
inside the session manager before the runner boundary
(`runtime/internal/api/claude_settings_handlers.go`,
`runtime/internal/session/claude_defaults.go`). Remote Control, permission
mode, model, effort, Chrome, Remote Control naming, and the Somewhere MCP are
typed rather than free-form startup commands. Top-level Claude sessions default
to the native interactive CLI, but Remote Control stays explicitly off until
the user chooses it in first-run onboarding or Settings. A Claude child created
by the authenticated CLI inside another Sessions session defaults to the
provider-native structured runtime; `--pty-claude` explicitly chooses the
interactive terminal when a child needs it. A legacy `on` value and a
per-session override cannot count as consent. Once enabled, Sessions renders
the provider's canonical JSONL in Conversation while Terminal, claude.ai, and
mobile remain views of that same process. The explicit Rich `--print` runtime
remains available for headless automation and disables Remote Control because
it is a separate non-interactive process. Sessions never rewrites Claude's
files.
The Somewhere resolver recognizes the canonical HTTP registration or local
`somewhere mcp` adapter, avoids an equivalent duplicate, and fails on a
same-name/different-target conflict without copying a token into runner state.

The same onboarding boundary records the user's delegated-task access choice.
`runtime/internal/session/delegation.go` resolves it below every UI and CLI
caller. A user-created session is constrained unless full access is explicit.
An agent-created child inherits the parent's exact Claude permission mode or
Codex sandbox and approval flags; it cannot ask the daemon to promote itself.
Explicit autonomous consent permits full-access agent children without changing
already-running sessions. Every new conversation, including an agent-created
child, defaults to `session` lifecycle. A caller can explicitly mark a bounded
`task`, but Sessions does not treat a provider's final response as permission to
end it; lifecycle metadata stays visible to the manager that owns the decision.
Claude children use the structured provider boundary by default so their
manager receives exact events and submission confirmation without parsing a
terminal. Codex children retain the terminal default unless the caller chooses
app-server; constrained Rich Codex sessions route their approval prompts through
Sessions and use Codex's untrusted policy rather than on-request.

### `agentcall`

`agentcall` is the shared one-shot boundary for explicitly requested AI
features (`runtime/internal/agentcall/agentcall.go`). It invokes the user's
already-authenticated Codex or Claude CLI in a temporary directory, disables
tools and persistence, and does not hardcode a model. The child environment is
built from an allowlist rather than by stripping known API-key names: only
variables needed to locate the CLI, let it read credentials the user already
stored on disk, and reach the network at all are passed through, and `PATH` is
rebuilt rather than inherited verbatim. Nothing that selects a model, an
account, or an endpoint is on the list, so a name a vendor ships later —
`ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_USE_BEDROCK`, `ANTHROPIC_BASE_URL`,
`OPENAI_BASE_URL` — is dropped without needing a denylist update. Names are
compared case-insensitively for Windows parity. Codex runs ephemeral/read-only
with user config and rules ignored; its supported isolation features are
preflighted so an older CLI fails with an update/provider instruction rather
than weakening the boundary.
Claude runs in safe mode with Chrome, slash commands, settings sources, tools,
MCP, and persistence disabled.

### `backup`

`backup` implements opt-in transcript uploads using the user's configured
somewhere credentials; its config records the token path rather than copying a
token (`runtime/internal/backup/config.go`, `runtime/internal/backup/push.go`).
Pushes are serialized, and scheduled work only runs when the feature is enabled
(`runtime/internal/backup/service.go`). With `--encrypt`, transcripts and the
manifest are sealed locally with XChaCha20-Poly1305 before upload — the key
stays on the machine (0600, recovery phrase printed once) so the destination
can never read them; enabling encryption busts the incremental cache so prior
plaintext re-uploads encrypted (`runtime/internal/backup/encrypt.go`).

### `claudep`

`claudep` drives structured Claude turns through `claude -p` with stream-JSON,
using a new session ID for the first turn and `--resume` thereafter
(`runtime/internal/claudep/client.go`). It removes `ANTHROPIC_API_KEY` from the
child environment and validates provider events before persisting normalized
history (`runtime/internal/claudep/events.go`,
`runtime/internal/claudep/history.go`).

### `codexapp`

`codexapp` speaks the Codex app-server JSON-RPC contract and persists provider
thread IDs across turns (`runtime/internal/codexapp/client.go`,
`runtime/internal/codexapp/transport.go`). It permits one active turn per
conversation and normalizes app-server events into stored history; model IDs
are checked against the provider catalog rather than guessed
(`runtime/internal/codexapp/history.go`, `runtime/internal/codexapp/models.go`).

### `integrations`

`integrations` is a stable local contract for reading live or persisted provider
history; it does not call a model service (`runtime/internal/integrations/history.go`).
Exact history identifiers can be looked up through the same authenticated
discovery boundary. Claude's bounded prompt index is represented explicitly as
prompt-only history rather than being mistaken for a readable full transcript.
Every normalized message receives a stable transcript index. Plain user and
assistant text remains the primary stream; synthetic scheduled/task
notifications become tool events, and only selected session-control relay
payloads are expanded so delegated prompts remain searchable without admitting
arbitrary command output. Normal transcript and raw contracts remain complete
for deliberate integrations. `TranscriptWindow` returns role/range-selected
stable indices for the native reader; the older tail-bounded
`TranscriptPreview` remains available to compatibility surfaces.
It also keeps append-only integration failures and records lost or nonzero
runner exits (`runtime/internal/integrations/errors.go`), which is where the
torn-record policy for every append-only log in the runtime is stated: a record
that cannot be decoded is skipped, counted, and reported to the caller, and the
file is never rewritten to repair it. History applies that policy per item, so
one unreadable transcript degrades its own row — `unreadable` plus an
instructional `unreadable_reason` — instead of emptying the listing, while a
single-session fetch still fails loudly. The counters are contract fields;
[`docs/INTEGRATIONS.md`](INTEGRATIONS.md) documents what a consumer does with
them.

### `lan`

`lan` chooses the primary private IPv4 address associated with the default
route, excluding loopback, link-local, and Tailscale's carrier-grade NAT range
(`runtime/internal/lan/network.go`). Failure messages guide the user to connect
Wi-Fi or Ethernet, and its routing/address cases are pinned in
`runtime/internal/lan/network_test.go`.

### `ledger`

`ledger` is an append-only SQLite event log for lane identity, provenance, and
recovery; it deliberately excludes prompts and terminal bytes
(`runtime/internal/ledger/types.go`, `runtime/internal/ledger/store.go`). A
fold derives `live-managed`, `closed`, `unexpectedly-lost`, or `external` state
from those events and exposes safe resume recipes (`runtime/internal/ledger/fold.go`).
The store enables WAL and synchronous-full durability and blocks update/delete
with database triggers. Explicit retention uses a separate atomic writer to
append `archived` facts for old closed records; it never deletes the evidence.
`runtime/internal/session/retention.go` refuses live registry entries and any
still-present socket, metadata process, or current/legacy LaunchAgent; apply is
also refused while discovery is running. Finished parents and descendants can
be archived independently because the append-only ledger preserves lineage.
The authenticated API and dry-run-first CLI surfaces
are `runtime/internal/api/retention_handlers.go` and
`runtime/cmd/sessions/gc.go`.

### `migrate`

`migrate` moves a provider conversation and its safe resume recipe, not a live
process or arbitrary worktree contents (`runtime/internal/migrate/types.go`,
`runtime/internal/migrate/source.go`). Receivers validate the recipe and refuse
to overwrite an existing destination (`runtime/internal/migrate/receive.go`);
workspace transfer policy is isolated in `runtime/internal/migrate/workspace.go`.
A moved Claude conversation is written into the bucket Claude itself computes
(`watch.EncodeClaudeCWDStrict`), not the narrow Sessions encoding, because a
destination transcript in any other directory is one native `claude --resume`
cannot see; a bucket that already holds that provider UUID wins over the strict
one, so a repeated move reuses the file rather than leaving one conversation in
two buckets and making lookup by UUID ambiguous.
`runtime/internal/migrate/create.go` independently validates the exact source
identity, minimal recipe, absolute workspace, and Claude/Codex provider before
creating a Rich target runtime. Both daemons append provenance links, while the
source provider file remains in place. The native app uses saved source and
destination machine credentials through the bundled CLI and requires a dry-run
review; the low-level endpoint/token CLI form remains an explicit escape hatch.
The transfer is client-mediated: the client authenticates to each daemon
independently and never sends either machine credential to the other daemon. A
remote source exports the bounded handoff, the destination receives and creates
it, and only then does the client record source completion
(`runtime/internal/api/move_handlers.go`). The transfer does not
carry the full Sessions ledger, tags, isolated profile credentials, attachments,
usage database, or PTY history. The transfer boundary is checksum-verified and
client-mediated. Public behavior is described by `runtime/internal/migrate/`
and the CLI reference; private hosted-service designs are outside this
repository.

### `mirror`

`mirror` is the daemon's concurrent headless terminal emulator. It applies PTY
output into a viewport, serializes terminal state, drains terminal replies, and
reflows scrollback when dimensions change (`runtime/internal/mirror/mirror.go`,
`runtime/internal/mirror/reflow.go`). Defaults and scrollback bounds are owned
by this package rather than by API clients.

### `proto`

`proto` defines the framed runner protocol and the daemon-side socket client
(`runtime/internal/proto/proto.go`, `runtime/internal/proto/client.go`). The
current revision is protocol 2, which added the model request/response frames;
every revision requires server-first HELLO, bounds frame size, and distinguishes
replay from live traffic. The daemon accepts protocol 0 for immutable legacy
runners whose HELLO omitted the field, accepts every revision through the
current one, and rejects an unknown future version before replay or control
frames (`MinimumCompatibleVersion`, `MaximumCompatibleVersion`). HELLO also reports the runner's
runtime release when known; semantic runner capabilities are exposed through
`runtime/internal/proto/runner.go`. Structured provider events use the protocol's
extension frame instead of masquerading as terminal output.

### `recovery`

`recovery` reconciles ledger state with live runners and provider files without
mutating anything while it builds a report (`runtime/internal/recovery/report.go`).
Its per-lane `Reality` reports readability and native resumability as two
separate facts. `conversation` is the provider's own file and
`resumeSourceExists` means only that this file is present, which is what makes
`claude --resume` or `codex resume` possible. `transcriptMirror` is the path to
Sessions' own copy when one exists and holds records, and
`conversationRecoverable` is true when either source can still be read. A
conversation is therefore routinely fully readable through Sessions while
provider-native resume is impossible; the mirror is never folded into
`resumeSourceExists`, because promising a native resume a mirror cannot deliver
would fail in the user's terminal.
Reopen operations only use validated safe recipes and avoid creating duplicate
live ownership (`runtime/internal/recovery/mutate.go`). Adoption requires an
explicit, unambiguous provider artifact (`runtime/internal/recovery/adopt.go`).
Adoption reports, and does not refuse, a conversation another live Claude
process already has open. Claude keeps an undocumented per-process registry
that `AdoptOptions.ClaudeLive` reads strictly read-only to name the holding
window, its pid, and what it is waiting on; it never changes what adoption
does. Refusing there would claim an exclusivity Sessions cannot deliver —
Claude does not lock its transcript and a user can open one a second after any
check — so the consequence is made survivable instead: the transcript mirror is
append-only, so if two writers do collide the Sessions copy holds the union.
This is [`AGENTS.md`](../AGENTS.md) rule 10 in its worked form.
The one prompt-index exception is still exact: an authenticated Claude history
ID must supply a valid provider UUID and an existing recorded absolute
workspace. Sessions launches only `claude --resume <uuid>` there and never
searches for a similar folder or conversation. Claude continuations default to
the native interactive Conversation + Terminal runtime with the destination's
Remote Control setting. Codex continuations default to its Rich app-server
runtime. Explicit runtime choices remain available at the recovery boundary.

`runtime/internal/recovery/continue_provider.go` owns the separate
cross-provider creation boundary. The API normalizes the exact selected
transcript to authored user/assistant messages and places that local bundle in
a mode-0600 runner sidecar (`runtime/internal/state/continuation.go`). Codex
Rich consumes it through app-server `thread/inject_items`; Claude Rich keeps
the source linked and receives an explicit local transcript lookup on its first
turn. Neither path rewrites the source provider store or copies credentials,
tool output, diffs, or attachments. The public behavior and limitations are in
[`docs/CONTINUATION.md`](CONTINUATION.md).

The adjacent `POST /api/recovery/fork` boundary is deliberately
non-lifecycle: it snapshots authored messages only after a live current turn is
idle, optionally truncates at an exact normalized message index guarded by its
stable message ID, creates a fresh provider conversation, and leaves the source
unchanged. Same-provider forks and cross-provider copies share the private
continuation sidecar, while `recovery.ForkConversation` skips the successor
ledger mutation used by Continue and groups the copy beneath its source only
through the display hierarchy. The sidecar carries the selected fork point so
lineage remains explicit after launch.

### Feedback and support

`runtime/cmd/sessions/support.go` owns the local diagnostic schema and official
ticket destinations. It extracts only allowlisted health fields rather than
redacting an arbitrary log after collection. Its JSON envelope also publishes
the stable agent-reporting contract: the machine-readable command, safe fields
to capture, and the requirement for user approval before submission. The native app invokes that
bundled command through `src-tauri/src/lib.rs`, whose support command accepts a
small destination enum instead of an arbitrary URL.
`frontend/src/components/SettingsView.tsx` keeps the user draft in memory,
shows the diagnostic preview, copies the reviewed text, and opens but never
submits the public ticket form. GitHub issue forms under
`.github/ISSUE_TEMPLATE/` repeat the privacy boundary at submission time and
record whether an agent, the user, or both encountered the problem.

### `search`

`search` offers three local retrieval contracts
(`runtime/internal/search/search.go`): ranked token recall by default, explicit
case-insensitive contiguous substring matching, and regular expressions. The
SQLite FTS5 path (`runtime/internal/search/index.go`) uses BM25, stemming,
phrases, and `near(a,b,N)` proximity. Bare terms are conjunctive — every word
has to appear — and the query relaxes in named rungs instead of starting broad:
`strict` requires all terms, `quorum` requires half of them once there are at
least three, and `broad` is the old OR (`rankedPlan` in
`runtime/internal/search/index.go`, `runtime/internal/search/query.go`). Boolean
words in prose are stopwords rather than operators, and raw FTS5 syntax is an
explicit `fts:` prefix (`search.RawSyntaxPrefix`), so a sentence containing
"and" can no longer flip a whole query into a syntax the caller did not ask for.
A pasted path is expanded into its own suffix alternatives instead of being read
as one phrase. Session name and cwd are indexed columns weighted above body
text, with a name-only pass so a session is findable by its exact title.
Results carry a stable message index plus
content-derived bookmark ID, ranking score, match span,
provider/session/workspace/machine/creator metadata, and optional neighboring
messages; full bodies are fetched only after the user opens a hit. Filters
compose role (user/assistant/tool), multiple
session IDs, lane-name glob, workspace, provider, and date bounds; timeline
mode reorders the cross-session result set chronologically. Complete provider
histories are indexed, but an actual source path/size/nanosecond-mtime
fingerprint prevents unrelated runner activity from reparsing a large unchanged
transcript. Refreshes are serialized and cancelable; unavailable transcripts
are removed from the plaintext local index. Search-result history is streamed
into 500-position pages, and the first page verifies the bookmarked message ID
before displaying it.

The response says how it answered. `effective_query` and `match_mode` echo the
expression that actually ran and the rung it ran at, and a `sessions` rollup
counts hits per session over the whole result set rather than the page — the
"which conversation was that" answer — with `rollup_partial` when the scan hit
its bound. Scores are absolute, a saturating relevance blended with recency, so
the same match scores the same at any page size and a caller can threshold on
it; a per-session spread keeps one large conversation from owning the page. The
index version is mixed into every fingerprint, so a schema change like this one
rebuilds the index once on the first search after the upgrade.

With no explicit global `--machine` or `--host`, the CLI fans search across the
local daemon and every approved machine concurrently. Results add a stable
`machine::history-id` reference and collapse matching provider-message copies
while retaining their `available_on` locations. Per-machine status is included
in JSON; reachable results still succeed when another machine is offline.
`sessions grep` accepts familiar `-i` and `-C` spelling over this contract,
`sessions cat` streams the exact normalized transcript from its source,
including `approval_requested` and `approval_resolved` audit records, and
`sessions resurrect` is an accepted spelling of `sessions resume`. No transcript
is materialized into a shared plaintext directory for OS-level grep. The fleet
merge preserves the per-session rollup, machine-qualifies it the same way
matches are, and folds one conversation reachable from two machines into one
row; it withdraws `effective_query`/`match_mode` and marks the response partial
when two machines interpret the query differently, because naming either
interpretation would be a guess about which produced these results. `search`
answers "which message"; a caller who wants "which conversation", or who has
filters but no words, is pointed at `sessions history` rather than refused with
a syntax error.

### `smartsearch`

`smartsearch` translates one explicit, bounded natural-language request into a
compact, recall-oriented SQLite FTS5 query
(`runtime/internal/smartsearch/service.go`). It sends
no transcripts, session IDs, result snippets, or index contents to the selected
CLI; provider and speaker filters remain deterministic API parameters. The
generated query is bounded again and then executed by `internal/search` against
the private local index. Only one planner call may run at once, and identical
provider/query plans are cached for ten minutes. Cache keys are SHA-256 digests
rather than natural-language queries, the map is capped at 128 entries, and
expiry timers remove plans even when no later lookup occurs.

### `session`

`session` coordinates high-level lifecycle, activity, models, hooks, and idle
notifications (`runtime/internal/session/manager.go`,
`runtime/internal/session/idle.go`). Structured lifecycle events are
authoritative when present; PTY output classification is the fallback
(`runtime/internal/session/classifier.go`). A transition to non-working records
an operator-facing reason/detail/time and last useful summary, which powers GUI
health, CLI status/list output, and summary-returning waits. Creation and user-kill intent are
recorded before the corresponding process action. Its daily activity projection
selects sessions and lanes active in a local day, carries hierarchy/tags/outcome,
and uses only final structured assistant summaries for the local journal
(`runtime/internal/session/daily_activity.go`).

`MassKillGuard` refuses more than `DefaultMassKillLimit` (3) runner removals in
one operation without an explicit force
(`runtime/internal/session/manager.go`). By the standard in
[`AGENTS.md`](../AGENTS.md) rule 10 this is a guard compensating for an
inference rather than an invariant: Sessions does not know a runner is dead, it
infers it from discovery, and the guard exists to bound the damage when that
inference is wrong. It is load-bearing today and stays until discovery stops
deleting on inference; it is not a model to copy for new refusals.

### `state`

`state` owns daemon configuration, runner paths, launchd registration, and each
attached session's replay/event state (`runtime/internal/state/config.go`,
`runtime/internal/state/registry.go`, `runtime/internal/state/session.go`).
Runner artifacts have defined suffixes in `runtime/internal/state/paths.go`,
and the in-memory replay plus persisted event log are bounded. Attached state
also exposes runner protocol/release and the additive idle outcome without
changing runner ownership. Additive daemon
settings persist notification, LAN, smart-feature provider choices, and
typed Claude launch defaults
without coupling them to runner state (`runtime/internal/state/settings.go`). This is
low-level runtime state; product lifecycle policy stays in `internal/session`.

### `usage`

`usage` records live structured Claude and Codex token events at the session-manager boundary, then incrementally
indexes the local provider JSONL stores into the same private SQLite ledger without adding a Node runtime dependency.
Stable provider/turn and provider/message keys make backfill enrich rather than double-count live rows
(`runtime/internal/usage/live.go`, `runtime/internal/usage/scanner.go`,
`runtime/internal/usage/store.go`). It retains parser offsets, rebuilds an
index when parsing or pricing semantics change, and reports reasoning as a
subset of output tokens. Forked Codex child rollouts are treated as a context
snapshot followed by new child work: copied parent turns are neither rebilled
nor re-dated, and physical-log provenance cannot be replaced by an equal replay.
Aggregation exposes schema-versioned daily, weekly,
monthly, session, tag, provider, and model views; session tags are joined from
current runner metadata at query time (`runtime/internal/usage/report.go`). The
desktop separates the requested time window from those grouping dimensions and
queries every configured machine directly. Fleet reports can include stable,
content-free event identities so the client deduplicates copied histories
without uploading transcript contents; mixed-version fleets are visibly
reported as a machine sum until every daemon supports exact deduplication.
Pricing is an explicit pinned `ccusage`-compatible table: recorded costs remain
distinguishable from estimates, and unknown models remain visibly unpriced
(`runtime/internal/usage/pricing.go`).

Every calculated cost is an estimate, and the known gaps are worth stating
rather than discovering. The rate table is hand-maintained in source with a
pinned upstream revision but no as-of date, so it lags a vendor price change
until someone edits it. Server-side tool use is billed by the provider but never
appears in the token stream Sessions reads, so it is absent from the total.
Cache writes are priced from one `cache_creation_input_tokens` figure and the
ephemeral 5-minute/1-hour split is not read, so a longer cache TTL is
under-priced. A Codex session's service tier is inferred from launch arguments
and profile configuration rather than reported by the provider. And on a Claude
Max or ChatGPT subscription the marginal cost of a session is zero, so the
figure is a list-price valuation of the tokens spent, not money owed. Treat the
totals as directional; recorded provider costs, where present, are the only
non-estimated numbers.

### `verdict`

`verdict` accepts explicit producer-authored JSON verdicts and never infers them
from prose or terminal output (`runtime/internal/verdict/verdict.go`). It appends
records per session, enforces increasing sequence numbers, and retrieves the
latest record (`runtime/internal/verdict/store.go`); the ledger stores only a
pointer to verdict state. `Emit` writes that ledger pointer *before* the durable
JSONL record, following the write-ahead rule. A consumer should read the
resulting partial state accordingly: an interrupted emit can leave a lane event
saying a verdict was attempted with no record behind it, which is visible and
retryable, rather than a durable record whose caller was told the write failed
and whose retry would append a duplicate verdict.

### `waitcond`

`waitcond` observes commit, file-content, and stable-idle conditions without
changing the target (`runtime/internal/waitcond/waitcond.go`). Filesystem
notifications are only a wake-up hint; polling remains the liveness mechanism,
and file reads are bounded. CLI integration behavior is exercised in
`runtime/internal/waitcond/cli_e2e_test.go`.

### `watch`

`watch` resolves and tails Claude project JSONL and Codex rollout JSONL, then
normalizes provider events for the session layer (`runtime/internal/watch/types.go`,
`runtime/internal/watch/codex_normalize.go`). Claude resolution prefers an
exact conversation UUID, then a sole candidate; when several share a project
bucket it splits them by the cwd each transcript stamped into its own records —
recorded fact rather than a guess — and only one survivor resolves, because
following the wrong conversation is worse than showing none
(`runtime/internal/watch/claude_resolver.go`); Codex resolution uses resume ID,
working directory, and creation time with a broader fallback
(`runtime/internal/watch/codex_resolver.go`). Watchers combine filesystem hints
with polling so missed notifications do not stop progress.

Two Claude project-bucket encodings live here and both matter.
`EncodeClaudeCWD` is the narrow historical one, folding only `/`, `\` and `:`;
it stays as it is because existing files are named with it.
`EncodeClaudeCWDStrict` reproduces what Claude Code actually does — every
non-alphanumeric character folded to a dash, iterated over UTF-16 code units so
an astral character contributes both surrogates, and past 200 characters a
200-character prefix plus a base-36 hash of the original path, so two long
directories sharing a prefix still land in different buckets.
`ClaudeProjectDirsUnder` probes both, narrow first so nothing that resolves
today moves. Strict is a
write path as well as a read path: `internal/migrate` names a moved
conversation with it, because a file written anywhere else is one the provider
will never read. The strict encoding is lossy, so a reader that needs the real
working directory takes it from what the transcripts in that bucket recorded
rather than by inverting the directory name.

`watch` also owns the transcript mirror: Sessions' own durable, append-only copy
of a Claude conversation at `<runner-state-dir>/<id>.transcript.jsonl` with a
`<id>.transcript.meta.json` provenance sidecar
(`runtime/internal/watch/transcript_mirror.go`). A PTY-backed Claude watcher tees
provider lines into it verbatim and in observed order, so the mirror is itself a
legal Claude transcript that every existing reader handles by substituting the
path. Structured Claude and Codex app-server kinds start no watcher, and the
Codex tailer is not mirrored.

Record identity is a multiset, not a set. A record carrying a `uuid` dedupes on
that uuid globally; a record without one is keyed by content hash plus a
per-pass ordinal, so a legitimately repeated line is kept rather than swallowed
as a duplicate. `BeginPass` is the in-memory boundary every reader that restarts
at byte zero calls first, and it is what resets those ordinals — nothing but
verbatim provider lines is ever written into the file. This is load-bearing
rather than pedantic: repeated uuid-less `mode`, `permission-mode`,
`custom-title`, and `agent-name` records are precisely what a native
`claude --resume` replays last-one-wins to restore model, permission mode, and
agent, so collapsing them would not drop decoration, it would rewind restored
state.

The contract has three parts. The provider file always wins whenever it still
resolves, so a session resolves to exactly one transcript path and nothing is
counted twice (`watch.ResolveClaudeWithMirror`, used by `backup.Resolver.Resolve`).
The mirror is never truncated, rotated, or repaired — reaching its 512 MiB cap
stops appends and is recorded in the sidecar rather than discarding recorded
conversation, and it is not unlinked when the session ends. And it becomes the
answer once the provider prunes, renames the bucket it wrote into, or leaves the
resolver unable to choose: `sessions source` and `GET /api/history/<id>/source`
then report `source_kind: "sessions-mirror"` instead of `provider-jsonl`, because
saying `provider-jsonl` would imply a provider-native resume that is no longer
possible.

`sessions transcripts` backfills that copy for conversations nobody is watching —
ended sessions whose provider transcript is still on disk and next in line for
the provider's retention timer (`runtime/cmd/sessions/transcripts.go`). It is a
dry run by default and copies only on an exact provider-id match. The resolver's
single-file fallback is a reasonable guess for *reading* a bucket that holds one
transcript, but writing a guess into a mirror makes it permanent, and once the
provider prunes there is nothing left to correct it against; anything less than
an exact match is reported as unverified and left alone.

### `webassets`

`webassets` provides an optional embedded frontend filesystem. Normal developer
builds expose no embedded assets (`runtime/internal/webassets/assets_dev.go`),
while `embedui` builds embed the built SPA and provide guarded route fallback
(`runtime/internal/webassets/assets.go`,
`runtime/internal/webassets/assets_embedui.go`).

## Source structure

The structural gate is `npm run check:structure` from the repository root. It
measures physical source spans from the Go AST and TypeScript compiler API,
including nested function literals, and reports violations as
`file:line:function:length`. The initial distribution does not support an
80-line limit without turning the exception list into a second baseline: its
sixth-largest functions are 221 lines in Go and 678 lines in TypeScript/TSX.
Those are therefore the enforced limits. The five larger functions in each
language are named with their measured ceilings in
`scripts/function-length-exceptions.txt`; they may shrink but may not grow, and
the checker rejects stale or additional exceptions.

Go package direction is an exact direct-edge declaration in
`scripts/import-boundaries.txt`, generated from `go list -deps` for Darwin,
Linux, and Windows. The checker rejects both an undeclared edge and an allowed
edge no longer present, so dependency changes require a deliberate review of
the graph. Command packages remain assembly points over `internal/*`.
`internal/state` does not point back into `internal/api` or `internal/session`;
`internal/discovery`, `internal/ipc`, and `internal/tokenstore` have no product
dependencies; and `internal/proto` retains only its existing dependency on
`internal/ipc`.

The frontend follows the same inward direction. Files under `frontend/src/lib`
and `frontend/src/api` may depend on shared types and lower-level helpers, but
never on React components or hooks. Shared provider-message types therefore
live in `frontend/src/types/index.ts`, below both the history parser and its
React hook. `scripts/check-source-size.sh` still prints the largest handwritten
production and build files for orientation, but file length is informational;
function responsibility and import direction are enforced.

## Session lifecycle

1. An API create request reaches `session.Manager.Create`, which validates the
   request and records `created` in the ledger before asking the registry to
   launch anything (`runtime/internal/api/server.go`,
   `runtime/internal/session/manager.go`).
2. The registry writes runner metadata and its per-user launchd definition,
   starts or attaches to the runner, performs HELLO/replay, and constructs the
   daemon-side session (`runtime/internal/state/registry.go`,
   `runtime/internal/proto/client.go`, `runtime/internal/state/session.go`).
3. PTY bytes are appended to the runner event file and framed over its Unix
   socket. The daemon updates the mirror and broadcasts WebSocket state; a
   structured runner sends lifecycle/content frames through the same durable
   boundary (`runtime/cmd/sessions-runner/main.go`, `runtime/internal/api/ws.go`).
4. A normal exit is ledgered and its launchd registration is reaped. An
   unexpected socket loss gets bounded reconnect attempts and later discovery;
   an explicit kill is ledgered before the kill request
   (`runtime/internal/session/manager.go`,
   `runtime/internal/state/registry.go`).

Idle classification treats a provider approval or confirmation footer, or a
structured `approval_requested` event, as `needs-input` and preserves its
actionable detail. That state flows through status, list, Fleet, notifications,
JSON, and `sessions wait`, whose envelope reports `reason: needs-input` with or
without `--summary`; no watcher sends Enter on the user's behalf. Lifecycle
metadata records whether a caller intended a bounded task or a durable
conversation, but provider output never authorizes Sessions to end either one.
Only an explicit End records the boundary and closes the runner-owned process
tree.

A server or watcher that must outlive an agent turn should be launched as its
own Sessions command session. A process backgrounded inside a provider terminal
belongs to that terminal and may end when the provider exits; a deliberately
detached process is outside Sessions' lifecycle and cannot be managed honestly.

The binding check in `runtime/internal/session/manager.go` prevents two live
sessions from resuming the same provider conversation. The runner keeps exited
state available briefly for reconnecting clients before removing its transient
socket and metadata (`runtime/cmd/sessions-runner/main.go`).
During the Mini compatibility window, doctor and clean-exit reaping recognize
both the Sessions runner LaunchAgent and the retained legacy runner LaunchAgent;
new sessions always use the Sessions label
(`runtime/cmd/sessions/doctor.go`, `runtime/internal/state/launcher.go`).

## Lane lifecycle

`sessions run` creates a session with lane provenance and a noninteractive child
command (`runtime/cmd/sessions/run.go`). The runner captures its stdout/stderr
through pipes and writes a manifest at exit instead of allocating a PTY
(`runtime/cmd/sessions-runner/main.go`, `runtime/internal/state/paths.go`). The same
write-ahead ledger and ownership checks used by sessions make the lane visible
to `sessions lanes`, `sessions recover`, and explicit adoption
(`runtime/internal/ledger/fold.go`, `runtime/internal/recovery/`).
The manifest's `files_changed` is a repository-root-relative, before/after
Git-visible path delta captured around that lane, including start/end commit
tree changes. Pre-existing dirty paths therefore contribute zero unless their
content or Git state changes while the lane runs, and committed work remains
visible even when the lane exits with a clean worktree
(`runtime/cmd/sessions-runner/main.go`).

Recovery is deliberately two-step: reports are read-only, while reopen/adopt
are explicit mutations. A safe recipe can reopen provider context, but it does
not claim to resurrect process memory or uncommitted worktree bytes
(`runtime/internal/recovery/report.go`, `runtime/internal/migrate/types.go`).

## State on disk

The default Unix state root is `~/.local/state/sessions`; Windows uses
`%LOCALAPPDATA%\Sessions\state`. Both have a `runners/` subdirectory. Sessions'
own configuration root is `~/.config/sessions` on Unix and
`%LOCALAPPDATA%\Sessions\config` on Windows. Derive both from
`state.UserStateRootFor`/`state.UserConfigRootFor` rather than rebuilding either
layout by hand (`runtime/internal/state/config.go`). `SESSIONS_STATE_DIR` relocates runner,
token, open-sentinel, uploads, usage, and integration-error state for a
scratch daemon, while the user state root — settings, machine identity, approved
machines, search index, idle sentinels — stays where `HOME` puts it. The
override is necessary but not sufficient for scratch work: a scratch daemon also
needs `SESSIONS_LEDGER_PATH` and its own `HOME`, or it will still write into the
daily driver's ledger and sweep the daily driver's runner plists (`docs/DEV.md`).

| State | Default location | Source |
| --- | --- | --- |
| Runner socket, metadata, frames, logs, manifests, structured histories | `~/.local/state/sessions/runners/` | `runtime/internal/state/paths.go` |
| Sessions' own copy of a Claude conversation | `~/.local/state/sessions/runners/<session-id>.transcript.jsonl` plus a `.transcript.meta.json` sidecar; append-only, mode 0600, never truncated, rotated, or unlinked when the session ends | `runtime/internal/watch/transcript_mirror.go`, `runtime/internal/state/paths.go` |
| Daemon settings | `~/.local/state/sessions/settings.json` | `runtime/internal/state/config.go` |
| Access token and open sentinel | Unix: `~/.local/state/sessions/{token,open}`; Windows: `%LOCALAPPDATA%\Sessions\state\{token,open}` with the token DPAPI-protected | `runtime/internal/state/config.go`, `runtime/internal/tokenstore/` |
| Approved machine metadata and per-device credentials | `~/.local/state/sessions/clients.json` plus `clients/<machine-id>.token`; private files on Unix and DPAPI-protected credential files on Windows | `runtime/cmd/sessions/machines.go`, `runtime/internal/tokenstore/` |
| Paired-device records | `~/.local/state/sessions/devices.json` | `runtime/internal/api/pair.go` |
| Durable machine identity | `~/.local/state/sessions/machine-id` | `runtime/internal/api/identity.go` |
| Search index | `~/.local/state/sessions/search-index.db` | `runtime/internal/api/search_handlers.go` |
| Integration errors | `~/.local/state/sessions/errors.jsonl`; follows an explicit `SESSIONS_STATE_DIR` | `runtime/internal/integrations/errors.go` |
| Local usage rollup | `~/.local/state/sessions/usage.sqlite3`; follows an explicit `SESSIONS_STATE_DIR` | `runtime/internal/usage/config.go` |
| Browser push keys and subscriptions | `~/.local/state/sessions/{vapid.json,push-subscriptions.json}` | `runtime/internal/session/push.go` |
| Idle completion sentinels | `~/.local/state/sessions/idle/<session-id>` | `runtime/internal/session/idle.go` |
| Saved provider profiles | `~/.local/state/sessions/profiles/<tool>/<name>` | `runtime/internal/session/profiles.go` |
| Fleet-search peer health cache (CLI-local, best effort) | `<user state root>/fleet-search-health.json`, so Windows gets `%LOCALAPPDATA%\Sessions\state`; written only by the CLI, holding the last failure and a five-minute cooldown per approved peer | `runtime/cmd/sessions/fleet.go` |
| Windows supervisor identity | `%LOCALAPPDATA%\Sessions\state\supervisor.json` | `runtime/cmd/sessionsd/supervisor_windows.go` |
| Files uploaded to a session | `~/.local/state/sessions/uploads/<stem>-<8 hex><ext>`; an explicit `SESSIONS_STATE_DIR` keeps them inside that scratch state | `runtime/internal/api/files.go` |
| Lane ledger | `<user state root>/ledger/lanes.sqlite3`; an existing `~/Library/Application Support/sessions/ledger/lanes.sqlite3` is adopted rather than abandoned | `runtime/internal/ledger/store.go` |
| Global idle hook | `<user config root>/hooks.json` | `runtime/internal/state/config.go` |
| Backup configuration and encryption key | `<user config root>/{backup.json,backup.key}` | `runtime/internal/backup/config.go`, `runtime/internal/backup/encrypt.go` |
| Runner LaunchAgents on macOS | `~/Library/LaunchAgents/tech.somewhere.sessions.runner.<id>.plist` | `runtime/internal/state/registry.go` |

The event log is persistent and trims toward its lower bound after crossing its
soft limit; the daemon also keeps a bounded replay window in memory
(`runtime/internal/state/persistent.go`, `runtime/internal/state/eventlog.go`).
Treat these files as protocol state, not as disposable caches, when implementing
compatibility or cleanup.

## Frontend assembly

`runtime/scripts/build-binaries.sh` builds `frontend/`, copies its `dist/`
output into `runtime/internal/webassets/dist/`, and builds `sessionsd` with the
`embedui` tag. At runtime the API first uses an explicitly configured or
checkout frontend directory and otherwise falls back to the embedded filesystem
(`runtime/internal/state/config.go`, `runtime/internal/api/server.go`,
`runtime/internal/webassets/assets_embedui.go`). API and WebSocket routes are
matched before the guarded SPA fallback.
The served SPA is still an implemented compatibility surface, but product
direction now deprecates interactive browser terminal/control. Do not infer a
new browser feature commitment from the current embedded asset path.

## Authentication and network surfaces

The default daemon binds `127.0.0.1`; wildcard bind addresses are rejected in
`runtime/cmd/sessionsd/main.go`. A direct loopback peer can use the local API
without a token, while non-loopback traffic normally authenticates with the
token; forwarding headers disable the loopback shortcut
(`runtime/internal/api/auth.go`). The `open` sentinel is an explicit
compatibility bypass, and static UI/health routing is distinct from
authenticated API routes (`runtime/internal/api/server.go`).

After authentication, `GET /api/machine` returns the daemon's durable machine
ID and the operating system's user-facing computer name. The ID survives a
computer rename, while its DNS-derived legacy display name is upgraded without
changing that identity and clients keep any explicit Fleet nickname as a
separate override. Local UI labels use the real current name followed by
`(this machine)`; they do not use that phrase as the machine's identity
(`runtime/internal/api/server.go`, `frontend/src/lib/servers.ts`).

`sessions lan enable` adds and persists a listener on the selected private
network address and starts its Bonjour advertisement
(`runtime/cmd/sessions/lan.go`, `runtime/internal/lan/network.go`,
`runtime/internal/discovery/bonjour.go`). The LAN listener wraps requests with
an internal transport marker; public nearby bootstrap routes cannot be reached
through the loopback listener. LAN HTTP is authenticated but unencrypted and is
documented for trusted private networks only.
`sessions remote enable` configures Tailscale Serve and verifies the resulting
HTTPS health endpoint (`runtime/cmd/sessions/remote.go`). Allowed browser origins
are checked independently in `runtime/internal/api/auth.go`; that check limits
browser callers but does not replace token authentication.

## Provider watcher model

Structured Codex and Claude runner kinds publish their own lifecycle events, so
the session manager does not start transcript-file watchers for them
(`runtime/internal/session/manager.go`). PTY-backed Claude and Codex sessions
instead resolve provider artifacts using the session working directory,
arguments, creation time, and any explicit resume ID.

Claude lookup maps the real working directory to its project directories under
both encodings, prefers an exact UUID, accepts a sole unambiguous candidate,
splits a shared bucket by the cwd the transcripts themselves recorded, and
otherwise refuses to guess among multiple files
(`runtime/internal/watch/claude_resolver.go`). Codex lookup
first handles a global explicit resume ID, then searches date/cwd/time candidates
and finally performs a bounded broad scan (`runtime/internal/watch/codex_resolver.go`).
Both tailers combine polling with filesystem notification hints, and Codex
events are normalized before they enter the shared session event stream
(`runtime/internal/watch/claude_watcher.go`,
`runtime/internal/watch/codex_watcher.go`,
`runtime/internal/watch/codex_normalize.go`).
