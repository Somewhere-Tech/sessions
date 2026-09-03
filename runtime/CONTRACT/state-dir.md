# sessionsd state and discovery contract

This records the paths and lifecycle used by the shipped Go runtime. The
current layout is split: runner artifacts live in a `runners/`
subdirectory by default, while auth, push, uploads, and idle state live at the
root.

## Default layout

```text
~/.local/state/sessions/
├── token
├── open
├── vapid.json
├── push-subscriptions.json
├── delivery-operations/
│   └── <operation-id>.json
├── uploads/
│   └── <sanitized-stem>-<8 UUID chars><ext>
├── idle/
│   └── <session-id>
└── runners/
    ├── <session-id>.json
    ├── <session-id>.sock
    ├── <session-id>.events
    ├── <session-id>.events.tmp       # transient trim file only
    ├── <session-id>.log
    ├── <session-id>.keepalive.json         # boot-scoped restart permit
    ├── <session-id>.restore-pending.json   # paused automatic restore reason
    ├── <session-id>.transcript.jsonl      # Go runtime only
    └── <session-id>.transcript.meta.json  # Go runtime only

~/Library/LaunchAgents/
└── tech.somewhere.sessions.runner.<session-id>.plist
```

The Unix root above is the platform user state root. Windows uses
`%LOCALAPPDATA%\Sessions\state` with the same children, and the launch-agent
tree has no Windows equivalent. Derive it from `state.UserStateRootFor` rather
than rebuilding either layout by hand.

`SESSIONS_STATE_DIR` names the runner artifact directory passed to runners as
`RUNNER_STATE_DIR`. For example, `SESSIONS_STATE_DIR=/tmp/ct-state` places
`<id>.json/.sock/.events/.log` directly in `/tmp/ct-state`, not
`/tmp/ct-state/runners`.

Setting it also derives a separate *state root* — the parent when the configured
directory is named `runners`, otherwise the directory itself — so a scratch
daemon never reads the installed daemon's credentials or ledgers
(`runtime/internal/state/config.go` `stateRootsFromEnv`). These follow the
override:

- `token` and `open`;
- `uploads/` (`runtime/internal/api/files.go` `uploadsDir`), matching how recap,
  usage, and integration-error state already resolve;
- `delivery-operations/`, the content-free idempotency receipts for composer
  submissions;
- `recaps/`, `usage.sqlite3`, and `errors.jsonl`.

These do **not** follow it, and resolve from the user state root, which is
always derived from the home directory: `settings.json`, `machine-id`,
`clients.json` and `clients/`, `devices.json`, `profiles/`, `search-index.db`,
VAPID/subscriptions, and the `idle/` sentinels. Neither does the lane ledger,
which has its own `SESSIONS_LEDGER_PATH` override, nor
`~/Library/LaunchAgents`. A safe isolated test must therefore set `HOME`,
`SESSIONS_STATE_DIR`, and `SESSIONS_LEDGER_PATH` together, plus `SESSIONS_PORT`
so it does not contend for the installed daemon's default `8787`.

One consequence of relocating uploads: `POST /api/sessions/:id/upload` still
requires the resolved uploads directory to be inside the daemon's home
directory and answers
`403 {"error":"upload directory outside home"}` otherwise. A scratch
`SESSIONS_STATE_DIR` outside its scratch `HOME` therefore cannot accept uploads.

## Files

### `token`

Exactly 64 lowercase hexadecimal characters representing 32 random bytes. It is
read with UTF-8 and trimmed. A missing, unreadable, truncated, uppercase, or
otherwise malformed value is replaced on the next `getAuthToken()` call.

The root directory is created recursively with requested mode 0700, and a new
token is written with mode 0600. Existing directory/file modes are not repaired.
Token creation is lazy: health/static requests alone do not create it. A
protected HTTP request creates it unless the `open` check bypasses auth first;
every WS auth check calls the token getter even in open mode.

### `open`

Only existence matters; contents and mode are not read. Its presence bypasses
HTTP and WS token comparison but does not bypass Origin checks. Removing it
immediately restores auth on later requests.

### `vapid.json`

Sessions-printed JSON with exactly the generated key fields used by web-push:

```json
{
  "publicKey": "<non-empty string>",
  "privateKey": "<non-empty string>"
}
```

Malformed/missing data is replaced lazily when VAPID keys are next requested.
The file is written mode 0600 under the fixed root.

### `push-subscriptions.json`

Sessions-printed JSON array. Invalid top-level data reads as an empty list;
invalid elements are filtered. Each valid element is:

```json
{
  "endpoint": "<non-empty string>",
  "expirationTime": null,
  "keys": {"p256dh":"<non-empty string>","auth":"<non-empty string>"}
}
```

`expirationTime` may be absent, null, or numeric. Writes use mode 0600.
Subscribe replaces by endpoint; unsubscribe filters by endpoint. Push responses
404/410 also remove their stale subscriptions.

### `uploads/*`

Uploaded raw request bodies for known sessions. The uploads directory is created
recursively with requested mode 0700 and files are written mode 0600. Names and
the 25 MiB limit are specified in `http-api.md`. There is no automatic cleanup.
This directory follows an explicit `SESSIONS_STATE_DIR` as described above.

### `delivery-operations/<operation-id>.json`

Go-runtime-only, mode 0600 files below a mode-0700 directory. Each file is a
durable receipt for one logical `/submit` operation: UUID, target session id,
content SHA-256, content byte count, status, delivery/retry booleans, optional
reason, and creation/update times. It deliberately does not store the message
body. A `pending` file left by a crash is treated as `unknown` and must not be
retried automatically. Reusing an operation id with different content or a
different target is refused. This directory follows `SESSIONS_STATE_DIR` so an
isolated daemon cannot read or write the installed daemon's receipts.

### `idle/<id>`

A best-effort completion sentinel written on an observed `working: true ->
false` transition after the activity classifier's first sample. Contents are
one compact JSON object plus newline:

```json
{"id":"<id>","name":"<display label>","at":"2026-07-16T19:44:02.123Z"}
```

`name` chooses explicit session `name`, Claude custom title, Claude AI title,
cwd basename, command, then the first eight ID characters. Directory mode is
requested as 0700 and file mode as 0600. A later `false -> true` transition
unlinks the sentinel. Failures are swallowed. This path ignores
`SESSIONS_STATE_DIR`.

### `runners/<id>.json`

Runner-written, sessions-printed JSON, mode 0600. The schema has exactly these
fields in current output:

```json
{
  "id": "2f577cd7-565b-4861-8ea2-c77c39a20e24",
  "cmd": "/bin/zsh",
  "args": ["-l"],
  "cwd": "/Users/example/project",
  "cols": 300,
  "rows": 50,
  "createdAt": 1750000000123,
  "pid": 43210,
  "sockPath": "/Users/example/.local/state/sessions/runners/2f577cd7-565b-4861-8ea2-c77c39a20e24.sock"
}
```

Types:

| Field | JSON type | Source value |
| --- | --- | --- |
| `id` | string | `RUNNER_ID` |
| `cmd` | string | configured command |
| `args` | string[] | original configured args, even when respawn temporarily changes `--session-id` to `--resume` |
| `cwd` | string | configured cwd |
| `cols`, `rows` | number | startup dimensions |
| `createdAt` | number | runner startup Unix epoch milliseconds |
| `pid` | number | spawned PTY child PID, not runner PID |
| `sockPath` | string | absolute/joined socket path |

The runner mutates an in-memory metadata object after RESIZE but never rewrites
the JSON, so on-disk dimensions remain the startup values. No `name`, `onIdle`,
`working`, exit state, tool classification, environment, runner process PID, or
protocol version is persisted. Daemon-only `name` and `onIdle` therefore do not
survive daemon reattachment. See `fixtures/runner.json`.

The file is overwritten on every runner respawn, resetting `createdAt` and
`pid`. It is removed on both normal-ended cleanup and SIGTERM cleanup.

The Go runtime writes additional daemon-owned fields into the same document
and preserves them across a runner write, which rebuilds the document from
launch configuration and carries none of them
(`runtime/internal/state/metadata_merge.go` `MergeRunnerMetadata`). Among them:

| Field | JSON type | Meaning |
| --- | --- | --- |
| `name` | string, optional | the canonical Sessions title |
| `name_source` | `"launch" \| "provider" \| "explicit"`, optional | who the name came from, and therefore who may change it |

`name_source` decides whether the daemon keeps the session's name on the
provider's own conversation title. `launch` and `provider` are adoptable: the
daemon rewrites `name` whenever Claude's `custom-title`/`ai-title` records give
the conversation a different title, so the session card agrees with every
Claude surface and stays searchable under the name the user can see there.
`explicit` is set by `PUT /api/sessions/:id/name` and pins the name to the
user's choice until `PUT` with `{"auto":true}` (`sessions rename --auto`)
releases it. Absent means adoptable, so no migration is needed for sessions
created before the field existed; the daemon cannot tell such a session's name
from a launch auto-name, so one provider title may replace an old rename, and
renaming again pins it for good. Codex writes no conversation title into its
rollout files, so a Codex session's name is never adopted.

### `runners/<id>.sock`

Unix stream socket implementing `runner-protocol.md`, chmod 0600 after bind. It
is removed before a stale rebind and on all runner cleanup flavors. A runner
bind error exits nonzero. The path's presence is the daemon discovery key.

### `runners/<id>.events`

Append-only terminal output records as specified byte-for-byte in
`runner-protocol.md`. It survives SIGTERM/reboot-style cleanup and is restored
on the next runner start. It is removed after the actual PTY ends. Initial
creation uses `openSync(..., "a+")` without an explicit mode, so creation mode
comes from Node/POSIX defaults and umask; a trim rewrite uses explicit 0600.

`<id>.events.tmp` exists only while an over-cap trim is being atomically
rewritten. Runner open attempts to remove a stale temp file.

### `runners/<id>.log`

Both launchd `StandardOutPath` and `StandardErrorPath` point here. The runner
does not explicitly create, chmod, rotate, truncate, or unlink it; launchd owns
opening/appending behavior. Despite stale source comments describing stdio-log
removal, current cleanup code leaves `.log` behind.

### `runners/<id>.keepalive.json` and `.restore-pending.json`

Go runtime only. `.keepalive.json` is a mode-0600 boot-scoped restart permit
containing the platform boot identity and its creation time. Its presence makes
launchd's `PathState` true. A runner keeps the permit across an unexpected
same-boot crash, and removes it on a normal end, explicit stop, malformed
startup, or other terminal startup failure.

When the permit belongs to an earlier boot, the runner admits only the eight
most recently active pinned, non-lane roots whose permits prove they were
running before shutdown. It renews those permits for the current boot. Every
other runner removes its stale permit, exits without starting its provider, and
writes `.restore-pending.json` with the session id, reason, and detection time.
Discovery treats that marker as an actionable safety state: it preserves the
metadata, launch record, events, and transcript, includes the session in the
ordinary list as `unreachableReason: "restart-restore-pending"`, and never
reports an empty successful live read for it.

Both files are sidecars, not runner metadata documents, and are excluded by
`RunnerIDFromMetadataName`. Both `/api/health` responses report the current
marker count as `restore.pending` and the compiled ceiling as
`restore.automaticPinnedLimit`. A non-zero count also makes health `status`
and `restore.status` equal `"degraded"`, with code `SESSION_RESTORE_PENDING`
and the explicit recovery action, so a client or operator does not have to
infer recovery failure from an empty live-session list.

### `runners/<id>.transcript.jsonl` and `.transcript.meta.json`

The transcript file is Sessions' own append-only copy of a Claude conversation,
written by the daemon's
PTY-backed Claude watcher rather than by the runner
(`runtime/internal/watch/transcript_mirror.go`). Provider lines are stored
verbatim and in observed order, deduplicated by each record's own `uuid`, so the
file is itself a legal Claude transcript. The transcript file is opened 0600 and
re-chmoded on every open; the sidecar is written 0600.

The provider file wins whenever it still resolves, so a session resolves to
exactly one transcript path and no reader can count a conversation twice
(`watch.ResolveClaudeWithMirror`). This copy is the answer only once the
provider's file cannot be resolved at all, and it is deliberately outside every
cleanup path: it is not truncated, rotated, repaired, or unlinked when the
session ends, when discovery reaps a dead runner, or when retention archives the
record. Reaching the 512 MiB cap stops appends and is recorded in the sidecar
instead of discarding stored conversation.

`.transcript.meta.json` is one compact JSON object plus newline, carrying
`version`, the Sessions and provider identifiers, the observed `providerPath`,
`records`, `bytes`, first/last observed times, a `generations` count of observed
provider truncations or replacements, and `capped`/`writeErrors`/`lastError`
when they apply. It is replaced by temp-file rename. The transcript is authoritative
over a sidecar a crash left mid-write: opening the mirror rescans it and
recomputes `records` and `bytes`.

Neither name is a runner metadata document. `.transcript.meta.json` is listed in
`runtime/internal/state/paths.go` `runnerSidecarJSONSuffixes` so discovery does
not read it as `<id>.json` and invent a phantom runner id.

## Runner launch environment

The daemon creates the runner directory recursively with requested mode 0700.
For each session it generates a UUID and launchd environment including:

- fixed/control values: `TERM=xterm-256color`, `RUNNER_ID`,
  `RUNNER_STATE_DIR`, `RUNNER_CMD`, `RUNNER_ARGS_JSON`, `RUNNER_CWD`,
  `RUNNER_COLS`, `RUNNER_ROWS`;
- default/current `HOME`, `USER`, `PATH`, `LANG`, `SHELL`;
- selected SSH/auth/proxy/CA pass-through variables;
- caller values after reserved and process-injection keys are stripped.

The locked `TERM` and `RUNNER_*` values win over caller environment. The runner
removes its control variables before spawning the PTY child and forces
`TERM=xterm-256color`, `COLORTERM=truecolor` there.

## Per-session launchd plist

Path and label scheme are normative:

```text
path:  ~/Library/LaunchAgents/tech.somewhere.sessions.runner.<id>.plist
label: tech.somewhere.sessions.runner.<id>
```

The file is written/chmoded 0600 because its environment can contain secrets.
The semantic generated shape, with XML-escaped values and one entry per
argument and environment pair, is:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>tech.somewhere.sessions.runner.<id></string>
  <key>ProgramArguments</key>
  <array>
    <string><program argv[0]></string>
    <string><program argv[1]></string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>NAME</key>
    <string>value</string>
  </dict>
  <key>WorkingDirectory</key>
  <string><session cwd></string>
  <key>RunAtLoad</key>
  <false/>
  <key>KeepAlive</key>
  <dict>
    <key>PathState</key>
    <dict>
      <key><runner state dir>/<id>.keepalive.json</key>
      <true/>
    </dict>
  </dict>
  <key>ProcessType</key>
  <string>Interactive</string>
  <key>StandardOutPath</key>
  <string><runner state dir>/<id>.log</string>
  <key>StandardErrorPath</key>
  <string><runner state dir>/<id>.log</string>
</dict>
</plist>
```

Comments explaining KeepAlive and ProcessType are also emitted in the actual
file, but have no plist semantics. Environment entries are sorted by key. XML
escaping replaces `&`, `<`, and `>` in text values.

Bootstrap invokes:

```text
launchctl bootstrap gui/<uid> <plist-path>
```

Exit status 17 or stderr matching “already loaded/bootstrapped” is accepted as
success. Bootout invokes `launchctl bootout
gui/<uid>/tech.somewhere.sessions.runner.<id>` and then unlinks the plist regardless of
the command result.

New plists run the configured native `sessions-runner` executable directly.
An adopted pre-native plist may retain its earlier argv until the session exits
and the compatibility registration is reaped.

## Runner lifecycle and restoration

On a fresh runner start:

1. validate control environment and create the runner state directory;
2. validate the boot-scoped restart permit, or record a paused restore and exit
   without starting the provider;
3. guard against a duplicate socket owner, then remove a stale socket;
4. decide the actual PTY spawn args;
5. spawn the PTY;
6. open and restore `.events` into the in-memory log/mirror;
7. write `.json`;
8. listen on and chmod `.sock`.

When a non-empty `.events` exists and configured args contain
`--session-id <uuid>`, the runner uses a copied spawn argv with that flag changed
to `--resume`, while HELLO and metadata continue to expose the original args.
It best-effort searches `~/.claude/projects/*/<uuid>.jsonl`; absence changes
cleanup preservation behavior but does not prevent the resume attempt.

On real PTY exit, runner state remains attachable until no clients remain and a
30-second timer expires. Cleanup removes socket and metadata and normally
deletes events. If a Claude respawn found its backing JSONL missing, the runner
deliberately leaves `sessionEnded=false` and preserves events instead. The
daemon bootouts/unlinks the plist on EXIT and retains an in-memory exited
session for its own 30-second grace period. The `.log` remains.

On runner SIGTERM, cleanup removes socket/metadata, closes but preserves events,
kills the PTY child, preserves the boot-scoped permit, and exits. launchd uses
that permit to restart an unexpected same-boot termination; after reboot the
runner reads its old boot id and applies the bounded restore decision before it
starts a provider. A normal provider exit, an explicit runner Kill, and daemon
reaping remove the permit. SIGINT and SIGHUP are ignored.

At daemon startup, canonical Sessions plists carrying the exact older
`RunAtLoad=true` plus `SuccessfulExit=false` policy are atomically migrated to
the boot-scoped `PathState` policy, receive the matching runner environment,
and get a current-boot permit so the following login can apply the bounded
restore decision visibly. Unknown, hand-edited, and legacy-prefix plists are
not rewritten.

## Daemon startup discovery

`server.ts` begins listening first, then starts discovery asynchronously.
`/api/health` exposes `discovering=true` during the scan, and session lists can
be partial until it finishes.

Discovery uses this algorithm:

1. Set `discovering=true` in a `try/finally`.
2. Run orphan-plist cleanup against the selected runner state directory.
3. If the directory is absent/unreadable, finish.
4. Enumerate entries ending exactly `.sock`; other artifacts alone are not
   attachment candidates.
5. For each socket, try `registerRunner()` up to three times, waiting 800 ms
   after each of the first two failures. Registration requires HELLO, tolerates
   protocol mismatch, requests replay from sequence 0, and waits at most 10
   seconds for replay completion.
6. After three failures, read the same basename's `.json` and test its `pid`
   (the PTY child PID) with signal 0. If alive, inspect `ps -p <pid> -o args=`:
   - a non-empty command line containing none of `runner.js`, `runner.ts`, or
     the session ID is treated as PID reuse/dead;
   - a matching or unavailable/empty command line is conserved as possibly
     live, and all state is left untouched.
7. When no live PID is conservatively established, unlink `.sock` and `.json`,
   boot out the launchd service, and unlink its plist. This failure path does
   not unlink `.events` or `.log`.
8. Clear `discovering` even if the scan throws.

`registerRunner()` keys the daemon map by the ID received in HELLO, while the
failure cleanup uses the socket filename's ID.

### Orphan-plist cleanup pre-pass

For every `tech.somewhere.sessions.runner.*.plist`:

- if matching `.events` exists, always keep the plist;
- otherwise, if plist mtime is less than 30 seconds old, conservatively keep it;
- otherwise, only when both `.sock` and `.json` are absent, bootout and unlink
  the plist;
- stat/read problems are handled conservatively by keeping the plist.

Thus an events-only session is intentionally preserved for explicit recovery.
A lone socket or lone metadata file also prevents this pre-pass from deleting
the plist. A `.restore-pending.json` marker additionally prevents cleanup of a
runner deliberately paused by the reboot budget. The later socket-connection
failure path is stricter as described above.
