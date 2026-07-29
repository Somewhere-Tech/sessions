# Sessions integration endpoints

Sessions exposes local, read-only integration contracts for session recall and
error automation. These endpoints do not call somewhere, file tasks, or upload
data. A consumer such as a somewhere MCP server calls the user's Sessions daemon
with the user's normal Sessions token.

## Common contract

- All routes require normal Sessions authentication: `Authorization: Bearer
  <token>` or `?token=<token>`. The daemon's existing loopback and `open`-file
  behavior is unchanged.
- JSON responses use `Content-Type: application/json` and include the integer
  `schemaVersion`. The current version is `1`.
- Additive fields may appear within a schema version. A breaking rename, type
  change, removal, or semantic change requires a new `schemaVersion`.
- Times ending in `_at` in history metadata are Unix epoch milliseconds.
  Message timestamps and error event `ts` values are RFC 3339 strings when
  available.
- Conversation files are sensitive. Token access to these endpoints grants
  access to the normalized transcript and exact raw file bytes.

## Session-history recall

History is assembled from live sessions and durable runner metadata in the
configured runner state directory. Claude and Codex conversation paths are
resolved with the same watcher rules used by Sessions' backup feature. Results
are ordered by `last_activity_at` descending, then `id` ascending.

### `GET /api/history`

Returns every known session with recall metadata. `message_count` counts only
normalized, non-empty user and assistant text turns. Tool-only records,
lifecycle records, and Codex environment preambles are not counted.

```json
{
  "schemaVersion": 1,
  "sessions": [
    {
      "id": "11111111-2222-4333-8444-555555555555",
      "name": "release investigation",
      "tool": "claude",
      "cwd": "/Users/alice/src/service",
      "machine": "alice-mac.local",
      "created_at": 1784221200000,
      "last_activity_at": 1784221320000,
      "message_count": 14,
      "conversation_available": true
    }
  ]
}
```

Each session object always contains all ten fields:

| Field | JSON type | Meaning |
| --- | --- | --- |
| `id` | string | Sessions runner/session identifier |
| `name` | string | User label; empty string when none was set |
| `tool` | string | Normalized tool name, normally `claude`, `codex`, or `terminal` |
| `cwd` | string | Session working directory |
| `machine` | string | Hostname of the Sessions daemon |
| `created_at` | number | Session creation time in Unix epoch milliseconds |
| `last_activity_at` | number | Latest metadata or conversation-file activity in Unix epoch milliseconds |
| `message_count` | number | Count of normalized user/assistant text turns |
| `conversation_available` | boolean | Whether a resolved regular conversation file can be recalled |

An empty result is `{"schemaVersion":1,"sessions":[]}`.

### `GET /api/history/:id?format=json`

`format=json` is the default. The response repeats the exact history session
shape and adds ordered normalized messages:

```json
{
  "schemaVersion": 1,
  "session": {
    "id": "11111111-2222-4333-8444-555555555555",
    "name": "release investigation",
    "tool": "claude",
    "cwd": "/Users/alice/src/service",
    "machine": "alice-mac.local",
    "created_at": 1784221200000,
    "last_activity_at": 1784221320000,
    "message_count": 2,
    "conversation_available": true
  },
  "messages": [
    {
      "role": "user",
      "text": "Why did the release fail?",
      "timestamp": "2026-07-16T17:01:00Z"
    },
    {
      "role": "assistant",
      "text": "The signing dependency was unavailable.",
      "timestamp": null
    }
  ]
}
```

Message fields are stable:

| Field | JSON type | Meaning |
| --- | --- | --- |
| `role` | `"user"` or `"assistant"` | Normalized conversational role |
| `text` | string | Plain text; multiple text blocks are joined by one blank line |
| `timestamp` | string or null | Provider timestamp, or null when the source record has none |

Claude's canonical event records are reduced directly. Codex rollout records
first pass through Sessions' `internal/watch` Codex normalizer, so recall and the
live daemon share the same filtering and role semantics.

### `GET /api/history/:id?format=text`

Returns `text/plain; charset=utf-8`. Each turn is formatted as a bracketed role
and optional timestamp followed by its text:

```text
[user 2026-07-16T17:01:00Z]
Why did the release fail?

[assistant]
The signing dependency was unavailable.
```

The response header `X-Sessions-Schema-Version: 1` versions this text format.

### `GET /api/history/:id/raw`

Returns the exact resolved Claude JSONL or Codex rollout bytes with
`Content-Type: application/octet-stream`. The raw provider format is not a
Sessions schema and therefore has no `schemaVersion`; consumers that need a
stable shape should use `format=json`.

For a missing session or a known session without an available conversation,
the transcript and raw routes return:

```json
{
  "error": "history session not found",
  "id": "11111111-2222-4333-8444-555555555555"
}
```

The status is 404. An unsupported `format` returns 400 with
`{"error":"format must be json or text"}`. A caught filesystem/read failure
returns 500 and is also offered to the error feed as `kind: "daemon_error"`.

## Error-event feed

Sessions stores structured errors in `<state-dir>/errors.jsonl`. The file is
append-only, created with mode `0600`, and each line is one complete JSON event.
Sequence numbers resume from the highest durable sequence after daemon restart.

The daemon emits `runner_exit` for observed nonzero or signaled runner exits,
and `runner_lost` when a tracked runner connection disappears unexpectedly.
An integration request attaches a read-only terminal observer to live sessions
and also reconciles exited sessions still in the daemon grace window. Caught
integration filesystem/read errors emit `daemon_error`. Additional daemon
failure sites can use the same internal recorder without changing this feed.

### `GET /api/errors?since=<seq>`

`since` is an exclusive, non-negative integer cursor. It defaults to `0`.
Every event with `seq > since` is returned in append order. `nextSeq` is the
cursor the consumer should send on its next poll. The route currently returns
all available events after the cursor; consumers must tolerate a future
documented page limit while continuing to advance via `nextSeq`.

```json
{
  "schemaVersion": 1,
  "errors": [
    {
      "seq": 41,
      "ts": "2026-07-16T18:00:01.123Z",
      "kind": "runner_exit",
      "session_id": "11111111-2222-4333-8444-555555555555",
      "summary": "runner exited with code 17",
      "detail": "exit_code=17 signal= tool=codex cwd=/Users/alice/src/service",
      "machine": "alice-mac.local"
    },
    {
      "seq": 42,
      "ts": "2026-07-16T18:00:03.456Z",
      "kind": "daemon_error",
      "summary": "history transcript failed",
      "detail": "read history transcript 11111111-2222-4333-8444-555555555555: permission denied",
      "machine": "alice-mac.local"
    }
  ],
  "nextSeq": 42
}
```

Error event fields are stable:

| Field | JSON type | Meaning |
| --- | --- | --- |
| `seq` | number | Durable, monotonically increasing feed sequence |
| `ts` | string | UTC RFC 3339 event time |
| `kind` | string | Machine-readable category such as `runner_exit`, `runner_lost`, or `daemon_error` |
| `session_id` | string, optional | Related Sessions session; omitted for daemon-wide errors |
| `summary` | string | Short human-readable failure summary |
| `detail` | string | Diagnostic detail; consumers must not parse this as a stable sub-schema |
| `machine` | string | Hostname of the emitting Sessions daemon |

If no new event exists, `errors` is an empty array and `nextSeq` remains the
current cursor. Invalid cursors return 400:

```json
{"error":"since must be a non-negative integer sequence"}
```

A polling integration should persist `nextSeq` only after it has processed the
corresponding response. Processing the same event again must be idempotent; its
stable identity is the pair `(machine, seq)`.

## Thin local CLI

`sessions recall` lists history, `sessions recall <full-session-id>` prints the
normalized text transcript, and `sessions recall <full-session-id> --raw` writes
the raw bytes. The global `--json` flag returns the versioned JSON list or
transcript. The HTTP API above remains the integration contract.
