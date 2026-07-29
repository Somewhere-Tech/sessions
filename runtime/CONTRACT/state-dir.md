# sessionsd state and discovery contract

This records the paths and lifecycle actually used by the TypeScript source.
The current layout is split: runner artifacts live in a `runners/` subdirectory
by default, while auth, push, uploads, and idle state live at the root.

## Default layout

```text
~/.local/state/sessions/
├── token
├── open
├── vapid.json
├── push-subscriptions.json
├── uploads/
│   └── <sanitized-stem>-<8 UUID chars><ext>
├── idle/
│   └── <session-id>
└── runners/
    ├── <session-id>.json
    ├── <session-id>.sock
    ├── <session-id>.events
    ├── <session-id>.events.tmp       # transient trim file only
    └── <session-id>.log

~/Library/LaunchAgents/
└── tech.somewhere.sessions.runner.<session-id>.plist
```

`SESSIONS_STATE_DIR` replaces only the runner artifact directory used by
`sessions.ts` and passed to runners as `RUNNER_STATE_DIR`. For example,
`SESSIONS_STATE_DIR=/tmp/ct-state` places `<id>.json/.sock/.events/.log` directly
in `/tmp/ct-state`, not `/tmp/ct-state/runners`. It does **not** relocate
`token`, `open`, VAPID/subscriptions, uploads, or idle sentinels. Those use
`os.homedir()` and the fixed root above. A safe isolated test must therefore set
both `HOME` and `SESSIONS_STATE_DIR`.

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

Uploaded raw request bodies for known sessions. The fixed uploads directory is
created recursively with requested mode 0700 and files are written mode 0600.
Names and the 25 MiB limit are specified in `http-api.md`. There is no automatic
cleanup in the normative source.

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
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
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
file, but have no plist semantics. Environment entries retain JavaScript object
insertion order. XML escaping replaces `&`, `<`, and `>` in text values.

Bootstrap invokes:

```text
launchctl bootstrap gui/<uid> <plist-path>
```

Exit status 17 or stderr matching “already loaded/bootstrapped” is accepted as
success. Bootout invokes `launchctl bootout
gui/<uid>/tech.somewhere.sessions.runner.<id>` and then unlinks the plist regardless of
the command result.

Current production runner argv prefers `/usr/bin/env node <runner.js>`. A fresh
source/development tree may use a fresh `dist/runner.js` or
`/usr/bin/env node <local tsx> <runner.ts>`.

## Runner lifecycle and restoration

On a fresh runner start:

1. validate control environment and create the runner state directory;
2. guard against a duplicate socket owner, then remove a stale socket;
3. decide the actual PTY spawn args;
4. spawn the PTY;
5. open and restore `.events` into the in-memory log/mirror;
6. write `.json`;
7. listen on and chmod `.sock`.

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
kills the PTY child, and exits 0. KeepAlive with `SuccessfulExit=false` therefore
does not itself respawn that clean SIGTERM exit; the plist remains for a future
RunAtLoad. SIGINT and SIGHUP are ignored.

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

Thus an events-only session is intentionally preserved for a later RunAtLoad.
A lone socket or lone metadata file also prevents this pre-pass from deleting
the plist. The later socket-connection failure path is stricter as described
above.
