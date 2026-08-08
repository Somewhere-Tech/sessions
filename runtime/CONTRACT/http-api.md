# sessionsd HTTP API contract

This document records the behavior of the normative TypeScript implementation,
principally `runtime/testdata/node-runtime/src/http.ts`. It describes observed compatibility behavior,
including quirks; it is not a redesign.

## Listener and common behavior

- The default listener is `127.0.0.1:8787`. `SESSIONS_HOST` and
  `SESSIONS_PORT` override it. The server refuses `0.0.0.0`, `::`, `::0`, and
  `*` with process exit status 2.
- All JSON replies are compact `JSON.stringify` output with
  `Content-Type: application/json`. Except for static-file replies and the
  plain-text snapshot success response, every reply also sets:
  - `Vary: Origin`
  - `Access-Control-Allow-Methods: GET,POST,PUT,DELETE,OPTIONS` in the Go runtime
    (`GET,POST,DELETE,OPTIONS` in the retained Node compatibility fixture)
  - `Access-Control-Allow-Headers: content-type, authorization,
    x-sessions-creator-session, x-sessions-owner-id, x-sessions-client,
    x-sessions-filename, x-sessions-user-consent`
  - `Access-Control-Allow-Origin: <request Origin>` only when the Origin is
    allowed as described below.
- Every `OPTIONS` request, regardless of path, returns 204 before auth or route
  matching. `send()` supplies `{}`, but Node suppresses the body for 204.
- JSON bodies are limited to 2 MiB. An empty body decodes as `{}`. Invalid JSON
  and an oversized body become the route's documented error response.
- A method/path combination not matched below reaches `404
  {"error":"not found","path":"<pathname>"}` after auth. Thus a wrong method
  on an API path normally requires auth before returning 404.
- An uncaught handler error is converted by `server.ts` to 500
  `{"error":"<message>"}`. That outer error path does not add the normal CORS
  headers.

## Authentication

`GET /api/health`, every `OPTIONS` request, and static GETs are exempt.
`GET /api/health/deep` was previously exempt but now requires authentication:
it enumerates live session identifiers and host process IDs, which must not be
readable by an unauthenticated LAN or tailnet peer. Every other HTTP route
requires either:

- `Authorization: Bearer <token>`, or
- `?token=<token>`.

The token comparison is constant-time after an equal-length check. The token is
64 lowercase hex characters and is created when the daemon starts if no valid
token exists. Unix stores it as a mode-0600 plaintext file at
`~/.local/state/sessions/token`. Windows keeps the same logical `token` path
below `%LOCALAPPDATA%\Sessions\state` but stores a user-scope DPAPI envelope
with a protected signed-in-user + LocalSystem DACL; `sessions` and `sessionsd`
resolve the same path. `SESSIONS_STATE_DIR` relocates both platform forms for
scratch state. The local CLI relies on the same loopback-peer exemption and does
not add the master token to loopback HTTP headers or WebSocket URLs. A present
`open` file beside the token bypasses token auth. Failed auth is
`401 {"error":"unauthorized"}`.

The Go runtime adds two narrowly exempt Tailscale bootstrap routes documented
below. They do not accept caller-supplied identity: the immediate TCP peer must
be loopback and Tailscale Serve must have injected exactly one valid
`Tailscale-User-Login` header. Every approval-management route and every route
used after the bootstrap still requires the normal bearer credential. Local
processes are outside this identity-display boundary: like the normal loopback
API shortcut, a malicious process already running as the user can fabricate
these headers and already has local daemon control.

## Origin and CORS rules

An absent `Origin` is allowed. A present value must parse as a URL and satisfy
one of these rules:

1. its serialized origin is `https://sessions.somewhere.tech` or the platform's
   canonical redirect target `https://sessions.somewhere.site`;
2. its hostname is exactly `127.0.0.1`, `localhost`, or `::1`; or
3. its hostname is exactly the configured bind host.

Scheme and port are unrestricted for the hostname rules. The two hosted values
are serialized-origin matches, so another scheme or port fails. For HTTP, a
disallowed or malformed Origin does **not** reject the request; it merely omits
`Access-Control-Allow-Origin`, leaving browser CORS enforcement to block access.
The WebSocket rule is stricter; see `ws.md`.

## Shared object schemas

### `SessionInfo`

The session create response and each member of a session list have these JSON
fields. Optional fields are omitted when their value is `undefined`.

| Field | JSON type | Meaning |
| --- | --- | --- |
| `id` | string | daemon-generated UUID |
| `name` | string, optional | trimmed caller label |
| `name_source` | `"launch" \| "provider" \| "explicit"`, optional | where the current `name` came from; anything but `explicit` keeps following the provider's conversation title, and absent means the same as `launch` |
| `cmd` | string | launched command |
| `args` | string[] | effective arguments, including daemon-injected tool defaults |
| `cwd` | string | working directory |
| `profile` | string, optional | Claude or Codex login profile name |
| `config_dir` | string, optional | private profile root used for conversation resolution |
| `worktree_path` | string, optional | Sessions-created worktree path recorded in the ledger |
| `branch` | string, optional | Sessions-created branch checked out in `worktree_path` |
| `base` | string, optional | ref the Sessions-created branch started from and must merge into before cleanup |
| `source_repo` | string, optional | source checkout from which Sessions created the worktree |
| `cols` | number | current PTY columns |
| `rows` | number | current PTY rows |
| `createdAt` | number | Unix epoch milliseconds reported by the runner |
| `pid` | number | PTY child PID |
| `runnerProtocol` | number | attached runner wire version; 0 is the legacy missing-version path |
| `runnerVersion` | string, optional | Sessions runtime release reported by the runner |
| `tool` | `"claude-code" \| "codex" \| "terminal"` | classification derived from `cmd` |
| `working` | boolean | current activity classification |
| `lastDataAt` | number | Unix epoch milliseconds of latest PTY output |
| `lastUserMessageAt` | number or null | latest user-role record in the provider transcript. Transcript-derived, so it includes the provider's own internal injections — a scheduled prompt or cron tick is written straight into the transcript and is indistinguishable there from a person. Do not read it as human contact; use `lastHumanMessageAt` for that |
| `lastHumanMessageAt` | number or null | Unix epoch milliseconds of the latest input that reached Sessions **without** source-session attribution: a person at a keyboard, a composer, an attached terminal, `sessions send` run by hand. Stamped at the input boundary, which a provider's internal injection never crosses. Null means no person has spoken into this session |
| `lastAgentMessageAt` | number or null | Unix epoch milliseconds of the latest input that reached Sessions **with** source-session attribution (`X-Sessions-Creator-Session`), meaning one session relayed it to another. Null means no session has |
| `idleReason` | `"never-started" \| "completed" \| "needs-input" \| "failed"`, optional | operator-facing reason the live session is not working |
| `idleDetail` | string, optional | useful prompt or error line from idle classification |
| `idleSince` | number, optional | Unix epoch milliseconds when the current idle outcome began |
| `lastSummary` | string, optional | last useful structured assistant or terminal-tail summary |
| `exited` | boolean | whether an EXIT frame was received |
| `exitCode` | number or null | PTY exit code |
| `exitSignal` | string or null | PTY exit signal as a string |
| `exitedAt` | number or null | Unix epoch milliseconds when EXIT was received |
| `claudeCustomTitle` | string, optional | latest Claude `custom-title` value |
| `claudeAiTitle` | string, optional | latest Claude `ai-title` value |
| `onIdle` | string, optional | trimmed per-session idle hook command |
| `model` | string, optional | model parsed from effective arguments |
| `effort` | string, optional | effort parsed from effective arguments |
| `fast` | boolean, optional | present as `true` for Codex priority service tier; otherwise omitted |
| `setAsideAt` | number, optional | Unix epoch milliseconds when the live session was removed from the native app's default working set; this is organization, not lifecycle |
| `pinned` | boolean, always present | whether the user marked this session as a workbench: it sorts first in every listing and automatic termination leaves it alone. Always present, including as `false`, so a caller can tell "not pinned" from a daemon that predates the field |
| `memoryBytes` | number, optional | resident memory of the session's whole process tree at `resourceSampledAt`. **Omitted, never zero, when unknown** — no live process, a process the daemon may not inspect, or a platform without sampling. A client that treats a missing value as `0` will report a saturated machine as idle |
| `cpuPercent` | number, optional | percent of one core the process tree used between the two most recent samples. It is a rate, not an average over the life of the process; a tree spanning cores exceeds 100. Omitted when unknown, which includes the first sample of a tree, because a rate needs two readings. Omitted is not zero; a measured zero is sent as `0` |
| `resourceProcesses` | number, optional | how many processes `memoryBytes` and `cpuPercent` cover. Present exactly when `memoryBytes` is |
| `resourceSampledAt` | number, optional | Unix epoch milliseconds when the three fields above were measured. Sampling is periodic, so a reader must treat the figures as of this time rather than as of the response |
| `delegation_kind` | `"user" \| "agent"`, optional | presentation provenance for a child session: explicitly started by the user or created by its parent agent |
| `permissions` | `"constrained" \| "full"`, optional | daemon-resolved access class for this runtime; provider-specific approval and sandbox arguments remain visible in `args` |
| `lifecycle` | `"task" \| "session"`, optional | `task` workers retire after a successful final response; `session` conversations remain live until explicitly ended |

Exited sessions remain in the daemon map for 30 seconds. They are omitted from
the default list but can be requested with `include_exited=1` during that grace
period.

### Standard error bodies

Error strings originating from Node, the filesystem, JSON parsing, launchd, or
session creation are passed through as strings. Consumers must not depend on
such platform-dependent text. The literal error bodies listed per route are
stable source literals.

## Routes

### `GET /api/health`

No auth. Returns 200:

```json
{
  "ok": true,
  "name": "sessionsd",
  "version": "0.2.3",
  "listen": { "host": "127.0.0.1", "port": 8787 },
  "lan": {
    "enabled": true,
    "url": "http://192.168.1.24:8787",
    "bonjour": { "advertised": true, "service": "_sessions._tcp" }
  },
  "access": { "open": false },
  "system": { "os": "darwin", "arch": "arm64" },
  "compatibility": {
    "api": { "current": 1, "minimumClient": 1, "maximumClient": 1 },
    "runner": { "current": 2, "minimum": 0, "maximum": 2 }
  },
  "discovering": false,
  "sessionsLoaded": 0
}
```

`host`, `port`, `system`, `discovering`, and the count vary. `listen` always
reports the main loopback listener's configured host and port, not the LAN
listener's. `access.open` reports whether the `open` sentinel is present beside
the token, so a client can tell an intentionally unauthenticated daemon from a
misconfigured one.

`lan.enabled` is `false` and `lan.url` is `null` whenever the user-enabled LAN
listener is not running. When it is running, `lan.url` is **redacted to `null`
for callers that have not proved they belong here**: the field survives only for
a direct loopback peer or a caller whose `Authorization` header or `?token=`
query parameter authorizes successfully
(`runtime/internal/api/server.go` `mayReadLANEndpoint`). The route itself stays
unauthenticated because native discovery, `sessions machines discover`, the
updater, and the frontend's origin bootstrap all depend on it and on the
200-vs-401 distinction; the selected private IPv4 address is withheld separately
because it maps the user's network to anyone who can reach the port.
`lan.enabled` and `lan.bonjour` are never redacted, so a probe can still tell
whether the listener is up.

`system.os` uses Go's stable platform names (`darwin`, `windows`, `linux`, and
so on) so native clients can choose a machine icon without guessing from a
hostname. `compatibility.api` is the authoritative client acceptance range;
`compatibility.runner` describes the living runners this daemon can adopt.
Clients preserve their legacy behavior when an older daemon omits the additive
object, but must stop before normal use when their protocol is outside an
advertised range. The count includes exited sessions still in their 30-second
grace period. The deep-health response carries the same `compatibility` and
`access` objects but no `listen` or `lan`.

### `GET /api/health/deep`

Requires authentication (loopback peers are already authorized). Returns 200:

```json
{
  "ok": true,
  "name": "sessionsd",
  "version": "0.2.3",
  "discovering": false,
  "sessionsLoaded": 1,
  "uptimeSec": 12,
  "sessions": [
    {
      "id": "<id>",
      "tool": "terminal",
      "cols": 300,
      "rows": 50,
      "pid": 12345,
      "working": false,
      "exited": false,
      "claudeEvents": 0,
      "lastDataAgeMs": 42
    }
  ]
}
```

`uptimeSec` is rounded `process.uptime()`. `claudeEvents` is the absolute count
including events evicted from the in-memory front. `lastDataAgeMs` is computed
at request time.

### `GET /api/push/vapid`

Auth required. Returns `200 {"publicKey":"<base64url string>"}`. It lazily
loads or generates VAPID keys. Failure returns `500 {"error":"<message>"}`.

### `POST /api/push/subscribe`

Auth required. JSON body:

```json
{
  "endpoint": "https://push.example/subscription",
  "expirationTime": null,
  "keys": { "p256dh": "<non-empty string>", "auth": "<non-empty string>" }
}
```

`expirationTime` may be omitted, null, or a number. All other shown fields are
required and non-empty strings. A subscription with the same endpoint replaces
the old record. Success is `200 {"ok":true}`. Invalid input, invalid JSON, or an
oversized body is `400 {"error":"<message>"}`; invalid shape specifically uses
`"invalid push subscription"`.

### `POST /api/push/unsubscribe`

Auth required. Body is `{"endpoint":"<non-empty string>"}`. It removes every
stored record with that endpoint; absence is still success. Responses:

- `200 {"ok":true}`
- `400 {"error":"endpoint is required"}` for missing, empty, or non-string
  endpoint
- `400 {"error":"<message>"}` for JSON/body errors

### `GET /api/sessions`

Auth required. Query `include_exited=1` is the only value that includes exited
sessions. Other values and duplicates do not. Returns 200:

```json
{"sessions":[/* SessionInfo objects */]}
```

The order is the daemon map's insertion order; the route does not sort.

### `POST /api/sessions`

Auth required. Every request field is optional:

| Field | JSON type | Source behavior/default |
| --- | --- | --- |
| `cmd` | string | `$SHELL`, else `/bin/bash` |
| `args` | string[] | `[]`; the daemon resolves provider-specific constrained or full-access arguments |
| `cwd` | string | `$HOME`, else OS home; must exist and be a directory |
| `cols` | number | 300 |
| `rows` | number | 50 |
| `env` | object of string values | caller environment after filtering reserved/injection keys |
| `name` | string | trimmed; empty becomes absent |
| `profile` | string | optional `[a-z0-9-]{1,32}` Claude/Codex login profile; rejected for shell sessions |
| `worktree` | boolean | when true, create an isolated Git worktree and use it as `cwd` |
| `base` | string | optional worktree base ref; requires `worktree`; defaults to the source checkout's current branch |
| `onIdle` | string | trimmed; empty becomes absent |
| `waitReady` | boolean | only literal `true` waits for readiness, capped at 30 seconds |
| `delegationKind` | `"user" \| "agent"` | optional child presentation provenance; requires a validated `X-Sessions-Creator-Session` parent |
| `permissions` | `"inherit" \| "constrained" \| "full"` | optional requested access; `inherit` requires a parent, and a child cannot exceed its parent unless the user explicitly enabled autonomous delegated work |
| `lifecycle` | `"task" \| "session"` | optional runtime lifetime; agent-created children default to `task`, while user-created sessions default to `session` |

`RUNNER_*`, `NODE_OPTIONS`, `DYLD_INSERT_LIBRARIES`, `DYLD_LIBRARY_PATH`, and
`LD_PRELOAD` caller keys are stripped. User-created Claude/Codex sessions are
constrained unless full access is explicitly requested. An agent-created child
inherits the parent's exact Claude permission mode or Codex sandbox and
approval flags. The daemon rejects self-escalation. A machine-level autonomous
delegation choice can make new agent-created children full-access; only the
explicit user-facing onboarding/Settings route can grant that choice. Success
is 201 with a bare `SessionInfo` object, not an envelope. Any caught failure is
`400 {"error":"<message>"}`. Creating a session invokes the platform runner
supervisor; there is no unmanaged create path in the normative implementation.

When a `task` worker produces a successful final response and becomes idle,
the daemon records the normal durable end boundary and closes its runtime. Its
transcript, parent/child lineage, and workspace stay available. A worker in
`needs-input` or `failed` state is never auto-ended and Sessions never accepts
its prompt by blindly sending Enter.

Profile directories are created mode `0700` below
`<UserStateRoot>/profiles/<tool>/<name>`. A profiled Claude launch receives
`CLAUDE_CONFIG_DIR`; a profiled Codex launch receives `CODEX_HOME`. Unprofiled
launches receive neither variable. The daemon records `profile` and
`config_dir` in runner metadata and the created ledger payload, then uses the
same root for watcher, transcript, search, backup, and recovery resolution
([`internal/session/profiles.go`](../internal/session/profiles.go),
[`internal/state/registry.go`](../internal/state/registry.go),
[`internal/backup/sessions.go`](../internal/backup/sessions.go)).

### `GET /api/profiles`

Auth required. Returns profile directories by tool and name, including their
path, currently active sessions, and last-used Unix milliseconds:

```json
{"profiles":[{"tool":"claude","name":"work","path":"/Users/me/.local/state/sessions/profiles/claude/work","sessions":[],"last_used":1784491200000}]}
```

Sessions exposes no profile deletion route because these directories contain
provider credentials. Listing is implemented by
[`internal/api/profiles_handlers.go`](../internal/api/profiles_handlers.go) and
[`internal/session/profiles.go`](../internal/session/profiles.go).

The optional worktree request and response fields are a backward-compatible Go
extension implemented by [`internal/state/types.go`](../internal/state/types.go)
and [`internal/session/worktrees.go`](../internal/session/worktrees.go).

### `GET /api/providers`

Auth required. Returns local Claude Code and Codex installation status:

```json
{"providers":[{"id":"claude","name":"Claude Code","installed":true,"version":"1.0.0","latestVersion":"1.0.1","lastCheckedAt":"2026-07-25T12:00:00Z","updateAvailable":true}]}
```

Version and last-check fields are omitted when the provider or its local update
metadata is unavailable. Status inspection is read-only and is allowed for
authenticated local and paired clients.

### `POST /api/providers/:id/update`

Auth required and restricted to a loopback client on the daemon machine.
Authenticated paired devices, the master token, and open remote access receive
403 before executable lookup or mutation. The provider ID must be `claude` or
`codex`; the daemon then runs that installed provider's own `update` command
with a five-minute deadline. Success returns the refreshed provider object and
bounded installer output. Unknown, absent, failed, or timed-out providers return
4xx/5xx without changing Sessions itself.

### `GET /api/worktrees`

Auth required. Returns worktrees created by Sessions according to ledger
provenance, never arbitrary Git worktrees. Each result includes `session`,
`session_name`, `worktree_path`, `branch`, `base`, `source_repo`, `tree_state`,
`dirty`, `merged_into_base`, `session_state`, `exists`, and an optional
`inspection_error`:

```json
{"worktrees":[]}
```

The route is implemented in
[`internal/api/worktrees_handlers.go`](../internal/api/worktrees_handlers.go);
Git inspection and ledger filtering live in
[`internal/session/worktrees.go`](../internal/session/worktrees.go).

### `POST /api/worktrees/clean`

Auth required. Body is `{"dry_run":true|false}`. Cleanup considers only
Sessions-created worktrees and removes one only when its session is durably
exited, its tree is clean, and its branch is fully merged into its recorded
base. It uses non-forced Git removal and branch deletion; all ineligible or
refused operations return `action:"skipped"` with a `reason`. Dry-run returns
`action:"would-remove"` without changing the repository. Success is:

```json
{"results":[],"dry_run":true}
```

There is no force option, and session kill does not call this route
([`internal/session/worktrees.go`](../internal/session/worktrees.go),
[`internal/session/manager.go`](../internal/session/manager.go)).

### `POST /api/retention/gc`

Auth required. Body is
`{"older_than_ms":2592000000,"dry_run":true|false}`. The age must be between
one hour and ten years. This is list-retention, not transcript deletion: it
considers only durably closed ledger records and appends an `archived` fact
instead of deleting lifecycle events or runner artifacts. A live runner is
always skipped. An ancestor is also skipped while any descendant remains
retained, so archiving cannot break the visible lineage graph.

The default CLI flow is a dry run (`sessions gc`); mutation requires
`sessions gc --apply`. Each result item reports `archived`, `would_archive`, or
`skipped` plus an instructional reason. Success is:

```json
{"dry_run":true,"cutoff_ms":1782250000000,"items":[]}
```

The route is implemented by
[`internal/api/retention_handlers.go`](../internal/api/retention_handlers.go);
candidate selection and the atomic append-only batch live in
[`internal/session/retention.go`](../internal/session/retention.go) and
[`internal/ledger/store.go`](../internal/ledger/store.go).

### `POST /api/retention/archive`

Auth required. Body is `{"ids":["<session id>"]}`. It explicitly archives only
selected, durably closed records while preserving lifecycle facts, transcripts,
and lineage. Live, unknown, already archived, or ancestry-constrained records
are reported as skipped. The successful response uses the same retention result
shape as GC.

### `DELETE /api/sessions/:id`

Auth required. An optional JSON body carries
`{"reason":"<operator text>","operationId":"<correlation id>"}`; an empty body
remains valid for older clients. `?force=1` bypasses the normal graceful end
request. The daemon captures the authenticated initiator plus the optional
session/external-owner attribution headers before it sends the runner KILL
frame, and leaves removal to the runner EXIT path. Responses:

- known map entry: `200 {"ok":true}`
- unknown entry: `404 {"ok":false}`

### `POST /api/sessions/end-batch`

Auth required. Body is:

```json
{"ids":["<session id>","<session id>"],"reason":"<operator text>","operationId":"<correlation id>","force":false}
```

At least two non-empty live session IDs are required. The request is rejected
before mutation if a target is missing or the manager's mass-end safety guard
requires explicit `force:true`. On success the daemon records one attributed
operation for the batch and returns `{"ok":true,"ids":[...]}`.

### `PUT /api/sessions/:id/display-parent`

Auth required. Body is `{"parentSessionId":"<session id>"}`. The empty string
promotes the session to a visual root. A non-empty value must identify another
retained session, and the resulting display hierarchy must remain acyclic.
Success is:

```json
{"displayParentSessionId":"<session id or empty string>"}
```

This route changes only user organization. It persists
`display_parent_session_id` in the runner metadata and updates the live
`SessionInfo`; it never changes `parent_session_id`, creator kind/ID, ancestry,
or the append-only creator ledger. Unknown source or parent sessions return
404. Self-parenting, descendant cycles, malformed JSON, and an already cyclic
display graph return 400.

### `PUT /api/sessions/:id/name`

Auth required. Body is `{"name":"<title>"}`. The title is trimmed, must be
non-empty, contain no control characters, and be at most 120 Unicode
characters. Success returns `{"name":"<title>"}` and updates both runner
metadata and the append-only `renamed` fact used by retained history. It also
records `name_source: "explicit"`, which stops the daemon from following the
provider's conversation title for that session.

Body `{"auto":true}` reverses that and takes no name of its own: it records
`name_source: "launch"`, adopts the provider's current conversation title
immediately when there is one, and otherwise keeps the present name until the
next title arrives. Success returns the resulting `{"name":"<title>"}`. A
runtime without it answers
`501 {"error":"automatic session naming is not available on this runtime"}`.

This is the canonical title across the Sessions app, CLI, Fleet, search, and
later continuations. Until a session is renamed it follows the provider's own
title automatically, so a Claude conversation is named whatever Claude calls it
and keeps following later title changes; that is what makes a session findable
in Sessions under the name every Claude surface shows. Codex writes no title
into its rollout files, so a Codex session keeps the name it was created with.
Sessions never rewrites Claude or Codex private conversation files to imitate
an unsupported provider rename API.

### `PUT /api/sessions/:id/model`

Auth required. Body is `{"model":"<exact model>","effort":"<level>"}`. Omitting
`effort` preserves the session's current effort. This changes the defaults used
by the next turn of an idle Rich Claude or Rich Codex session; it does not
rewrite provider history or interrupt a turn already in progress.

Codex choices are checked against the live app-server model catalog, including
supported effort and the session's existing service tier. Claude accepts a
bounded model name and the provider effort values `low`, `medium`, `high`,
`xhigh`, `max`, or empty. Success returns the updated bare `SessionInfo`.
Terminal sessions, ended sessions, working sessions, old runners, unavailable
models, and invalid efforts fail explicitly without changing the recorded
model. Agents use the same contract through `sessions model`.

### `GET /api/models/codex`

Auth required. Returns `{"models":[...]}` from the live Codex app-server model
catalog without creating a session. The New Session launcher uses this route
to offer an exact dropdown before runtime creation. A catalog failure is
reported explicitly; Sessions does not replace it with a stale hardcoded list.

### `GET /api/sessions/:id/model-options`

Auth required. For a Rich Codex session, returns `{"models":[...]}` from the
live app-server catalog used by the model validator. Each entry includes its
exact ID, display name, default flag, and supported reasoning efforts. The
native composer loads this catalog only when its model picker opens. Claude
uses the provider's stable model aliases locally. Other session kinds return
400 instead of inventing choices.

### `PUT /api/sessions/:id/set-aside`

Auth required. Body is `{"setAside":true|false}`. A true value persists the
current Unix epoch milliseconds as `set_aside_at` in runner metadata; false
clears it. Success returns the resulting value:

```json
{"setAsideAt":1785024000000}
```

Clearing returns `{"setAsideAt":null}`. The operation does not stop, archive,
detach, or otherwise alter the runner. Unknown sessions return 404. Ended
records return 409 with guidance to use archive instead. A successful
`POST /api/sessions/:id/input` also clears the field so user input brings a live
session back into the working set.

### `PUT /api/sessions/:id/pin`

Auth required. Body is `{"pinned":true|false}`. The value is persisted as
`pinned` in runner metadata and reported back as the state actually stored:

```json
{"pinned":true}
```

A pinned session sorts first in every listing and is exempt from automatic
termination — the task-lifecycle sweep that retires a finished delegate, and
the sleep and retention policies that follow it. It is never exempt from an
explicit end by the user. The operation starts, stops, and otherwise touches
nothing about the runner.

The pin is daemon-owned end to end: a runner has no way to express it, so a
runner metadata write preserves whatever the daemon last stored. Unknown
sessions return 404. Ended records return 409, because a pin exempts a live
session from automatic termination and cannot protect one that already ended;
archive is the verb for those. Any method other than `PUT` returns 405.

### `GET /api/sessions/:id/snapshot`

Auth required. Optional `cols=N` is converted with `Number`, truncated through a
32-bit integer operation, and clamped to at least zero. A positive value asks
the daemon for ANSI-aware reflow; non-positive/invalid values select the
canonical snapshot.

Success is 200 with `Content-Type: text/plain; charset=utf-8`, the serialized
xterm buffer as the body, and `X-Sessions-Seq: <decimal sequence>`. If an allowed
Origin was present it also sets that ACAO value and
`Access-Control-Expose-Headers: X-Sessions-Seq`. The success path does not set
`Vary` or the common allow-method/header fields. Unknown session is
`404 {"error":"unknown session","id":"<id>"}`.

### `GET /api/sessions/:id/events`

Auth required. The event values are passthrough structured Claude records (and
normalized Codex records) represented as arbitrary JSON objects. Returns 200:

```json
{
  "events": [],
  "nextIndex": 0,
  "totalCount": 0,
  "startIndex": 0,
  "endIndex": 0
}
```

All indices are absolute. Let `base` be the number evicted from the front and
`len` the retained count; `total = base + len`.

- `before=n`, when finite and non-negative, caps the exclusive end to `n`.
- `since=n`, when finite and non-negative, moves the start to `n`, bounded by
  the selected end.
- `tail=n`, when finite and positive, moves the start to at most the last
  `floor(n)` entries before the selected end. It composes with `since` by taking
  the later start.
- Invalid, negative, and (for `tail`) zero values are ignored.
- `nextIndex` and `totalCount` are always `total`, even for a window ending
  before the current end. `startIndex`/`endIndex` describe the returned window.

Unknown session is `404 {"error":"unknown session","id":"<id>"}`.

### `POST /api/sessions/:id/input`

Auth required. Body is `{"data":"<raw UTF-8 terminal bytes>"}`. A missing or
null `data` becomes the empty string. Responses:

- live known session: `200 {"ok":true}`
- unknown or exited session: `404 {"ok":false}`
- JSON/body/runtime error: `400 {"error":"<message>"}`

Successful input clears `setAsideAt` best-effort after the runner accepts the
bytes. Failure to persist that organizational change is logged but does not
turn accepted input into an error.

### `POST /api/sessions/:id/submit`

Auth required. Body is `{"data":"<one complete composer message>"}`. The
daemon serializes logical submissions across callers, writes `data`, waits for
the provider TUI to settle, and writes carriage return as a second PTY frame.
This is the message boundary used by the CLI and desktop composer: concurrent
agents cannot interleave one message's text with another message's Enter.
Terminal keys and paste-without-submit continue to use `/input`.

Success is `200 {"ok":true}`. Unknown/exited targets return `404`. If text was
accepted but Enter could not be delivered, the error includes
`{"delivered":true,"retry":false}` so an automated caller does not duplicate
the prompt.

### `POST /api/sessions/:id/upload`

Auth required. The request body is raw bytes, not JSON. Optional header
`X-Sessions-Filename` defaults to `file`; `Content-Type` is accepted but not used
in the saved name or response. The filename is reduced to its basename,
characters outside `[A-Za-z0-9_. -]` become `_`, and the result is limited to
96 characters. The stored name is `<stem>-<first 8 chars of random UUID><ext>`.

The destination is the `uploads/` directory under the daemon's state root —
`~/.local/state/sessions/uploads/` on Unix and the same child of
`%LOCALAPPDATA%\Sessions\state` on Windows. In the Go runtime an explicitly set
`SESSIONS_STATE_DIR` moves it, so a scratch daemon does not write into the
installed daemon's uploads; see `state-dir.md`. The Node fixture keeps it fixed
under `os.homedir()`. Responses:

- `200 {"path":"<absolute path>","size":<byte count>}`
- `404 {"error":"unknown session","id":"<id>"}` before reading the body
- `403 {"error":"upload directory outside home"}` when the resolved uploads
  directory is not inside the daemon's home directory, which an out-of-home
  `SESSIONS_STATE_DIR` produces
- `413 {"error":"file too large","max":26214400}` once the body exceeds 25
  MiB; the remainder is drained and no file is written
- `500 {"error":"<message>"}` for filesystem/read errors

### `GET /api/claude-sessions`

Auth required. Scans `~/.claude/projects/*/*.jsonl`, skips unreadable entries,
and sorts newest first. Returns 200:

```json
{
  "sessions": [
    {
      "sessionId": "<filename without .jsonl>",
      "cwd": "/decoded/project/path",
      "modifiedAt": 1750000000000,
      "firstUserMessage": "first user text, whitespace folded, max 200 chars",
      "sizeBytes": 1234
    }
  ]
}
```

The cwd decoder replaces every `-` in the project directory name with `/`, a
deliberately lossy mapping. An absent projects directory yields an empty list.

### `GET /api/resumable-conversations`

Auth required. This is the provider-neutral successor to
`GET /api/claude-sessions`. It scans the local Claude and Codex stores,
deduplicates resumed Codex rollout files by provider conversation identity,
and returns newest first:

```json
{
  "sessions": [
    {
      "sessionId": "<provider conversation UUID>",
      "tool": "claude",
      "origin": "Claude Code",
      "cwd": "/absolute/workspace",
      "modifiedAt": 1750000000000,
      "firstUserMessage": "bounded local preview",
      "sizeBytes": 1234
    }
  ]
}
```

`tool` is `claude` or `codex`. The endpoint is read-only and does not copy a
transcript. A native client may pass the chosen `sessionId` to the existing
`POST /api/recovery/adopt` boundary; the recovery layer still applies its live,
moved, collision, and explicit-provider guards before creating a Sessions
lane. The legacy Claude-only route retains its original response shape.

### `POST /api/recovery/adopt`

Auth required. Resolves one explicit provider conversation and creates its
successor through the normal write-ahead session boundary:

```json
{"target":"<provider UUID or conversation path>","sourceSessionId":"<optional ended Sessions id>","force":false}
```

A complete adoption returns `201` with `ok: true`, the new `laneId`, and the
resolved provider metadata. The runtime exists before the secondary
actor/provider/source-link annotations are appended. If one of those
post-launch appends fails, the endpoint therefore returns `202`, not a false
full failure:

`runtimeMode` is optional. Claude defaults to its native interactive runtime,
where Sessions presents Conversation and Terminal for the same process and the
destination's typed Claude setting enables Remote Control. Codex defaults to
its Rich app-server runtime. Explicit `rich` selects the structured runtime;
explicit `terminal` selects the provider terminal. Terminal is accepted only
for same-provider continuation; cross-provider continuation requires Rich mode
because its imported/linked context is delivered through the structured
runtime.

```json
{
  "ok": false,
  "partial": true,
  "laneId": "<live successor id>",
  "warning": "Session ... is running ... Repair records only; do not Resume again.",
  "missingAnnotations": ["source-linked"],
  "repair": {
    "target": "<same explicit provider source>",
    "laneId": "<live successor id>",
    "sourceSessionId": "<optional ended Sessions id>"
  },
  "adoption": {}
}
```

Clients must refresh and open `laneId` for both `201` and `202`. They must not
repeat the original Resume on `202`. To repair, POST the returned fields to the
same route with `repairLaneId`:

```json
{"target":"<provider source>","sourceSessionId":"<optional source>","repairLaneId":"<live successor>"}
```

Repair verifies the durable creation record, live successor, and exact provider
identity, then appends only missing annotations. It never calls the session
creator and cannot launch another runtime. Complete repair returns `200`;
another recoverable append failure remains `202`. A missing, ended, or
provider-mismatched successor returns `409` and explicitly says that no session
was started.

### `POST /api/recovery/fork`

Auth required. Creates a new conversation from a stable authored-history
snapshot while leaving the source unchanged:

```json
{"sourceSessionId":"<live Sessions id>","destinationProvider":"codex"}
```

`destinationProvider` is optional and defaults to the source provider. The
source must be a live, idle Claude or Codex session with a complete local
conversation. A working source returns `409`; clients should wait for its
current turn to finish instead of copying a partial assistant response.

To fork through one exact authored message, include its normalized transcript
index and stable ID:

```json
{
  "sourceSessionId": "<Sessions id>",
  "destinationProvider": "claude",
  "sourceMessageIndex": 42,
  "sourceMessageId": "<stable transcript message id>"
}
```

The index is zero-based. The ID is a concurrency guard: a mismatch returns
`409` and no runtime is created. Omitting both fields forks from the latest
stable authored snapshot.

Success returns `201`:

```json
{
  "ok": true,
  "laneId": "<new Rich session id>",
  "sourceHistoryId": "<stable local history id>",
  "sourceProvider": "claude",
  "destinationProvider": "codex",
  "mode": "native-import",
  "importedMessages": 42,
  "forkedFromSessionId": "<source Sessions id>",
  "forkPointIndex": 42,
  "forkPointMessageId": "<stable transcript message id>",
  "sourceUntouched": true
}
```

Only user and assistant messages cross the boundary. Sessions does not copy
credentials, tool output, attachments, diffs, or provider-internal events. The
new session is displayed beneath the source as a branch; trusted creator
provenance is not rewritten. The route never sends an end request, never marks
the source as reopened, and never requires a force flag.

### Cross-machine continuation

Cross-machine continuation is client-mediated. The native app or CLI
authenticates to the source and destination independently; neither daemon
receives the other daemon's bearer credential. The source provider history is
copied rather than deleted, and both ledgers record the continuation link only
after the target runtime starts. Only ended Claude and Codex sessions using the
default provider store are eligible. Isolated profile credentials, arbitrary
attachments, usage data, PTY history, and the full Sessions ledger are never
transferred.

#### `POST /api/migrate/export`

Auth required on the source. Body:

```json
{
  "session_id": "<ended source id>",
  "source_endpoint": "https://source.example.ts.net",
  "runtime_mode": "rich",
  "dry_run": true,
  "allow_dirty": false
}
```

`runtime_mode` is optional and must be `rich` or `terminal`. The route resolves
the exact provider conversation and safe resume recipe from the source's own
ledger and provider files. It rejects a live source with 409, an unsupported or
profiled source with 400, and a missing session with 404. `dry_run` computes the
plan without creating a checkpoint or writing a ledger event. A successful
response contains `request`, the authenticated handoff the client will carry to
the target, and `plan`, the operator review object. Conversation bytes are
bounded to 64 MiB.

#### `POST /api/migrate/receive`

Auth required on the destination. It accepts the `request` returned by export,
verifies the minimal provider resume recipe and conversation identity, prepares
or validates the destination workspace, and writes a new mode-0600 provider
history file. An identical existing file is idempotent; a different file at the
same provider identity is never overwritten. This route does not start a
runtime.

#### `POST /api/migrate/create`

Auth required on the destination. It accepts the same handoff metadata after a
successful receive, validates it again, starts exactly one target runtime
through the normal write-ahead create boundary, and records `moved_from`
provenance. It returns 201 with the new `SessionInfo`, lineage status, and any
recoverable annotation warning.

#### `POST /api/migrate/complete`

Auth required on the source. Body:

```json
{
  "source_id": "<ended source id>",
  "target_endpoint": "https://target.example.ts.net",
  "target_id": "<new target id>",
  "checkpoint_ref": "<optional Git checkpoint>"
}
```

The client calls this only after target creation succeeds. It verifies that the
source is still a known ended session, validates the target endpoint, and
records `moved_to` on the source. Failure does not remove the target or source
provider history; the returned error identifies that the target is live but the
source lineage annotation needs repair.

### `GET /api/directories`

Auth required. Returns `200 {"directories":[...]}`. Each entry is:

```json
{"path":"/absolute/path","label":"~/sessions/path","kind":"home"}
```

`kind` is `home`, `common`, `project`, or `somewhere`. The source offers
project-shaped checkouts beneath `~/somewhere` (including `~/somewhere/wt`)
first, followed by folders recently used by Sessions on that machine and
conventional local development roots. A project-shaped directory contains one
of `.git`, `package.json`, `pyproject.toml`, `Cargo.toml`, or `go.mod`.
Protected broad folders are offered as explicit choices without background
reads so discovery does not trigger unrelated macOS permission prompts.
Duplicates are skipped and the result remains bounded to roughly 50 entries.

### `GET /api/fs/list`

Auth required. Optional query `path` defaults to the OS home directory. It must
be absolute. The path and home are realpath-resolved when possible, and the
canonical path must equal the canonical home or be below it.

Success is 200:

```json
{
  "path": "/canonical/absolute/directory",
  "parent": "/canonical/absolute",
  "entries": [
    {"name":"child","kind":"dir","hidden":false}
  ]
}
```

`parent` is null only at the canonical home. Entry `kind` is `dir`, `file`,
`symlink`, or `other`; symlinks to readable directories/files are reported by
their target kind, while an unresolved symlink remains `symlink`. Entries sort
directories first, then locale-alphabetically with base sensitivity.

Errors:

- relative input: `400 {"error":"path must be absolute"}`
- outside home: `403 {"error":"path outside home directory","path":"<canonical>"}`
- non-directory: `400 {"error":"not a directory","path":"<canonical>"}`
- caught filesystem error: status 404 for `ENOENT`, 403 for `EACCES`, otherwise
  500, with `{"error":"<message>","code":"<errno code>"}`. Because nonexistent
  input first falls back to `path.resolve`, the eventual `statSync` normally
  supplies the `ENOENT` 404.

## Go runtime extension: tailnet discovery approval

The native client discovers online peers from the local `tailscale status
--json` result and probes their unauthenticated `/api/health` route. A healthy
peer is only a discovery candidate; it grants no session access.

`POST /api/tailnet/access/request` is exempt from bearer authentication only
when the immediate connection came through local Tailscale Serve with a
verified Tailscale identity. It rejects any request carrying an `Origin` header
and requires `Content-Type: application/json`, so browser JavaScript—including
the allowed Somewhere origins and daemon-served same-origin UI—cannot
participate in native onboarding or read its credential. It accepts:

```json
{"client_id":"<lowercase v4 UUID>","name":"MacBook Pro"}
```

It returns 202 with a ten-minute, in-memory request:

```json
{"request_id":"<UUID>","request_secret":"<random secret>","expires_at":"<RFC3339>","status":"pending"}
```

Repeating the request for the same Tailscale login and client UUID returns the
same pending request. At most 64 requests wait at once. The request secret is
returned only to the requester and is never exposed by the host listing.

The normally authenticated `GET /api/tailnet/access/requests` returns
`{"requests":[...]}` with pending request ID, client ID, device name, Tailscale
login/display name, creation/expiry times, and status. The authenticated
`POST /api/tailnet/access/requests/<request-id>` accepts
`{"decision":"accept"}` or `{"decision":"deny"}`.

The requester polls `POST /api/tailnet/access/claim` through the same verified
Tailscale Serve identity with:

```json
{"request_id":"<UUID>","request_secret":"<random secret>"}
```

Pending claims return 202, denied claims 403, and expired or mismatched claims
410. An accepted claim creates a two-minute pending per-device bearer
credential plus the daemon's stable machine ID/name, using the same response
shape as `POST /api/pair/claim`. Repeated claims return the same device ID and
token, so a lost 201 response is safe. The first authenticated API request with
that token durably acknowledges it; until then it is hidden from the device
list and cannot authorize after its deadline. Issuance starts its own two-minute
acknowledgement window even when host approval happened near the original
request deadline. Thus a response lost before the client receives the token
cannot leave a permanent active credential. Expired pending device records are
purged when the device store is next loaded. Pending request state itself
disappears on daemon restart or after its current deadline; the client can
safely request again.

`sessions pair` remains the explicit same-LAN fallback for devices without
Tailscale. It no longer creates Tailscale QR links.

## Go runtime extensions: smart search

The following authenticated routes are additive Go-runtime surfaces implemented
by `internal/api/search_handlers.go`; older compatibility runtimes return the standard
404 body for them.

### `GET /api/ai/settings`

Returns the smart-feature provider as `200 {"provider":"codex"}` or
`200 {"provider":"claude"}`. A missing setting defaults to `codex`; the default
does not itself launch a model.

### `GET /api/search`

Searches normalized Claude and Codex history on this daemon. `q` is required.
Ranked FTS5 recall is selected with `ranked=true`; bare terms are OR
alternatives, exact phrases and uppercase `AND`/`OR`/`NOT` are accepted, and
`near(a,b,N)` becomes an FTS5 proximity expression. `regex=true` selects Go
regular-expression matching; omitting both uses a case-insensitive contiguous
substring. Ranked and regex cannot be combined.

Optional filters are `session` (one ID/prefix or a comma-separated set),
`role=user|assistant|tool`, `tool=claude|codex|shell`, `name` (a session-name
glob), `cwd` (that workspace or a descendant), `since` and `until`
(`YYYY-MM-DD` or RFC3339), `context=0..20`, `timeline=true|false`, and
`limit=1..1000`. A date-only `until` includes that whole local calendar day.
Search spans every known persisted session by default. Invalid filters,
ambiguous session prefixes, malformed regex/FTS queries, or incompatible modes
return 400.

Each match includes session/name/provider/role/timestamp and a bounded snippet,
a stable zero-based `message_index`, content-derived `message_id`, byte match
span, normalized ranking score, workspace, machine, optional creator identity,
and requested neighboring messages. The full matching body is deliberately
omitted; the anchored history route retrieves it only when opened. Ranked
results are best-first unless timeline mode merges them chronologically.
Synthetic scheduled/task notifications and selected session-control relay
payloads have role `tool`; `kind` distinguishes `automation`, `delegation`,
`handoff`, and `status`, while arbitrary command/tool output is not indexed.
The private FTS index uses source path/size/nanosecond-mtime fingerprints,
serializes refreshes, stops parsing a canceled request, and purges indexed text
when its provider history is no longer available.

### `PUT /api/ai/settings`

Accepts `{"provider":"codex"}` or `{"provider":"claude"}`, persists the
normalized choice in daemon settings, and returns it. Unknown providers or
invalid JSON return 400; persistence errors return 500. Other methods return
405.

## Go runtime extension: onboarding and Claude launch defaults

### `GET /api/onboarding`

Returns the current machine-level user choice:

```json
{"version":2,"complete":false,"remoteControl":"pending","delegatedAccess":"pending"}
```

`remoteControl` is `pending`, `enabled`, or `local-only`. A missing, legacy, or
older-version onboarding record is always `pending`; provider settings do not
implicitly migrate into consent. `delegatedAccess` is `pending`, `inherit`, or
`autonomous`. `inherit` gives an agent-created child its manager's exact
provider permission mode. `autonomous` is explicit user consent for newly
created delegated children to use full access; it does not alter an existing
runtime.

### `PUT /api/onboarding`

The user-facing app submits both choices, for example
`{"remoteControl":"enabled","delegatedAccess":"inherit"}`, with
`X-Sessions-User-Consent: onboarding`. A v1 client that omits
`delegatedAccess` receives the conservative `inherit` behavior. The daemon
atomically records the current onboarding version, corresponding Claude launch
default, and delegated-access setting. A missing consent header returns 403;
unknown choices and invalid JSON return 400.

This is the only Sessions API that can grant Remote Control or autonomous
delegation consent. The CLI exposes `sessions onboarding` as read-only status
so an agent can inspect and explain either choice but cannot silently make it.
All routes still require the normal daemon authorization. The extra header is a
product-surface guard, not a second authentication factor.

### `GET /api/claude/settings`

Returns the effective typed defaults Sessions applies only to newly launched
Claude sessions:

```json
{"remoteControl":"off","permissionMode":"inherit","model":"","effort":"inherit","chrome":"inherit","somewhereMcp":"inherit","remoteControlNamePrefix":""}
```

Remote Control and Chrome accept `inherit`, `on`, or `off`; permission mode
accepts Claude's supported modes plus `inherit`; effort accepts `inherit`,
`low`, `medium`, `high`, `xhigh`, or `max`; Somewhere MCP accepts `inherit` or
`ensure`. Empty model and name-prefix fields preserve provider defaults.
Remote Control is an interactive Claude capability. The native Claude runtime
is the default, but Remote Control stays `off` until the user explicitly
enables it through onboarding or Settings. `off` starts the same Conversation
+ Terminal runtime without claude.ai/mobile connectivity. Explicit Rich
`claude-structured` sessions disable provider Remote Control because Claude's
`--print` interface cannot join the interactive Remote Control session.

### `PUT /api/claude/settings`

Validates and atomically persists the complete object above. A request for
`remoteControl: "on"` returns 403 until onboarding contains the current
explicit enabled choice. The daemon never edits Claude settings files or
stores provider credentials. Unknown choices, control characters, overlong
strings, invalid JSON, and unsupported methods return 400 or 405 as
appropriate.

`POST /api/sessions` may include a `claude` object with the same fields.
Non-empty values override the persisted Sessions defaults for that launch,
except Remote Control cannot override missing user consent; explicit `inherit`
defers the other setting to Claude. The object is rejected for a non-Claude
command. `somewhereMcp: "ensure"` adopts an equivalent existing
registration or injects the local `somewhere mcp` stdio adapter; a conflicting
server named `somewhere` fails closed rather than being overwritten.
An explicit `--remote-control` argument is rejected without recorded consent
and is also rejected for a `claude-structured` launch with an instructional
400 response; choose a Terminal runtime after the user enables the feature.

`POST /api/recovery/adopt` accepts `remoteControl: true` only together with
`runtimeMode: "terminal"` for a same-provider Claude continuation. It forwards
the typed per-launch setting through the normal session-creation boundary;
Rich, Codex, cross-provider, and repair requests reject the combination rather
than silently ignoring it. The CLI equivalent is
`sessions continue <history-id> --terminal --remote-control`.

### `POST /api/search/plan`

Accepts `{"query":"the session where I discussed Apple signing"}`. The query
is trimmed and limited to 4 KiB, then sent as untrusted data in one tool-disabled,
customization-isolated request to the configured, already-authenticated Codex or Claude CLI. Sessions
sends no transcripts, snippets, session IDs, results, or index content. The CLI
chooses its own default model. Success returns the bounded FTS5 plan:

```json
{"provider":"codex","query":"apple OR notarization OR signing"}
```

The browser applies that query through the existing local `GET /api/search`
route and keeps `role` and `tool` filters deterministic. Empty/oversized input
returns 400. Only one planner request may be active; another distinct request
returns 429 with `Retry-After: 2`. Successful plans for an identical provider
and normalized natural query are cached for ten minutes. Planner or provider
failures return 502, and other methods return 405. The handler deadline is two
minutes. Cache keys are SHA-256 digests, the cache holds at most 128 plans, and
entries are evicted on their expiry timer even without another lookup.

## Go runtime extensions: history views

The existing authenticated `GET /api/history/<id>` route remains complete by
default. The transcript response assigns a stable zero-based `index` to every
normalized message.

Every response on these routes may carry the additive `skipped_records` counter,
omitted when zero, reporting records the torn-record policy could not decode and
skipped. A nonzero value means the answer is degraded but usable — complete
minus those records — and indices are assigned over the records that decoded.
`GET /api/history` degrades per item for the same reason and adds
`unreadable_sessions`; [`docs/INTEGRATIONS.md`](../../docs/INTEGRATIONS.md) is
the field-level contract.

Native search viewing first requests
`GET /api/history/<id>/window?format=json&start=N&end=M`; `end` is exclusive.
One response spans at most 500 original message positions; omitting `end`
selects the next 500 after `start`. `role=user|assistant|tool` retains only that
normalized role inside the page. The response keeps the complete transcript
count in `session.message_count`, preserves original message indices, sets
`truncated:true` when it omitted messages, and returns `has_more` plus
`next_index` for paging. The initial search open also supplies
`anchor=<message_index>&message_id=<message_id>`; a stale or rewritten bookmark
returns 409 rather than marking the wrong message. Invalid or overlarge ranges
and invalid roles return 400. This lets the native client page through
user-only, after-hit, bookmarked-range, or full views without materializing a
giant transcript in one daemon response or WebView render.

The compatibility viewer may request the distinct
`GET /api/history/<id>/preview?format=json` path, which reads at most the latest 2 MiB of the JSONL
artifact and returns at most its latest 400 normalized messages. The additive
response field `"truncated":true` appears when either bound removed older
content; it is omitted for a complete preview. This bound does not change the
deliberate full-history JSON/text response or `/api/history/<id>/raw` download.
The distinct path is intentional: an older runtime returns 404 instead of
silently ignoring a query parameter and sending an unbounded transcript.

## Static GETs

A GET is treated as static when its path does not start `/api/`, is not exactly
`/api`, and does not start `/ws`. Static serving is unauthenticated.

The web root is `SESSIONS_WEB_DIR` when set; otherwise it prefers
`frontend/dist` and falls back to the package's bundled `web`. Paths are URI
decoded, normalized, and constrained below that root. A readable exact file or
directory `index.html` is served; otherwise the web-root `index.html` is served
as the SPA fallback. If no web build is available, response is
`404 {"error":"web build not found","path":"<absolute web root>"}`. An invalid
decode or traversal candidate is `400 {"error":"invalid path"}`.

Success has only a 200 status and inferred `Content-Type`; it does not add the
common CORS headers. Recognized extensions are HTML, JS, CSS, JSON, SVG, PNG,
JPEG, WebP, ICO, webmanifest, WOFF/WOFF2, TTF, OTF, and WASM; everything else is
`application/octet-stream`. A stream error is a JSON `{"error":"<message>"}`
body with 500 only if headers have not already been sent.
