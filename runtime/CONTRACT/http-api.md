# sessionsd HTTP API contract

This document records the behavior of the normative Go daemon in
`runtime/internal/api`.

## Listener and common behavior

- The default listener is `127.0.0.1:8787`. `SESSIONS_HOST` and
  `SESSIONS_PORT` override it. The server refuses `0.0.0.0`, `::`, `::0`, and
  `*` with process exit status 2.
- When automatic Tailscale reachability is on and `tailscale status --json`
  reports a signed-in peer, the daemon also binds the same port on that peer's
  exact IPv4 address in `100.64.0.0/10`. It never wildcard-binds this listener
  or substitutes a LAN/private address. Tailscale authenticates and encrypts
  this HTTP transport; Sessions authentication remains required as below.
- All JSON replies are compact `JSON.stringify` output with
  `Content-Type: application/json`. Except for static-file replies and the
  plain-text snapshot success response, every reply also sets:
  - `Vary: Origin`
  - `Access-Control-Allow-Methods: GET,POST,PUT,DELETE,OPTIONS`
  - `Access-Control-Allow-Headers: content-type, authorization,
    x-sessions-creator-session, x-sessions-owner-id, x-sessions-client,
    x-sessions-filename, x-sessions-user-consent`
  - `Access-Control-Allow-Origin: <request Origin>` only when the Origin is
    allowed as described below.
- Every `OPTIONS` request with a Host that identifies the listener returns 204
  before auth or route matching. An invalid Host is rejected first as described
  below.
- JSON bodies are limited to 2 MiB. An empty body decodes as `{}`. Invalid JSON
  and an oversized body become the route's documented error response.
- A method/path combination not matched below reaches `404
  {"error":"not found","path":"<pathname>"}` after auth. Most Go route
  families claim their path first and answer a wrong method with `405
  {"error":"method not allowed"}` (the handlers that use
  `http.StatusMethodNotAllowed` in `runtime/internal/api`); the exact-path
  routes matched inline in `server_routes.go` (`/api/sessions`,
  `/api/machine`, `/api/sessions/end-batch`, `/api/recovery/*`,
  `/api/directories`, `/api/fs/list`, `/api/claude-sessions`,
  `/api/resumable-conversations`, `/api/models/codex`, the push routes) and
  the per-session suffixes served by `handleSessionRoute` (`/snapshot`,
  `/events`, `/model-options`, `/model`, `/input`, `/submit`, `/approve`, `/retry`, `/retry/stop`, `/name`,
  `/tags`, `/upload`, `/display-parent`, `/set-aside`, the bare `DELETE`)
  still fall through to the 404 body. `/pin`, `/wait`, `/wait-state`, and
  `/verdict` return 405. Either way the request is authenticated before the
  method is judged, so a wrong method on an API path normally requires auth
  before returning 404 or 405. Each route entry below states its own
  behavior where it differs.
- Route handlers return the documented JSON errors explicitly. A panic is not
  part of the HTTP contract; Go's `net/http` server terminates the affected
  request rather than exposing an internal error string to the client.

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
`open` file beside the token bypasses token auth. This is full daemon control,
not a read-only sharing mode: it includes creating sessions, sending input, and
ending processes. Failed auth is `401 {"error":"unauthorized"}`.

The per-device token issued by pairing is likewise a host-administrator
credential. It intentionally supports the native client and agent parity
surface, including session creation, input, and termination. Anyone who holds
one can run commands with the authority of the Sessions user; revoke a lost or
untrusted device immediately. Pairing is not a transcript-only viewer grant.

The Go runtime adds two narrowly exempt Tailscale bootstrap routes documented
below. They do not accept caller-supplied identity: the immediate TCP peer must
be loopback and Tailscale Serve must have injected exactly one valid
`Tailscale-User-Login` header. Every approval-management route and every route
used after the bootstrap still requires the normal bearer credential. Local
processes are outside this identity-display boundary: like the normal loopback
API shortcut, a malicious process already running as the user can fabricate
these headers and already has local daemon control.

## Origin and CORS rules

Before CORS or route dispatch, the daemon verifies that the HTTP `Host` names
its configured loopback or enabled LAN listener on the bound port. This closes
DNS rebinding, where an attacker-controlled hostname resolves to 127.0.0.1.
Tailscale Serve is the supported proxy exception and must supply a verified
Serve identity from an immediate loopback peer. A rejected Host returns 421.

An absent `Origin` is allowed. A present value must parse as a URL and satisfy
one of these rules:

1. its serialized origin is `https://sessions.somewhere.tech` or the platform's
   canonical redirect target `https://sessions.somewhere.site`;
2. its hostname is exactly `127.0.0.1`, `localhost`, or `::1`; or
3. its hostname is exactly the configured bind host.

Scheme and port are unrestricted for the hostname rules. The two hosted values
are serialized-origin matches, so another scheme or port fails. For HTTP
reads, a disallowed or malformed Origin omits `Access-Control-Allow-Origin`.
For state-changing methods, a browser Origin outside the native/same-listener
allowlist is rejected with 403 unless the request carries a credential that
actually verifies; the mere presence of an Authorization header is not enough.
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
| `failureKind` | `"provider-unavailable" \| "rate-limited" \| "auth" \| "other"`, optional | classified provider-turn failure; absent after a later turn succeeds |
| `failureDetail` | string, optional | concise provider fault in Sessions' words; also becomes `lastSummary` for the failed turn |
| `failureProvider` | `"claude" \| "codex"`, optional | provider that produced the current fault |
| `failureAt` | number, optional | Unix epoch milliseconds when the current provider fault was observed |
| `retry` | object, optional | runner-owned automatic Rich-turn retry schedule: `attempt` (the next retry, 1–5), `max` (5), `nextAt` (Unix epoch milliseconds), and `kind` (the current `failureKind`). Present while a retry is scheduled or running; omitted after success, cancellation, or exhaustion |
| `pendingApproval` | object, optional | permission a Rich session is waiting on, with `id`, `kind` (`command`, `file-change`, or `permissions`), `summary`, `command`, `cwd`, `reason`, and `at`; absent when no approval is pending |
| `exited` | boolean | whether Sessions reaped a real status for the session's process: an EXIT frame, a signal, or a completed user-requested end. It is never set because the daemon lost contact with a runner |
| `exitCode` | number or null | PTY exit code |
| `exitSignal` | string or null | PTY exit signal as a string |
| `exitedAt` | number or null | Unix epoch milliseconds when EXIT was received |
| `unreachable` | boolean, optional | present as `true` when the daemon cannot currently talk to the session's runner (socket read error, read deadline, daemon restart). This is a statement about the connection, not the work: the session is still listed, readable, and attachable, and reconnect or the next discovery pass may reattach it. It is never presented as ended, and `exited` stays `false` |
| `unreachableReason` | string, optional | why contact was lost; `"runner-lost"` for a socket read failure |
| `unreachableSince` | number, optional | Unix epoch milliseconds when contact was lost |
| `runnerGone` | boolean, optional | present as `true` only when the daemon's identity-aware process probe found no process belonging to this session. This is stronger than `unreachable`: the runtime cannot reconnect by itself. It still does not invent an exit status, so `exited` remains `false` |
| `claudeCustomTitle` | string, optional | latest Claude `custom-title` value |
| `claudeAiTitle` | string, optional | latest Claude `ai-title` value |
| `onIdle` | string, optional | trimmed per-session idle hook command |
| `model` | string, optional | model parsed from effective arguments |
| `effort` | string, optional | effort parsed from effective arguments |
| `fast` | boolean, optional | present as `true` for Codex priority service tier; otherwise omitted |
| `setAsideAt` | number, optional | Unix epoch milliseconds when the live session was removed from the native app's default working set; this is organization, not lifecycle |
| `pinned` | boolean, always present | whether the user marked this live session as a workbench: it sorts first in every listing and any future automatic cleanup policy must leave it alone. Always present, including as `false`, so a caller can tell "not pinned" from a daemon that predates the field |
| `memoryBytes` | number, optional | resident memory of the session's whole process tree at `resourceSampledAt`. **Omitted, never zero, when unknown** — no live process, a process the daemon may not inspect, or a platform without sampling. A client that treats a missing value as `0` will report a saturated machine as idle |
| `cpuPercent` | number, optional | percent of one core the process tree used between the two most recent samples. It is a rate, not an average over the life of the process; a tree spanning cores exceeds 100. Omitted when unknown, which includes the first sample of a tree, because a rate needs two readings. Omitted is not zero; a measured zero is sent as `0` |
| `resourceProcesses` | number, optional | how many processes `memoryBytes` and `cpuPercent` cover. Present exactly when `memoryBytes` is |
| `resourceSampledAt` | number, optional | Unix epoch milliseconds when the three fields above were measured. Sampling is periodic, so a reader must treat the figures as of this time rather than as of the response |
| `delegation_kind` | `"user" \| "agent"`, optional | presentation provenance for a child session: explicitly started by the user or created by its parent agent |
| `permissions` | `"constrained" \| "full"`, optional | daemon-resolved access class for this runtime; provider-specific approval and sandbox arguments remain visible in `args` |
| `lifecycle` | `"task" \| "session"`, optional | caller-declared runtime intent; it never authorizes Sessions to infer that a final response means the runtime should end |

Exited sessions remain in the daemon map for 30 seconds. They are omitted from
the default list but can be requested with `include_exited=1` during that grace
period. Unreachable sessions are not exited and are **not** filtered out of the
default list: losing a socket is not an ending, and hiding the session on that
basis would make work the daemon cannot currently reach look like work that
never existed.

### Standard error bodies

Error strings originating from the provider, filesystem, JSON parsing, launchd, or
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
  "version": "0.2.26",
  "listen": { "host": "127.0.0.1", "port": 8787 },
  "lan": {
    "enabled": true,
    "url": "http://192.168.1.24:8787",
    "bonjour": { "advertised": true, "service": "_sessions._tcp" }
  },
  "tailscale": {
    "present": true,
    "signedIn": true,
    "remoteEndpoint": "https://mini.example.ts.net",
    "tailnetIpEndpoint": "http://100.100.20.30:8787",
    "auto": true,
    "enabled": true
  },
  "account": {
    "signedIn": true,
    "lastRegistrationAt": "2026-09-03T20:15:00Z"
  },
  "access": { "open": false },
  "system": { "os": "darwin", "arch": "arm64" },
  "compatibility": {
    "api": { "current": 1, "minimumClient": 1, "maximumClient": 1 },
    "runner": { "current": 2, "minimum": 0, "maximum": 2 }
  },
  "discovering": false,
  "sessionsLoaded": 0,
  "restore": { "pending": 0, "automaticPinnedLimit": 8 }
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

`tailscale.present` reports whether the CLI was found, `signedIn` reports a
running backend with a local peer, `remoteEndpoint` is the Tailscale Serve HTTPS
origin, `tailnetIpEndpoint` is the direct CGNAT HTTP origin, and `auto` is the
persisted default-on choice. Missing endpoints are omitted. `enabled` means at
least one automatic listener is active; `preview`, when true, means endpoints
were detected without changing Serve or opening the direct listener.

`account.signedIn` reports whether sessionsd holds a complete Somewhere
access/refresh pair. `lastRegistrationAt` and `lastRegistrationError` are
omitted until one exists. This health projection never includes the email,
token pair, machine ID, or public key, because `/api/health` remains available
without authentication. Deep health carries the same `account` object.

`system.os` uses Go's stable platform names (`darwin`, `windows`, `linux`, and
so on) so native clients can choose a machine icon without guessing from a
hostname. `compatibility.api` is the authoritative client acceptance range;
`compatibility.runner` describes the living runners this daemon can adopt.
Clients preserve their legacy behavior when an older daemon omits the additive
object, but must stop before normal use when their protocol is outside an
advertised range. The count includes exited sessions still in their 30-second
grace period. The deep-health response carries the same `compatibility`,
`access`, and `tailscale` objects but no `listen` or `lan`. `restore.pending` counts runners
Sessions deliberately left stopped after reboot rather than starting an
unbounded retained fleet; their recovery evidence is preserved.
`restore.automaticPinnedLimit` is the compiled ceiling for the most recently
active pinned non-lane roots that may return automatically. A non-zero pending
count sets top-level `status` and `restore.status` to `"degraded"` and adds
`restore.code: "SESSION_RESTORE_PENDING"`, a human-readable message, and
`restore.action: "sessions doctor"`. `ok` continues to mean that the
daemon itself is serving requests; `status` carries this recoverable degraded
condition.

### `GET /api/health/deep`

Requires authentication (loopback peers are already authorized). Returns 200:

```json
{
  "ok": true,
  "name": "sessionsd",
  "version": "0.2.26",
  "status": "healthy",
  "discovering": false,
  "sessionsLoaded": 1,
  "restore": {
    "pending": 0,
    "automaticPinnedLimit": 8,
    "degraded": false,
    "status": "healthy"
  },
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

### `/api/account/*`

Auth required and local-principal only. A paired device, remote master token,
or open-access caller receives 403; the account token pair and machine private
key therefore remain daemon-owned on their host.

- `GET /api/account` returns
  `{"signed_in":false}` or the stored public user, machine public key, and
  optional `last_registration_at`, `last_registration_error`, and
  `last_heartbeat_at` fields. It never returns access, refresh, logout-session,
  or private-key bytes.
- `POST /api/account/magic-link` with `{"email":"..."}` requests a
  Somewhere magic link. Success is `{"ok":true}`.
- `POST /api/account/verify` with `{"token":"<code-or-link-token>"}`
  exchanges the single-use token, atomically stores the returned token pair,
  attempts immediate machine registration, and returns the same shape as
  `GET /api/account`. A registration failure is preserved in status rather
  than invalidating the consumed login token.
- `POST /api/account/logout` removes the signed machine row, revokes the
  Somewhere auth session, and then removes local account state. A network or
  platform failure leaves local state intact so the operation can be retried.
- `GET /api/account/key` creates the machine key when missing and returns only
  `{"public_key":"<unpadded-base64url-Ed25519-key>"}`.
- `GET /api/account/machines` returns
  `{"signed_in":<boolean>,"machine_id":"<local id>","machines":[...]}`.
  Signed-out machines return an empty list. Signed-in rows are the
  owner-scoped Somewhere directory objects with `id`, `name`,
  `machine_public_key`, `endpoints_json`, `daemon_version`, and
  `last_seen_at`.
- `POST /api/account/machines/claim` with `{"machine_id":"<directory id>"}`
  probes that row's LAN, Tailscale HTTPS, then Tailscale-IP candidates, signs
  an account challenge with this daemon's registered machine key, verifies the
  returned credential, and returns the normal local connection shape:
  `{"claim":{...},"endpoint":"<origin>","transport":"lan|tailnet|tailnet-ip"}`.

Wrong methods return 405. Invalid request bodies return 400; an unavailable
Somewhere auth or directory request returns 502. Account storage failures and
an unavailable machine identity return 500.

### `GET /api/remote`

Auth required. Returns the automatic Tailscale state using the same fields as
`GET /api/health`'s `tailscale` object, except the endpoint fields are named
`endpoint` and `tailnetIpEndpoint`. `auto` defaults to true even before a
settings file exists. `preview` is present only for an explicitly previewed
daemon.

### `PUT /api/remote`

Auth required, local-principal only. Body `{"auto":true}` persists automatic
tailnet reachability and immediately rechecks Tailscale. `{"auto":false}`
closes the direct Tailscale-IP listener and removes the Serve root only when it
currently targets this daemon. A missing or non-boolean value is 400. Tailscale
need not be installed to turn the setting off. Other methods return 405.

### `GET /api/machine`

Auth required. Returns the daemon's stable machine identity, the same
`machine_id` a paired device receives from `POST /api/lan/access/claim`:

```json
{"machine_id":"<stable machine UUID>","name":"<computer name>"}
```

`name` is the operating system's user-facing computer name, truncated to the
machine-name limit. A legacy DNS-derived name is upgraded without changing the
stable machine UUID. When the identity file could not be created or read, the
route returns `500 {"error":"<message>"}`. Other methods fall through to the
404 body.

### `GET /api/fleet/machines`

Auth required, with an additional caller restriction: only a loopback-local
caller or a paired-device credential may use the fleet relay. The daemon master
token and anonymous `open` access receive 403. The route reads the same saved
machine registry and separate per-machine credential files used by `sessions
machines`; it does not list discovery candidates or machines that this host has
not itself been approved on.

```json
{"machines":[{"id":"<machine id>","name":"Mac mini","endpoint":"https://mini.example.ts.net","transport":"tailnet","lan_endpoint":"http://192.168.1.24:8787","tailnet_endpoint":"https://mini.example.ts.net","tailnet_ip_endpoint":"http://100.100.20.30:8787","reachable":true}]}
```

Each row carries every saved origin additively. `transport` records the origin
currently in use and is `lan`, `tailnet`, or `tailnet-ip`. The host tries LAN,
then Tailscale HTTPS, then direct Tailscale-IP HTTP, making authenticated `GET
/api/machine` probes with its saved credential and requiring the returned stable
identity to match `id`. Offline machines remain in the array with
`reachable:false`. When a Darwin probe of a private or link-local destination
fails with `EHOSTUNREACH`, that row additionally carries
`reason:"local-network-permission"` and the exact `message` shown above.
Other reachability failures omit both fields. The response never contains a
credential or paired-device ID. An unreadable, malformed, or unsupported saved
machine registry is 500.

### `/api/fleet/:machine-id/api/*` and `/api/fleet/:machine-id/ws`

These are the authenticated host-relay prefixes for ordinary HTTP routes and
the `/ws` WebSocket mux. They have the same local-or-paired-device caller
restriction as the fleet listing. The daemon resolves `machine-id` only against
its current saved registry and requires the separate credential file to exist;
an unknown, forgotten, malformed, or credential-less ID is 404 before any
outbound request. Consequently even otherwise public remote routes such as
`/api/health`, and authenticated identity at `/api/machine`, cannot be reached
through an unsaved machine ID.

The suffix is forwarded unchanged to the first reachable saved endpoint in the
same LAN, Tailscale HTTPS, direct Tailscale-IP order. Request and response
bodies are streamed, and Go's reverse proxy carries WebSocket upgrades, so the
existing `/ws?mux=1` protocol works through the relay. The phone's
`Authorization` and `Proxy-Authorization` headers and `token` query parameter
are removed. The host then supplies its own saved per-machine bearer credential;
the destination therefore sees and can revoke the host's normal paired-device
identity. Other headers, including `X-Sessions-Creator-Session` and
`X-Sessions-Owner-ID`, retain their values. A transport failure is 502. Every
relayed request is logged at info level with method, destination path, machine
ID, and calling device ID (or `local`), but never with a request body or token.
A private or link-local Darwin dial that fails with `EHOSTUNREACH` uses the
exact Local Network permission sentence documented by `GET
/api/fleet/machines` instead of exposing `no route to host`.

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

### `GET /api/notify`

Auth required. Returns the persisted push-notification preferences and whether
any Web Push subscription is registered:

```json
{"notify":{"done":true,"waiting":true,"lost":true},"subscribed":false}
```

`done`, `waiting`, and `lost` are the three notification kinds. A settings
read failure is 500.

### `POST /api/notify`

Auth required. Body `{"enabled":<boolean>,"kind":"<done|waiting|lost>"}`.
`enabled` is mandatory; an absent or non-boolean value is
`400 {"error":"enabled must be true or false"}`. An empty or omitted `kind`
sets all three kinds at once; any other value is
`400 {"error":"unknown notification kind ..."}`. The result is persisted in
daemon settings and the same body as `GET /api/notify` is returned.
Persistence failures are 500; other methods return 405.

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
| `initialInput` | string | optional; the first request when the provider consumes it from `args` (a terminal Codex session), carried so the transcript watcher binds to the rollout that records it |
| `onIdle` | string | trimmed; empty becomes absent |
| `waitReady` | boolean | only literal `true` waits for readiness, capped at 30 seconds |
| `delegationKind` | `"user" \| "agent"` | optional child presentation provenance; requires a validated `X-Sessions-Creator-Session` parent |
| `providerTerminal` | boolean | explicit escape hatch for an agent-created Claude child that needs the interactive provider terminal; otherwise newly attributed agent children use the structured Claude runtime |
| `permissions` | `"inherit" \| "constrained" \| "full"` | optional requested access; `inherit` requires a parent, and a child cannot exceed its parent unless the user explicitly enabled autonomous delegated work |
| `lifecycle` | `"task" \| "session"` | optional runtime intent; all sessions, including agent-created children, default to `session`; callers must explicitly request a bounded `task` |

`RUNNER_*`, `NODE_OPTIONS`, `DYLD_INSERT_LIBRARIES`, `DYLD_LIBRARY_PATH`, and
`LD_PRELOAD` caller keys are stripped. User-created Claude/Codex sessions are
constrained unless full access is explicitly requested. An agent-created child
inherits the parent's exact Claude permission mode or Codex sandbox and
approval flags. When no exact Codex policy is inherited or supplied, the
constrained default is a workspace-write sandbox with the provider's untrusted
approval policy; on-request remains valid only when the caller explicitly
supplies or inherits it. Newly attributed Claude agent children default to the
provider's structured runtime; `providerTerminal: true` deliberately keeps a
child on the interactive terminal and never enables Remote Control by itself.
The daemon rejects self-escalation. A machine-level autonomous
delegation choice can make new agent-created children full-access; only the
explicit host onboarding/Settings route can grant that choice. Success
is 201 with a bare `SessionInfo` object, not an envelope. A create failure is
`400 {"error":"<message>"}`, with one exception: when the request resumes a
provider conversation that is already live in another session
(`sessionruntime.ConversationLiveError`) or that has been moved to another
machine (`sessionruntime.ConversationMovedError`), the daemon returns
`409 {"error":"<message>"}` so a client can distinguish a guard from bad
input. Creating a session invokes the platform runner supervisor; there is no
unmanaged create path in the normative implementation.

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

### `GET /api/lanes`

Auth required. Lists every session whose `kind` is `lane`, exited or not, in
daemon map order. Each element is a `SessionInfo` plus an additive
`lane_status` object and a `manifest` object when the lane's completion
manifest is readable:

```json
{"lanes":[{/* SessionInfo fields */,"lane_status":{"state":"exited"},"manifest":{"exit_code":0,"signal":null,"duration_ms":1234,"last_output_tail":"...","spec_path":"...","files_changed":3}}],"user_creator_id":"<local user creator id>"}
```

`lane_status.state` is `running`, `exited`, `unreachable`, `lost`, or
`needs-recovery`. A lost headless lane carries the short reason and the exact
command that closes its retained record without pretending an exit was
observed:

```json
{"lane_status":{"state":"lost","reason":"runner process is gone","command":"sessions kill <id>"}}
```

`manifest` is omitted while a lane has no readable completion manifest; that
absence alone does not mean the lane is running. `files_changed` is omitted
when unknown. A failure to resolve the local user creator ID is 500; other
methods return 405.

### `POST /api/lanes`

Auth required. Accepts the same body as `POST /api/sessions` and forces
`kind` to `lane`; a body whose `kind` is anything else is
`400 {"error":"lane kind must be \"lane\""}`. `cmd` is mandatory for a lane
(`400 {"error":"lane command is required"}`). Creator headers are captured
exactly as for `POST /api/sessions`. Success is 201 with a bare `SessionInfo`.
Every other create failure is `400 {"error":"<message>"}`; this route does not
map the live/moved conversation guards to 409.

### `GET /api/lanes/:id/manifest`

Auth required. `:id` must be a 36-character hyphenated UUID; any other id, any
other suffix below `/api/lanes/`, or any other method is the standard 404
body. A lane that is still running returns
`409 {"error":"lane is still running","id":"<id>"}`. Otherwise the completion
manifest is returned bare (`exit_code`, `signal`, `duration_ms`,
`last_output_tail`, `spec_path`, optional `files_changed`). A missing manifest
is `404 {"error":"unknown lane","id":"<id>"}` and any other read error is 500.

### `GET /api/lanes/mine`

Auth required. Answers "what am I responsible for" for one calling lane. The
caller is identified by `?lane=<id>` or, when that is absent, by the
`X-Sessions-Creator-Session` header; a missing identity or a malformed header
is 400, and an id that matches no session on this machine is
`404 {"error":"no session matches <id> on this machine"}`. The listing never
widens beyond the caller, its parent, and its transitive descendants (display
parent preferred over creator lineage, depth capped at 8):

```json
{"self":{...},"parent":{...},"members":[{...}],"needs_input":1}
```

Every member object carries `id`, optional `name`, `tool`, optional `cwd`,
`relation` (`self`, `parent`, or `child`), `depth`, `state` (`ended`,
`needs-recovery`, `lost`, `unreachable`, `needs-you`, `working`, `failed`,
`not-started`, or `idle`), `needs_you`, `branch` and `worktree_path` when the
lane works in its own worktree, `working`, `exited`, optional `summary`,
optional `waiting`, and optional `updated_at`. A lost or paused member also
carries `reason` and `recovery_command`; the command is
`sessions resume <id>` when a provider conversation can continue and
`sessions kill <id>` when only a headless record can be closed. `summary` and
`waiting` are capped at 200 bytes; no transcript, args, or env is included.
`parent` is omitted when the caller has none.
`members` sorts by `depth`, then `updated_at` descending; `needs_input` counts
live members waiting on a decision.

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

With `include_models=1`, each provider also returns `models`, using the same
model objects as `GET /api/models/codex`: `id`, `displayName`, `isDefault`,
supported effort choices, and the default effort. A provider whose catalog
cannot be loaded returns `modelsError` without hiding its installation status.

### `POST /api/providers/:id/update`

Auth required. A loopback client, the master token, or an explicitly paired
device may request the update; anonymous open remote access receives 403 before
executable lookup or mutation. The client must identify the destination machine
in its confirmation UI. The provider ID must be `claude` or `codex`; the daemon
then runs that installed provider's own `update` command with a five-minute
deadline. Only one provider update runs on a machine at a time; a second request
returns 409 instead of waiting behind the first. The updater runs in an isolated
process tree, and a timeout stops its package-manager descendants as well as the
provider parent. Success returns the refreshed provider object and bounded
installer output. Unknown, absent, failed, or timed-out providers return 4xx/5xx
without changing Sessions itself.

### `GET /api/worktrees`

Auth required. Returns worktrees created by Sessions according to ledger
provenance, never arbitrary Git worktrees. Successfully cleaned worktrees are
omitted unless the optional `all=true` query is present. Each result includes
`session`, `session_name`, `worktree_path`, `branch`, `base`, `source_repo`,
`tree_state`, `dirty`, `merged_into_base`, `session_state`, `exists`, `cleaned`,
`branch_removed`, and an optional `inspection_error`. Retained cleaned rows also
include `cleaned_at` as Unix milliseconds; their `tree_state` is `cleaned`:

```json
{"worktrees":[{"session":"<Sessions id>","session_name":"parser","worktree_path":"/work/project-wt/parser","branch":"sessions/parser","base":"main","source_repo":"/work/project","tree_state":"cleaned","dirty":false,"merged_into_base":true,"session_state":"exited","exists":false,"cleaned":true,"cleaned_at":1788389151693,"branch_removed":true}]}
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
Before Git mutation, cleanup records an append-only `worktree_clean_requested`
fact after the final safety check. After removal it records `worktree_cleaned`,
so the row remains auditable through `GET /api/worktrees?all=true` but no longer
clutters the default listing. An interrupted cleanup intent is reconciled by a
later clean without weakening the original safety decision.

### `GET /api/projects`

Auth required. Groups every known session, exited included, by resolved
project. Stored projects appear even with no sessions; implicit ones exist
only while a session sits in their folder:

```json
{"projects":[{"id":"p_...","name":"...","implicit":false,"roots":["/absolute/folder"],"github":"owner/repo","somewhere":"...","pinned":true,"session_ids":["<Sessions id>"],"live":2,"needs_input":1,"updated_at":1750000000000}]}
```

`github`, `somewhere`, and `pinned` are omitted when empty. Order is pinned
first, then stored before implicit, then `updated_at` descending. A resolution
failure is 500. When the project store is unavailable every `/api/projects`
path returns `501 {"error":"projects are not available on this runtime"}`.

### `GET /api/projects/suggest`

Auth required. Query `cwd` is mandatory (`400 {"error":"cwd is required"}`).
Returns a bare `Project` to seed a "name this project" form: for an unclaimed
folder, `name`, `roots` (the folder's top level), and any detected `github` or
`somewhere`, with `id`, `created_at`, and `updated_at` at their zero values;
for a folder that already belongs to a stored project, that project, so naming
it again renames in place.

### `PUT /api/projects`

Auth required. Body is a `Project` (`id`, `name`, `roots`, `github`,
`somewhere`, `pinned`); an empty `id` creates and a known `id` updates.
`name` is mandatory and at most 120 characters, at least one root is required,
every root must be an absolute folder, and a root already belonging to another
project is rejected; each of these is `400 {"error":"<message>"}`. Returns 200
with the stored `Project`, including `created_at` and `updated_at`.

### `DELETE /api/projects/:id`

Auth required. Forgets a stored project; its sessions become implicit again.
Returns `200 {"ok":true}`. An empty or nested id, or an unknown project, is
404. Any other method on a `/api/projects` path is
`405 {"error":"method not allowed","path":"<pathname>"}`.

### `POST /api/retention/gc`

Auth required. Body is
`{"older_than_ms":2592000000,"dry_run":true|false}`. The age must be between
one hour and ten years. This is list-retention, not transcript deletion: it
considers only durably closed ledger records and appends an `archived` fact
instead of deleting lifecycle events or runner artifacts. A live runner is
always skipped. Finished parents and descendants may be archived independently:
the append-only ledger retains their hierarchy provenance.

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
frame, and leaves removal to the runner EXIT path. A retained record with
`runnerGone:true` has no process to signal: the same request appends the
user-close boundary and removes any stale unreachable map entry instead.
Responses:

- known live entry, already-exited retained entry, or retained
  `runnerGone:true` record: `200 {"ok":true}`. Re-ending an already-exited
  entry is idempotent and does not append a user-kill boundary after its exit.
- unknown entry: `404 {"ok":false}`
- a known entry whose attribution, durable boundary, or runner control cannot
  be completed safely: `409 {"ok":false,"error":"<instructional message>"}`.
  The daemon log retains the underlying error; the response directs the caller
  to establish current session status before retrying.

### `POST /api/sessions/end-batch`

Auth required. Body is:

```json
{"ids":["<session id>","<session id>"],"reason":"<operator text>","operationId":"<correlation id>","force":false}
```

At least two non-empty live, already-exited retained, or retained
`runnerGone:true` session IDs are required. The request is rejected before
mutation if a target is missing or the manager's mass-end safety guard requires
explicit `force:true`. On success the daemon records one attributed operation
for the batch and returns
`{"ok":true,"ids":[...]}`. A safety-guard refusal or another failure to
complete the requested end operation returns `409` with `ok:false` and an
instructional error; the exact underlying error is retained in the daemon log.

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

A pinned session sorts first in every listing and marks a live workbench that
future automatic cleanup policy must leave alone. Sessions has no automatic
terminator today, and a pin is never exempt from an explicit end by the user.
The operation starts, stops, and otherwise touches nothing about the runner.

The pin is daemon-owned end to end: a runner has no way to express it, so a
runner metadata write preserves whatever the daemon last stored. Unknown
sessions return 404. Ended records return 409 because a pin marks a live
workbench; archive is the organizational verb for ended records. Any method
other than `PUT` returns 405.

### `GET /api/sessions/:id/tags`

Auth required. Returns `{"tags":{"<key>":"<value>"}}` from the live session or,
for an ended one, from its stored metadata. An unknown session or missing
metadata file is `404 {"error":"unknown session","id":"<id>"}`; a metadata
read failure is `500 {"error":"<message>","id":"<id>"}` so a caller does not
retry forever against a lane that exists.

### `PUT /api/sessions/:id/tags`

Auth required. Body `{"tags":{"<key>":"<value>"}}` replaces the whole tag set;
an empty or absent object clears it. Keys are trimmed and lowercased and must
use letters, numbers, `.`, `_`, or `-` (at most 64 characters); values are
trimmed, must be non-empty, and are at most 256 characters; at most 32 tags.
Returns `200 {"tags":{...}}` with the normalized set, which a live runner also
adopts immediately. Validation and persistence failures are 400; an unknown
session or missing metadata file is 404.

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

A failed Rich provider turn appends a normalized system record after its
provider-specific failure is observed:

```json
{"type":"system","subtype":"provider_fault","kind":"provider-unavailable","detail":"Codex API unavailable (503, overloaded)","status":503,"provider":"codex"}
```

`status` is omitted when no HTTP status was available. This record is separate
from assistant prose and is rendered by transcript clients as an error. A later
successful turn clears the session's `failureKind`, `failureDetail`,
`failureProvider`, and `failureAt`; it does not delete append-only fault history.

For `provider-unavailable` and `rate-limited`, the Rich runner retains that
turn's exact input and schedules five attempts after 30 seconds, 1 minute,
2 minutes, 5 minutes, and 5 minutes. A rate-limit message such as
`try again in 42s` raises the applicable delay to that hint, capped at 5 minutes.
Each scheduled attempt appends:

```json
{"type":"system","subtype":"provider_retry","attempt":2,"max":5,"nextAt":1788465600000}
```

New user input replaces the retained failed turn and cancels its schedule;
interrupt, End, and the stop route also cancel it. Authentication and other
failures remain failed without automatic retries. No per-attempt notification
is sent; exhausting the schedule sends one provider-unavailable notification.

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

### `GET /api/sessions/:id/wait` and `GET /api/sessions/:id/wait-state`

Auth required. Both paths return the same observational facts a client needs
in order to wait on a session without scraping its terminal:

```json
{"session":"<id>","cwd":"/absolute/workspace","working":false,"source":"structured"}
```

`source` is `structured` when the Claude or Codex event classifier supplies
`working` and `heuristic` when it comes from raw terminal activity; neither
claims more than that evidence. An unknown session is
`404 {"error":"unknown session","id":"<id>"}`; an exited one is
`409 {"error":"session exited","id":"<id>"}`. Other methods return 405.

### `GET /api/sessions/:id/verdict`

Auth required. `:id` must match `[A-Za-z0-9][A-Za-z0-9._-]{0,127}` (400
otherwise) and is not required to name a live session. Returns the newest
decodable verdict record for that lane:

```json
{"schemaVersion":1,"verdict":"pass","findings":[{"severity":"error","title":"...","detail":"...","file":"...","line":12}],"meta":{},"seq":3,"emitted_at":"<RFC3339>"}
```

`findings` and `meta` are omitted when empty; `detail`, `file`, and `line` are
omitted per finding when absent. `skipped_records` appears, nonzero only, when
the torn-record policy skipped records while answering, meaning the returned
verdict is the newest usable one rather than provably the newest. No record is
`404 {"error":"no verdict","id":"<id>"}`; a store that cannot be opened is 500
and any other read error is 400.

### `POST /api/sessions/:id/verdict`

Auth required. Body is one verdict document decoded strictly: unknown fields,
duplicate keys, and trailing content are rejected. `schemaVersion` must be
`1`, `verdict` must be a non-empty string, each finding needs a non-empty
`severity` and `title`, and `line`, when present, must be positive. An invalid
document is `400 {"error":"<message>"}`. Success appends the record and returns
201 with the stored record, `seq` and `emitted_at` assigned by the daemon.
Append failures are 500; other methods return 405.

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

Auth required. Body is
`{"data":"<one complete composer message>","operation_id":"<UUID v4>"}`.
`operation_id` is optional for older callers; the daemon generates one when it
is absent. The daemon durably records the operation before runner input,
serializes logical submissions across callers, writes `data`, waits for the
provider TUI to settle, and writes carriage return as a second PTY frame. This
is the message boundary used by the CLI and desktop composer: concurrent agents
cannot interleave one message's text with another message's Enter. Terminal
keys and paste-without-submit continue to use `/input`.

The response is a delivery receipt with `operation_id`, `session_id`, `status`,
`delivered`, `retry`, `reason`, `duplicate`, `created_at_ms`, and
`updated_at_ms`. `status` is one of `accepted`, `not-delivered`, `unknown`, or
`text-delivered`. Retrying the same operation id with the same target and
content reads the original receipt without sending another message, including
after a daemon restart. Reusing it for different content or a different target
returns 409. Receipts store only the target id, byte count, and SHA-256 digest;
message text is not copied into the receipt directory.

An unknown/exited target is `not-delivered` with `retry:true`. A failure after
runner input may have happened is `unknown` or `text-delivered` with
`retry:false`; an automated caller must inspect the receipt instead of creating
a new operation. This conservative boundary prevents a lost HTTP response from
turning into a duplicate provider writer.

### `POST /api/sessions/:id/approve`

Auth required. Answers the permission a Rich Claude or Codex session is holding open.
A user-created session started with **Ask me**, or a lane that inherits the
person's permissions instead of running autonomously, asks before it runs a
command, changes files, or takes more access. Codex's **Ask me** choice is its
untrusted policy in a workspace-write sandbox. The runner holds the request,
the session reads `needs-input` with `idleDetail` set to `Allow? <summary>`, and
the session object carries the `pendingApproval` described in `SessionInfo`.
Body is `{"decision":"allow"|"allow-session"|"deny"}` with an optional `id`
that must match the pending approval. `allow-session` lets the same kind of
request through for the rest of the session; `deny` refuses it and the session
continues without it.

The optional `X-Sessions-Creator-Session` header attributes the decision to a
lane, and the runner records an `approval_resolved` event with that id in
`by` (empty when a person decided). Responses:

- `200 {"ok":true,"id":"<session>","decision":"<decision>","approval":{...}}`
- `400` for an unknown decision or a session that is not Rich
- `404` for an unknown session
- `409` when nothing is waiting, the id does not match, or the session ended
- `501` when the daemon cannot route approvals

### `POST /api/sessions/:id/retry`

Auth required. Runs the pending automatic retry immediately, or runs the last
retained failed Rich Claude/Codex turn after its automatic schedule exhausted.
Success is 200 with the current bare `SessionInfo`. A PTY session, a session
with no failed turn, an active or ended session, or an older runner that cannot
accept retry controls returns `409 {"error":"<sentence explaining why>"}`.
Unknown session is 404. Other runner-control failures are 502.

### `POST /api/sessions/:id/retry/stop`

Auth required. Cancels the runner-owned automatic retry schedule without
clearing the provider fault or its retained failed input. Success is 204 with no
body. Nothing scheduled, a PTY or ended session, and an older runner return 409
with an instructional error; unknown session is 404 and other control failures
are 502.

### `GET /api/message-deliveries/:operation-id`

Auth required. Returns the latest durable receipt for a composer submission.
A record left `pending` by a daemon crash is exposed as `unknown` with
`retry:false`, because Sessions cannot prove whether runner input happened
before the crash. A missing operation is 404. This endpoint never returns the
message body or its content digest.

### `POST /api/sessions/:id/upload`

Auth required. The request body is raw bytes, not JSON. Optional header
`X-Sessions-Filename` defaults to `file`; `Content-Type` is accepted but not used
in the saved name or response. The filename is reduced to its basename,
characters outside `[A-Za-z0-9_. -]` become `_`, and the result is limited to
96 characters. The stored name is `<stem>-<first 8 chars of random UUID><ext>`.

The destination is the `uploads/` directory under the daemon's state root —
`~/.local/state/sessions/uploads/` on Unix and the same child of
`%LOCALAPPDATA%\Sessions\state` on Windows. An explicitly set
`SESSIONS_STATE_DIR` moves it, so a scratch daemon does not write into the
installed daemon's uploads; see `state-dir.md`. Responses:

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

### `GET /api/recovery`

Auth required. Opens the append-only creator ledger and returns the recovery
report that `sessions doctor` reads:

```json
{"generatedAtMs":1750000000000,"lanes":[{"id":"<Sessions id>","tool":"claude","cwd":"/absolute/workspace","providerUuid":"<provider conversation UUID>","class":"...","anomalies":[],"reality":{"processAlive":false,"managerVisible":false,"conversation":"<provider file>","transcriptMirror":"<Sessions copy>","conversationRecoverable":true}}],"plan":{/* ledger RecoveryPlan */}}
```

Each lane also carries, when known, `name`, `profile`, `config_dir`,
`createdAtMs`, `lastEventAtMs`, `lastActivityAtMs`, `lastHumanInputAtMs`,
`lastProviderActivityAtMs`, `lastActivitySource`, `reopenedAs`, and
`resumeArgv`. In `reality`, `conversation` is the provider's own file, which is
what makes a native resume possible; `transcriptMirror` is Sessions' copy,
which makes the conversation readable and recoverable through Sessions but
never makes a native resume work; `conversationRecoverable` reports whether
either exists; `probeErrors` lists probe failures and is omitted when empty.
A ledger open or report failure is `500 {"error":"<message>"}`. Other methods
fall through to the 404 body.

### `POST /api/recovery/reopen`

Auth required. Body `{"force":false}`; both the body and `force` are optional.
Rebuilds the recovery report and reopens lost lanes, creating at most one live
lane per provider UUID, serialized against the other recovery mutations so two
concurrent requests cannot launch the same conversation twice. Returns 200:

```json
{"ok":true,"outcomes":[{"sourceLaneId":"<ended id>","name":"...","providerUuid":"...","status":"reopened","newLaneId":"<new id>"}]}
```

Every lost lane appears in `outcomes`, including refusals, so an unsafe
candidate is never silently omitted. `status` is `reopened`,
`skipped-live-provider`, `blocked`, or `failed`; `name`, `providerUuid`,
`newLaneId`, and `error` are omitted when empty. A successful reopen also
clears any paused-after-reboot restore record for the source lane. Invalid
JSON is 400; a ledger open or report failure is 500.

### `POST /api/recovery/adopt`

Auth required. Resolves one explicit provider conversation and creates its
successor through the normal write-ahead session boundary:

```json
{"target":"<provider UUID or conversation path>","sourceSessionId":"<optional ended Sessions id>","force":false,"claudePermissionMode":"<optional Claude mode>"}
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

`claudePermissionMode` is an optional per-launch Claude override using the same
typed values as `POST /api/sessions` (`inherit`, Claude's constrained modes, or
`bypassPermissions`). It is accepted only for a same-provider native Claude
resume. Transcript-only restoration, cross-provider continuation, Codex, and
repair reject it rather than pretending to alter a runtime they cannot control.
The CLI maps `sessions resume ID --permissions full` to
`bypassPermissions`; an existing constrained process is not silently mutated.

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

### Cross-provider continuation jobs

`POST /api/recovery/continuation/preview` is an authenticated dry run. Its body
selects one exact conversation and destination provider, with an optional tail:

```json
{"target":"<provider conversation id>","historyId":"<optional history id>","sourceSessionId":"<optional ended Sessions id>","destinationProvider":"claude","messageLimit":40}
```

It reads only user and assistant messages and creates no session. The response
contains `conversation`, source and destination providers, total and selected
message counts, Unicode character count, `estimatedTokens` (characters divided
by four, rounded up), `thresholdTokens`, `limited`, and `sourceUntouched`.
`messageLimit` selects the last N messages; zero or omission selects all. The
default threshold is 60,000 and can be configured with
`SESSIONS_CONTINUATION_TOKEN_THRESHOLD`.

`POST /api/recovery/continuation/jobs` accepts the same selection plus `model`,
optional `effort`, and `confirmWholeHistory`. Above the threshold, an unlimited
request requires `confirmWholeHistory:true`. It returns `202` with a job whose
status is `running`, `succeeded`, `canceled`, or `failed`. `events` is an
ordered list of `exporting-history`, `creating-session`, `provider-starting`,
and `first-reply` stages. The job also reports the chosen model, new `laneId`,
preview, error or warning, and current provider-fault fields when present.

`GET /api/recovery/continuation/jobs/:id` returns the latest job snapshot.
`DELETE` cancels it. If a destination session exists, cancellation requests a
normal session end and does not report `canceled` until the daemon observes it
ended. The source record is not marked as continued until the destination's
first reply completes.

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

### `GET /api/usage`

Auth required. Scans local Claude and Codex provider history and returns a
token and cost report. Query parameters: `group` (`daily`, `weekly`,
`monthly`, `session`, `tag`, `provider`, or `model`), `mode` (`auto`,
`calculate`, or `display`), `provider` (`claude` or `codex`), `dimension`
(mandatory when `group=tag`), `since` and `until` as local `YYYY-MM-DD` dates
(`until` is inclusive and `since` must not be after it), and `events=1` to
include per-event identities. Any other value is `400 {"error":"<message>"}`.
Returns 200:

```json
{"schemaVersion":1,"machine":"<machine id>","generatedAt":"<RFC3339>","group":"daily","mode":"auto","pricing":{"source":"...","revision":"...","url":"...","note":"..."},"scan":{"filesSeen":0,"filesRead":0,"linesRead":0,"entriesSeen":0},"rows":[/* ReportRow */],"totals":{/* ReportRow */},"eventsIncluded":false}
```

`dimension` is present only when set and `events` only with `events=1`. A
`ReportRow` carries `key`, optional `start`, `provider`, `sessionId`,
`providerSessionId`, and `tags`, then `models`, `tokens` (`inputTokens`,
`outputTokens`, `cacheCreationTokens`, `cacheReadTokens`, `reasoningTokens`),
`costUSD`, `recordedCostUSD`, `calculatedCostUSD`, `entries`, and
`missingPricingEntries`. Report failures are 500; other methods return 405.

### `GET /api/backup/status`

Auth required. Returns the non-secret backup configuration and last-push
counters:

```json
{"enabled":true,"encrypt":true,"key_path":"...","project":"...","interval":"1h","last_push_at":"<RFC3339>","last_push_count":0,"last_push_skipped":0,"last_push_pending":0,"last_session_count":0}
```

`key_path`, `project`, `interval`, and `last_push_at` are omitted when empty.
Every `/api/backup/*` route returns
`503 {"error":"backup home is unavailable"}` when the daemon's state root does
not belong to the current user's home. A status read failure is 500.

### `POST /api/backup/now`

Auth required. Runs one backup push immediately and returns 200:

```json
{"pushed_at":"<RFC3339>","uploaded":3,"skipped":1,"session_count":4,"unresolved":1,"unresolved_sessions":[{"id":"<Sessions id>","reason":"..."}],"manifest_path":"..."}
```

`unresolved_sessions` (omitted when empty) names each session this push could
not back up, such as a live transcript that grew mid-read or a single failed
upload; the rest of the run continues and the next push retries them, so a
partial push is never reported as complete. A push that fails as a whole is
`502 {"error":"<message>"}`.

### `POST /api/backup/reload`

Auth required. Re-reads the backup configuration and restarts the periodic
push schedule, returning `200 {"ok":true}`. A configuration error is 400. Any
other method on the three backup paths returns 405.

### `GET /api/daily`

Auth required. Query `date` is a local `YYYY-MM-DD` day and defaults to today;
any other value is `400 {"error":"date must use YYYY-MM-DD"}`. Returns 200:

```json
{"date":"2026-09-01","timezone":"<local zone name>","activities":[/* DailyActivity */],"usage":{/* usage ReportRow totals for the day */}}
```

`activities` combines Sessions-managed lanes with provider conversations
observed outside Sessions (`"source":"provider"`, `"provenanceStatus":"Outside
Sessions"`), sorted by `lastActivityAt` then `id`. Usage or provider-log scan
failures are 500; other methods return 405. This route makes no model call and
does not write a narrative document.

## Go runtime extension: tailnet discovery approval

The native client discovers online peers from the local `tailscale status
--json` result and probes their unauthenticated `/api/health` route. A healthy
peer is only a discovery candidate; it grants no session access.

`POST /api/tailnet/access/request` is exempt from bearer authentication only
when the immediate connection came through local Tailscale Serve with a
verified Tailscale identity. It rejects any request carrying an `Origin` header
and requires `Content-Type: application/json`, so browser JavaScript—including
the allowed Somewhere origins and daemon-served same-origin UI—cannot
participate in native pairing or read its credential. It accepts:

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

Host approval lives on `GET /api/access/requests` and
`POST /api/access/requests/:id`, documented below; the collection covers
tailnet and nearby (LAN) requests alike. The original
`/api/tailnet/access/requests` and `/api/tailnet/access/requests/:id` paths
are **deprecated** aliases served by the same handler: they behave
identically, but every response on them carries `Deprecation: true` and
`Link: </api/access/requests...>; rel="successor-version"` (RFC 8594). They
remain only until every shipped client speaks the canonical path.

The requester polls `POST /api/tailnet/access/claim` through the same verified
Tailscale Serve identity with:

```json
{"request_id":"<UUID>","request_secret":"<random secret>"}
```

Pending claims return 202, denied claims 403, and expired or mismatched claims
410. An accepted claim creates a two-minute pending per-device bearer
credential plus the daemon's stable machine ID/name and its currently available
`lan_endpoint`, `tailnet_endpoint`, and `tailnet_ip_endpoint`, using the same
response shape as `POST /api/lan/access/claim`. Repeated claims return the same device
ID and token, so a lost 201 response is safe. The first authenticated API request with
that token durably acknowledges it; until then it is hidden from the device
list and cannot authorize after its deadline. Issuance starts its own two-minute
acknowledgement window even when host approval happened near the original
request deadline. Thus a response lost before the client receives the token
cannot leave a permanent active credential. Expired pending device records are
purged when the device store is next loaded. Pending request state itself
disappears on daemon restart or after its current deadline; the client can
safely request again.

`sessions pair` is the consent-by-possession path when both devices are in
front of the user. Its application link includes every currently available
LAN, Tailscale HTTPS, and direct Tailscale-IP endpoint; claiming it needs no
request/accept decision.

Several routes in this section are **local-principal only**
(`requireLocalPrincipal` in `server_routes.go`): only a direct loopback peer
qualifies. A caller authorized by the master token from another host, a paired
device token, or the `open` sentinel receives
`403 {"error":"<operation> is available only on this machine"}` even though it
authenticated.

### `POST /api/lan/access/request`

The nearby counterpart of `POST /api/tailnet/access/request`. It is dispatched
before bearer authentication and answers on the user-enabled LAN listener, the
automatic direct Tailscale-IP listener, and the main listener for a true
loopback peer. Other main-listener peers receive
`403 {"error":"nearby access is available only on this machine's trusted LAN listener or local loopback"}`.
The LAN peer must be a private, non-loopback IPv4 address; the Tailscale peer
must be in `100.64.0.0/10` and must have arrived on that exact listener. The
request must carry no `Origin` header (403) and exactly one
`Content-Type: application/json` (415). Body
`{"client_id":"<lowercase v4 UUID>","name":"<device name>"}` returns 202 with
the same `{"request_id","request_secret","expires_at","status":"pending"}`
shape as the tailnet route; the request is recorded with `transport` `nearby`
or `tailnet-ip`, the peer address, and a synthetic login scoped to that
transport and address. An invalid
client id or device name is 400 and a full queue (64 pending) is 429. Other
methods return 405.

### `POST /api/lan/access/claim`

Same listener, peer, and content-type gates as
`POST /api/lan/access/request`. The request/accept body is
`{"request_id":"<UUID>","request_secret":"<secret>"}` and rejects every
browser `Origin`. A pending request is
`202 {"status":"pending"}`, a denied one
`403 {"status":"denied","error":"<message>"}`, and an expired or mismatched one
`410 {"status":"expired","error":"<message>"}`. Acceptance returns 201 with
`{"device_id","token","name","machine_id","machine_name","lan_endpoint","tailnet_endpoint","tailnet_ip_endpoint"}`, the
pairing-claim shape, under the same two-minute acknowledgement rule
described above. An unavailable machine identity is 503.

Alternatively, `{"ticket":"<id>.<secret>","name":"<device name>"}` claims a
one-time pairing ticket and immediately returns the same 201 credential shape;
these credentials are durable immediately. This form is allowed from native
clients without `Origin` and from the daemon's own same-origin `/pair/<ticket>`
page. Any other browser origin is 403. An invalid, used, expired, or revoked
ticket returns 410 with the sentence “Pairing ticket is invalid, expired, or
already used. Run `sessions pair` to create a new one.”

### `POST /api/lan/access/account-claim`

Public bootstrap for a signed-in device; bearer authentication is not yet
available because this route issues that device's credential. The JSON body is
`{"machine_id","device_id","timestamp","nonce","signature"}`. The signature
is unpadded base64url Ed25519 over this concatenation:

```text
machine_id + device_id + timestamp + nonce + "POST" +
"/api/lan/access/account-claim" + hex(sha256(unsigned_claim_json))
```

`unsigned_claim_json` is compact JSON with the first four body fields in the
order shown. The target host fetches `device_id` with its own Somewhere token;
the owner-scoped result, never a caller-supplied key, supplies the public key.
The target ID must be this daemon, the timestamp must be within five minutes,
and a `(device_id, nonce)` pair can succeed only once during that window.
Invalid signatures, stale claims, replay, and devices absent from this host's
account all return 403 without issuing a credential. A directory failure is
502; a host without fleet account support is 503; non-JSON is 415; other
methods return 405.

Success creates the same two-minute-pending device record as an accepted
request, writes `access granted to <device> via account` to the daemon log, and
returns the normal 201 pairing-claim shape. The caller must make one
authenticated request before the acknowledgement deadline.

### `GET /api/access/requests`

Auth required, local-principal only. Returns the pending tailnet and nearby
requests, oldest first:

```json
{"requests":[{"request_id":"<UUID>","client_id":"<UUID>","name":"MacBook Pro","login":"<Tailscale login or nearby:<address>>","user_name":"<Tailscale display name>","transport":"tailnet","address":"<peer IPv4>","created_at":"<RFC3339>","expires_at":"<RFC3339>","status":"pending"}]}
```

`user_name` and `address` are omitted when empty; `transport` is `tailnet`,
`tailnet-ip`, or `nearby`. Decided and expired requests are not listed, and the
request secret is never included. Other methods return 405.

### `POST /api/access/requests/:id`

Auth required, local-principal only. Body `{"decision":"accept"}` or
`{"decision":"deny"}`; returns 200 with the decided request in the listing
shape and `status` `accepted` or `denied`. An unknown, expired, or malformed
request id, or a decision other than those two words, is
`404 {"error":"access request is invalid or expired"}`; a request that was
already decided is 400; invalid JSON is 400. An empty or nested id is
`404 {"error":"access request not found"}`. Other methods return 405.

### `GET /api/lan`

Auth required. Returns the state of the user-enabled plaintext LAN listener:

```json
{"enabled":false,"url":null,"bonjour":{"advertised":false,"service":"_sessions._tcp"},"permission":{"status":"not-yet-asked"}}
```

`url` is the `http://<address>` of the running LAN listener or `null`;
`bonjour.error` carries the last advertisement error and is omitted when
empty. The `_sessions._tcp` TXT record always carries `lan=<origin>` and adds
`tailnet=<HTTPS origin>` and `tailnet-ip=<HTTP CGNAT origin>` whenever
Tailscale reports them; all three are hints and the client must still verify
health and obtain a device credential. `permission.status` is the daemon's last
observed Local Network state:
`granted`, `denied`, or `not-yet-asked` on Darwin and `not-required` elsewhere.
There is no permission preflight. A denied state additionally includes
`"reason":"local-network-permission"` and
`"message":"macOS has not allowed Sessions to use the local network. System Settings › Privacy & Security › Local Network › turn on Sessions."`.
Granted and denied observations persist across daemon restarts and change when
a later nearby operation proves the opposite state.

### `POST /api/lan`

Auth required, local-principal only: opening the LAN listener and its Bonjour
advertisement is a separate capability from remote API access and is never
enabled as a side effect of another route. Body `{"enabled":true}` or
`{"enabled":false}`; a missing or non-boolean value is
`400 {"error":"enabled must be true or false"}`. The choice is persisted in
daemon settings and the resulting state is returned in the `GET /api/lan`
shape. A listener that cannot be started or stopped is
`409 {"error":"<message>"}`. Other methods return 405.

### `GET /api/lan/discover`

Auth required, local-principal only. sessionsd performs a Bonjour browse and
then verifies every candidate with `GET /api/health`; the calling CLI or app
does not access the LAN. Optional `timeout` is a positive Go duration no longer
than 15 seconds and defaults to `3s`. Success is:

```json
{"machines":[{"name":"Mac mini","hostname":"mini.local.","endpoint":"http://192.168.1.24:8787","lan_endpoint":"http://192.168.1.24:8787","tailnet_endpoint":"https://mini.example.ts.net","tailnet_ip_endpoint":"http://100.100.20.30:8787","address":"192.168.1.24","port":8787,"transport":"nearby","version":"v0.2.27","os":"darwin","arch":"arm64","sessions_loaded":2,"reachable":true}],"warning":"Nearby access uses unencrypted HTTP. Connect only on a private network you trust."}
```

An invalid timeout is 400. A browse failure is 502, except that a Darwin Local
Network denial is 403 with
`{"error":"macOS has not allowed Sessions to use the local network. System Settings › Privacy & Security › Local Network › turn on Sessions.","reason":"local-network-permission"}`.
An empty Darwin browse while this daemon is itself advertising is classified
the same way. Other empty results are 200 with an empty `machines` array.
Other methods return 405.

### `POST /api/lan/connect`

Auth required, local-principal only. sessionsd owns the complete outbound
request/claim/credential-verification sequence. Body:

```json
{"lan_endpoint":"http://192.168.1.24:8787","tailnet_endpoint":"https://mini.example.ts.net","tailnet_ip_endpoint":"http://100.100.20.30:8787","client_id":"<lowercase v4 UUID>","name":"MacBook Pro","timeout":"10m"}
```

The endpoint fields are tried in LAN, `.ts.net` HTTPS, direct Tailscale-IP HTTP
order after an unauthenticated health probe; legacy `endpoint` is still
accepted and is assigned to its matching kind. `timeout` is optional, positive,
at most ten minutes, and defaults to ten minutes. The request remains open while
the other machine's user accepts or denies it. Acceptance returns 201:

```json
{"claim":{"device_id":"<device UUID>","token":"<credential>","name":"MacBook Pro","machine_id":"<machine id>","machine_name":"Mac mini","lan_endpoint":"http://192.168.1.24:8787","tailnet_endpoint":"https://mini.example.ts.net","tailnet_ip_endpoint":"http://100.100.20.30:8787"},"endpoint":"https://mini.example.ts.net","transport":"tailnet"}
```

The credential crosses only this authenticated loopback response; the CLI
stores it in the existing separate owner-readable credential file. Invalid
input is 400. A peer denial or expired request is 502. A Darwin Local Network
denial is the same 403 error and reason as `GET /api/lan/discover`. Other
methods return 405.

When `ticket` is present, the daemon skips the request/accept exchange, probes
the endpoint fields in the same order, posts the ticket to the first reachable
peer's `/api/lan/access/claim`, verifies the issued credential against
`/api/machine`, and returns the normal 201 response. This is the local-daemon
path used by `sessions machines connect <pairing-link>`.

### `POST /api/pair/ticket`

Auth required, local-principal only. Body
`{"name":"<device name>","ttl":"<Go duration>"}`, with the name trimmed and
truncated to the device-name limit. `ttl` defaults to ten minutes, must be
positive, and cannot exceed ten minutes. Mints a single-use pairing ticket and
returns 201:

```json
{"ticket":"<id>.<32-byte-base64url-secret>","ticket_id":"<id>","expires_at":"<RFC3339>","link":"sessions://pair?host=<encoded-origin>&host=<encoded-origin>&t=<encoded-ticket>","fallback":"https://<machine>.ts.net/pair/<ticket>","endpoints":[{"endpoint":"<origin>","transport":"lan|tailnet|tailnet-ip"}]}
```

Endpoint rows and repeated `host` parameters preserve LAN, Tailscale HTTPS,
then direct Tailscale-IP order and omit unavailable kinds. `fallback` uses the
Tailscale HTTPS origin when present and the trusted-LAN HTTP origin otherwise.
The ticket exists only in daemon memory; it is removed after one successful
claim, explicit revocation, expiry discovery, or daemon restart. A missing
endpoint is 409, invalid TTL is 400, a random-source failure is 500, and other
methods return 405.

### `DELETE /api/pair/tickets/:id`

Auth required, local-principal only. Revokes one outstanding ticket and returns
`200 {"ok":true,"ticket_id":"<id>"}`. An empty, nested, unknown, used,
expired, or already-revoked ID returns the same instructional 410 pairing
sentence as a failed claim. Other methods return 405.

### `GET /pair/:ticket`

Public browser fallback for a ticket minted by the same daemon. It validates
that the ticket is one path segment, then returns 303 to `/#pair=<ticket>` so
the root-relative application assets load normally and the ticket moves into a
fragment that is not sent with asset requests. The application scrubs the
fragment before claiming it against that same origin. An invalid path is 404;
other methods return 405.

### `POST /api/pair/claim`

Deprecated compatibility alias for native clients shipped with the original
LAN-only pairing feature. Body `{"ticket":"<id>.<secret>","name":"<device
name>"}` and credential/error responses match the ticket form of
`POST /api/lan/access/claim`. New clients use the latter route.

### `GET /api/devices`

Auth required, local-principal only. Returns
`{"devices":[{"device_id":"<UUID>","name":"...","created_at":"<RFC3339>","last_used_at":"<RFC3339>"}]}`
sorted by `created_at`, then `device_id`. Token hashes are never returned, and
a device whose credential is still pending acknowledgement is hidden until its
first authenticated request. A store read failure is 500; other methods return
405.

### `DELETE /api/devices/:id`

Auth required, local-principal only. Revokes one paired device's bearer
credential and returns `200 {"ok":true,"device_id":"<id>"}`. An empty or
nested id, or an unknown device, is `404 {"error":"device not found"}`; a
store write failure is 500; other methods return 405.

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

The user-facing app on the daemon host submits both choices, for example
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

Client-only phone apps read this machine-level state but do not present the
host onboarding flow or call `PUT /api/onboarding`; their host-owned settings
controls are read-only.

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
The same route accepts `claudePermissionMode` for a same-provider Claude
continuation. The CLI equivalent `sessions resume <history-id> --permissions
full` starts the successor with Claude's exact skip-permissions flag. This is a
new process bound to the same provider conversation, because Claude's live
permission-mode cycle cannot elevate a process that was launched constrained.

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
artifact and returns at most its latest 400 normalized messages. An optional
`limit=1..400` lowers the message count for a particular interactive view;
invalid limits return 400 rather than being ignored. The additive
response field `"truncated":true` appears when either bound removed older
content; it is omitted for a complete preview. This bound does not change the
deliberate full-history JSON/text response or `/api/history/<id>/raw` download.
The distinct path is intentional: an older runtime returns 404 instead of
silently ignoring a query parameter and sending an unbounded transcript.

### `GET /api/errors`

Auth required. Returns the daemon's append-only error feed for integrations:

```json
{"schemaVersion":1,"errors":[{"seq":1,"ts":"<RFC3339>","kind":"daemon_error","session_id":"<Sessions id>","summary":"...","detail":"...","machine":"<machine id>"}],"nextSeq":2}
```

`since=<sequence>` returns only events whose `seq` is greater than that value;
anything but a non-negative integer is
`400 {"error":"since must be a non-negative integer sequence"}`. `nextSeq` is
the value to pass as the next `since`. `session_id` is omitted when an event is
not tied to a session. `skipped_records` (nonzero only) counts undecodable
records, and `truncated_before` (nonzero only) is the lowest sequence still
retained after older events aged out, so a `since` below it means a gap the
caller can fill from the log file. A feed read failure is 500. Any method other
than `GET` on `/api/errors` or any `/api/history` path returns 405.

### `GET /api/history/:id/source`

Auth required. Describes where one history conversation's bytes come from
without reading them:

```json
{"schemaVersion":1,"session":{/* history session summary */},"source_kind":"provider-jsonl","source_path":"/absolute/provider/file.jsonl","raw_bytes":1234,"raw_available":true,"text_available":true}
```

`source_kind` is `provider-jsonl`, `prompt-index`, `sessions-mirror`, or
`missing`; `source_path` and `raw_bytes` are omitted when unknown.
`mirror_damaged` and `mirror_detail` appear only when the conversation is
served from Sessions' own mirror and that mirror records having stopped storing
provider records; both are absent for a non-mirror source or an unknown mirror
health. An unknown id is
`404 {"error":"history session not found","id":"<id>"}`; any other failure is
recorded on the error feed as a `daemon_error` and returned as 500.

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
