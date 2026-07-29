# sessionsd WebSocket contract

This is the protocol preserved in `runtime/testdata/node-runtime/src/ws.ts`, with message shapes
from `runtime/testdata/node-runtime/src/types.ts` and actual browser consumption from
`frontend/src/lib/wsMux.ts` and `frontend/src/hooks/useTerminal.ts`.

All application messages described below are UTF-8 JSON objects. Protocol
version is currently `2` and appears in each server `hello` message.

## Upgrade

The only upgrade path is exactly `/ws`. An upgrade for any other pathname is
destroyed without a WebSocket handshake.

Query parameters:

| Parameter | Mode | Meaning |
| --- | --- | --- |
| `token` | both | required auth token unless open mode is enabled |
| `mux=1` | mux | selects multiplexed mode; it takes precedence over `sessionId` |
| `sessionId` | single | required sessionsd session ID |
| `lastSeq` | single | resume raw output after this sequence; default 0 |
| `claudeEventsSince` | single | resume structured events at this absolute index; default 0 |

`lastSeq` and `claudeEventsSince` are parsed with `Number`, truncated through a
signed 32-bit integer conversion, then clamped to at least zero. Mux mode puts
those values on each `attach` message instead.

### Upgrade security

- Origin is checked before auth. The allowlist is the same as `http-api.md`,
  but a disallowed/malformed Origin receives raw HTTP `403 Forbidden` with
  `Content-Length: 0`, then the socket is destroyed. No Origin is allowed.
- WebSocket auth accepts only the `token` query parameter; it does not inspect
  an Authorization header. A bad/missing token receives raw HTTP `401
  Unauthorized` with `Content-Length: 0`, then the socket is destroyed.
- The fixed `open` sentinel bypass applies, but the implementation calls
  `getAuthToken()` before testing it, so the first WS upgrade can still create
  the token file in open mode.
- Inbound WebSocket payloads are capped at 256 KiB. The `ws` library closes an
  oversized connection with code 1009.

## Connection modes

### Single-session mode

URL: `/ws?sessionId=<id>&token=<token>`.

After upgrade, missing `sessionId` produces
`{"type":"error","message":"missing sessionId"}` and close code 1008 with
reason `missing sessionId`. An unknown ID similarly sends `unknown session
<id>` and closes 1008 with reason `unknown session`.

The connection attaches exactly one session. Message-level `sessionId` values
are ignored. Binary frames are decoded as UTF-8 and sent directly to the PTY.
A text frame that does not parse as JSON is also sent directly to the PTY. Of
parsed JSON messages, only `ping`, `input`, and `resize` have behavior; other
types are ignored. There are no input acknowledgements or RPCs in this mode.

Handlers are installed before replay starts, so input received during a
backpressured replay is handled. When the session exits, the server sends the
`exit` message and closes with code 1000, reason `pty exited`.

### Multiplexed mode

URL: `/ws?mux=1&token=<token>`.

There is no connection-level hello. The client sends one `attach` per session,
and each successful attach starts its own hello/replay/live stream. Every
session stream message is tagged with `sessionId`. Duplicate attaches are
ignored both after attachment and while the first async replay is pending.
Unknown attachment IDs produce an `error` but do not close the socket.

Mux input is JSON-only in the application protocol: invalid JSON is ignored and
there is no untagged raw-input fallback. Detaching/exiting one session leaves
the socket and its other streams alive. Closing the socket removes every live
listener.

## Attach and replay ordering

For either mode, a successful attach does the following in order:

1. Compute raw replay as events with sequence `> lastSeq` from the daemon's
   bounded in-memory log.
2. Compute an absolute Claude-event replay start.
3. Send `hello`.
4. If output is enabled, send `gap` when requested history has aged out, then
   all retained `output` events with sequence `> lastSeq`.
5. Send the selected historical `claudeEvent` records.
6. If already exited, send `exit` and install no live listeners; otherwise
   install listeners for live output/events/exit/runner loss.

Replay sends pause when `ws.bufferedAmount` exceeds 1 MiB. The drain wait has a
5-second safety timeout.

The Claude ring retains at most 5,000 records and tracks an absolute front
offset. A fresh attach (`claudeEventsSince` absent/zero) replays only the most
recent 300 retained records. A positive resume index maps through the front
offset and is bounded to the retained range. `claudeReplay=false` starts at the
current end, so no history is sent and `claudeReplayStart` equals the current
absolute count. `claudeLive=false` affects only subsequently emitted live
records; it does not itself suppress attach replay.

Raw output replay and gap delivery are both suppressed by
`outputReplay=false`, and no live output listener is installed. Hello,
structured events, exit, and runner-lost still flow.

## Client to server messages

Fields marked optional are genuinely optional at runtime unless behavior below
says the message is ignored without them.

### `ping`

```json
{"type":"ping"}
```

Both modes reply `{"type":"pong"}`. This is an application message, not a
WebSocket control ping. Browser clients send it every 20 seconds and force a
reconnect when no server message has arrived for 30 seconds.

### `input`

```json
{"type":"input","data":"raw UTF-8 bytes","sessionId":"<mux id>","requestId":"optional"}
```

- Single: URL session is used; message `sessionId` and `requestId` are ignored.
  No response.
- Mux: missing `sessionId` is ignored. The data is sent only for a known,
  non-exited session. When `requestId` is truthy, an `inputAck` is sent with the
  resulting boolean; without it, there is no response.

### `resize`

```json
{"type":"resize","cols":120,"rows":40,"sessionId":"<mux id>"}
```

Single uses the URL session. Mux requires `sessionId`. Both truncate dimensions
through signed 32-bit integer conversion and clamp columns to 40..500 and rows
to 10..200. Resize is last-writer-wins across clients. There is no response;
unknown/exited sessions silently fail after clamping.

### `attach` (mux only)

```json
{
  "type":"attach",
  "sessionId":"<id>",
  "lastSeq":17,
  "claudeEventsSince":42,
  "outputReplay":false,
  "claudeReplay":false,
  "claudeLive":false
}
```

`sessionId` is required. Resume values default to zero, are signed-32-bit
truncated, then clamped non-negative. Each suppression flag defaults to enabled;
only literal `false` disables its named stream behavior.

### `detach` (mux only)

```json
{"type":"detach","sessionId":"<id>"}
```

Removes listeners for that attachment. Missing/unknown IDs are silent.

### `snapshot` RPC (mux only)

```json
{"type":"snapshot","requestId":"<id>","sessionId":"<id>","cols":80}
```

Missing/falsy `requestId` or `sessionId` causes silent ignore. Positive `cols`
is floored and requests server-side ANSI reflow; absent/non-positive selects the
canonical snapshot. Success is a `snapshot` response. Unknown session is an
`rpcError` with code `not_found`; other thrown failures are `rpcError` without a
code.

### `events` RPC (mux only)

```json
{"type":"events","requestId":"<id>","sessionId":"<id>","since":10,"tail":40}
```

Missing/falsy `requestId` or `sessionId` is ignored. Unknown session is an
`rpcError` with code `not_found`. Indices are absolute:

- finite non-negative `since` maps through the retained front offset;
- finite positive `tail` selects at most the last `floor(tail)` retained
  records, composed with `since` by taking the later start;
- invalid values are ignored.

Unlike HTTP events, mux events has no `before`, `startIndex`, or `endIndex`; its
window always ends at the current retained end.

## Server to client messages

### `hello`

```json
{
  "type":"hello",
  "protocol":2,
  "session":{/* SessionInfo; see http-api.md */},
  "currentSeq":17,
  "resumedFromSeq":12,
  "claudeEventsCount":91,
  "claudeReplayStart":12,
  "sessionId":"<mux only>"
}
```

- `currentSeq` is the current raw event-log sequence at replay calculation.
- `resumedFromSeq` is the requested `lastSeq` when it is positive, otherwise
  null. It is not proof that the requested event still exists; a following
  `gap` reports that condition.
- `claudeEventsCount` is the absolute structured-event count at attach time.
- `claudeReplayStart` is the absolute index of the first history event about to
  be replayed, or the current end when replay is suppressed.
- `sessionId` is present only in mux mode.

### `output`

```json
{"type":"output","seq":18,"data":"\u001b[32mtext\u001b[0m","sessionId":"<mux only>"}
```

Sequence is the runner's monotonic unsigned-32-bit sequence. Data is a UTF-8 PTY
chunk and can split terminal lines or logical escape sequences.

### `gap`

```json
{"type":"gap","oldestAvailableSeq":15,"currentSeq":30,"sessionId":"<mux only>"}
```

Sent before output replay only when `lastSeq + 1` precedes the oldest retained
event and output replay is enabled. The frontend clears/resets xterm, prints a
resync banner, and sets its local last sequence to
`oldestAvailableSeq - 1` before applying replay.

### `exit`

```json
{"type":"exit","code":0,"signal":null,"seq":30,"sessionId":"<mux only>"}
```

`code` and `signal` are nullable. A runner socket disconnect without an EXIT
frame produces the synthetic form:

```json
{"type":"exit","code":null,"signal":null,"seq":30,"reason":"runner-lost","sessionId":"<mux only>"}
```

For runner loss, the emitted `seq` is the `replay.current` value captured when
the attachment was established, even if later output arrived. The frontend's
declared `ServerMsg` currently omits `reason` and its exit handler treats both
forms identically.

### `error`

```json
{"type":"error","message":"unknown session <id>","sessionId":"<mux id when known>"}
```

Used for missing/unknown single-session setup and unknown mux attach. The
frontend displays the message; an `unknown session` message marks the terminal
dead and detaches it.

### `rpcError`

```json
{"type":"rpcError","requestId":"<request id>","message":"unknown session <id>","code":"not_found","sessionId":"<id>"}
```

`code` and `sessionId` are optional in the type, though current snapshot/events
not-found paths supply both. The mux manager matches by `requestId` before
session routing and rejects the pending Promise.

### `snapshot`

```json
{"type":"snapshot","requestId":"<request id>","text":"<ANSI snapshot>","seq":30,"sessionId":"<id>"}
```

The sequence is read after serialization so it covers the mirrored bytes in the
returned snapshot.

### `events`

```json
{"type":"events","requestId":"<request id>","events":[],"nextIndex":91,"totalCount":91,"sessionId":"<id>"}
```

Events are passthrough structured JSON objects. Both counts are the current
absolute total; the response does not expose its selected start.

### `inputAck`

```json
{"type":"inputAck","requestId":"<request id>","ok":true,"sessionId":"<id>"}
```

Only mux `input` carrying `requestId` gets this response. `ok=false` means the
session was unknown or exited.

### `claudeEvent`

```json
{"type":"claudeEvent","event":{"type":"assistant"},"sessionId":"<mux only>"}
```

The event is a passthrough Claude persistence record or a Codex record
normalized into the same broad shape. Consumers must preserve unknown fields.

### `pong`

```json
{"type":"pong"}
```

Untagged in both modes. In mux mode it updates the staleness clock, then is
discarded because it has neither a `requestId` nor `sessionId`.

## Normative frontend consumption

The browser constructs `/ws?mux=1[&token=...]` and keeps one manager per exact
URL. On reconnect it reattaches every registered session with current
`lastSeq`, absolute structured-event count, and view-dependent replay/live
flags. Frames queued while offline are bounded to 2,000 and are flushed only
after attaches on reopen. Snapshot/events/input RPCs time out after 10 seconds.

The terminal consumer:

- initializes its structured-event counter from `hello.claudeReplayStart`;
- batches `output` writes while updating `lastSeq` immediately;
- resets xterm on `gap` as described above;
- flushes output, displays terminal state, and detaches on `exit`;
- displays and terminally handles unknown-session `error`;
- folds `claudeEvent` into UI state only for the active view.

Although both TypeScript copies export protocol version 2, the current frontend
does not reject a `hello` with a different `protocol` value. The single-session
CLI paths consume only `output` and `exit`; attach sends JSON `input`/`resize`,
while tail-follow sends no resize.
