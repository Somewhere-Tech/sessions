# daemon to runner protocol

This contract was implemented by `runtime/testdata/node-runtime/src/runnerProtocol.ts`,
`runtime/testdata/node-runtime/src/runner.ts`, `runtime/testdata/node-runtime/src/runnerClient.ts`, and the daemon-side
registration logic in `runtime/testdata/node-runtime/src/sessions.ts`.

## Transport and socket ownership

Each runner owns one local byte-stream endpoint. On Unix it is a domain socket
named `<id>.sock` in the runner state directory. On Windows it is a randomized
Named Pipe below the Sessions namespace. Both transports carry the same frame
contract and accept multiple clients, although sessionsd is normally the only
one. A new connection is server-first: the runner immediately sends HELLO and,
if its PTY has already exited, immediately follows with EXIT. There is no
separate connection preface, magic, or negotiation request.

The Unix runner chmods its listening socket to 0600 after bind. The Windows
runner creates the first pipe instance atomically with a protected
owner-SID-only DACL, rejects remote clients, and verifies the peer process SID
on both ends. Before binding, either platform probes a pre-existing endpoint
for up to one second: a successful authenticated connect means a duplicate live
owner and the new runner exits 0. A failed Unix probe may identify a stale
filesystem marker to unlink; a Windows pipe is a kernel object and disappears
when its owner closes it.

## Socket frame encoding

Every socket frame is:

```text
offset  width  encoding       value
0       4      uint32, BE     L = 1 + payload byte length
4       1      uint8          frame type
5       L-1    type-specific  payload
```

The length excludes the four-byte prefix and includes the one-byte type.
`MAX_FRAME_LEN` is 4,194,304 bytes for `L`, so the maximum payload is 4,194,303
bytes. An encoder rejects a larger frame. The streaming parser buffers partial
and coalesced socket reads; `L < 1` or `L > MAX_FRAME_LEN` clears its buffered
stream and throws. The owning caller destroys the socket on parser error.

Strings are UTF-8. JSON is compact `JSON.stringify` output on the wire. Integer
sequence fields outside JSON are unsigned 32-bit big-endian; encoders apply
JavaScript `>>> 0`, so they wrap modulo 2^32. Unknown frame types are ignored by
both current receivers for forward compatibility.

### Frame type summary

| Direction | Type | Hex | Payload |
| --- | --- | --- | --- |
| daemon -> runner | INPUT | `0x10` | raw UTF-8 input bytes |
| daemon -> runner | RESIZE | `0x11` | JSON `{"cols":number,"rows":number}` |
| daemon -> runner | SNAPSHOT_REQ | `0x12` | empty |
| daemon -> runner | REPLAY_REQ | `0x13` | 4-byte BE `afterSeq` |
| daemon -> runner | KILL | `0x14` | empty |
| daemon -> runner | MODEL_REQ | `0x15` | JSON `{"model":string,"effort":string}` |
| runner -> daemon | HELLO | `0x20` | JSON `RunnerHello` |
| runner -> daemon | OUTPUT | `0x21` | 4-byte BE `seq`, then UTF-8 chunk |
| runner -> daemon | EXIT | `0x22` | JSON `{"code":number|null,"signal":string|null,"seq":number}` |
| runner -> daemon | SNAPSHOT_RES | `0x23` | serialized xterm buffer as UTF-8 |
| runner -> daemon | REPLAY_DONE | `0x24` | empty |
| runner -> daemon | STRUCTURED | `0x25` | one normalized provider event as JSON |
| runner -> daemon | MODEL_RES | `0x26` | `{}` or JSON `{"error":string}` |

## Runner to daemon frames

### HELLO (`0x20`)

Sent immediately on every accepted socket connection:

```json
{
  "id":"2f577cd7-565b-4861-8ea2-c77c39a20e24",
  "cmd":"/bin/zsh",
  "args":["-l"],
  "cwd":"/Users/example/project",
  "cols":300,
  "rows":50,
  "createdAt":1750000000123,
  "pid":43210,
  "currentSeq":42,
  "protocolVersion":2,
  "runtimeVersion":"0.2.3"
}
```

Fields and types are exact:

- `id`, `cmd`, and `cwd`: strings
- `args`: string array; this is the configured/original argument array
- `cols`, `rows`, `createdAt`, `pid`, and `currentSeq`: numbers
- `protocolVersion`: optional number for compatibility; current runners send 2
- `runtimeVersion`: optional Sessions release string; legacy runners omit it

Current protocol version is 2. The daemon treats a missing version as 0 and
accepts versions 0 through 2. It rejects an explicitly unsupported version
immediately after HELLO, before replay, input, resize, snapshot, or kill frames.
This preserves immutable pre-versioned runners without guessing that unknown
future frame semantics are safe. `createdAt` is the current runner process's
start time from its metadata object, so it resets on a runner respawn.
`cols`/`rows` are the live PTY object's current values;
`currentSeq` is the in-memory log's latest sequence after disk restoration and
any non-persisted restore notices.

The daemon's `RunnerClient.connect()` requires HELLO within 2,000 ms by default.
It resolves the connection Promise on HELLO but also exposes HELLO as an event.

### OUTPUT (`0x21`)

Payload layout:

```text
offset  width  encoding    value
0       4      uint32, BE  seq
4       rest   UTF-8       terminal output chunk
```

A payload shorter than four bytes is a decode error; the daemon's socket-data
handler destroys the connection. Chunks preserve node-pty callback boundaries
and need not align to lines, code points at higher logical layers, or ANSI
sequences. Sequence starts at 1 for an empty history. Disk-restored sequences
advance the next number to one greater than the highest restored value.

On PTY data, the runner performs these calls in order: append to its in-memory
log, update activity/mirror state, enqueue the persistent record, then broadcast
OUTPUT. The persistent append is queued for a `setImmediate` synchronous batch
flush; despite being invoked first, the bytes need not already be on disk when
the socket write occurs.

### EXIT (`0x22`)

Payload:

```json
{"code":0,"signal":null,"seq":42}
```

`code` is number or null, `signal` is string or null, and `seq` is the current
sequence number. A numeric node-pty signal is converted with `String(signal)`.
The runner broadcasts EXIT when the PTY exits and sends the same saved EXIT to
clients that connect afterward. It keeps its socket available until no clients
remain, then waits 30 seconds before process cleanup. If clients are already
absent at PTY exit, that 30-second timer starts immediately.

### SNAPSHOT_RES (`0x23`)

Payload is the UTF-8 result of xterm-headless serialization with 1,000 rows of
scrollback. The protocol has no request identifier. `RunnerClient` sends
requests as called, queues Promise resolvers, and matches responses in FIFO
arrival order. An unsolicited response emits a `snapshot` event. Disconnect
resolves every outstanding waiter with the empty string.

The current HTTP/WS snapshot API normally serializes the daemon-side mirror
instead; this runner frame remains part of the interop protocol.

### REPLAY_DONE (`0x24`)

Empty payload. Terminates the OUTPUT sequence generated for one REPLAY_REQ.
There is no request identifier, so replay requests are expected to be ordered.

### MODEL_RES (`0x26`)

Protocol-2 Rich runners send exactly one response for each MODEL_REQ, in request
order. `{}` acknowledges that the runner updated both its provider defaults and
durable metadata. `{"error":"..."}` rejects the change; the daemon leaves its
exposed model and arguments unchanged. A disconnect or five-second timeout also
fails the request rather than guessing that it succeeded.

## Daemon to runner frames

### INPUT (`0x10`)

Payload bytes are decoded as UTF-8 and passed to `pty.write` when the PTY has not
exited. After exit they are ignored. No acknowledgement is sent at this layer.

### MODEL_REQ (`0x15`)

Protocol-2 structured runners accept
`{"model":"<exact model>","effort":"<level>"}`. The daemon validates the
choice, sends this request only to an idle Rich session, and updates exposed
session state only after MODEL_RES acknowledges both provider state and durable
metadata. Structured runners persist the effective arguments before the next
HELLO; a Codex runner also updates its app-server conversation defaults, while
a Claude runner applies the arguments to its next `claude -p` turn. Protocol
0/1 and terminal runners never receive model requests. Ordinary terminal input
behavior is unchanged.

### RESIZE (`0x11`)

Payload is JSON:

```json
{"cols":120,"rows":40}
```

Malformed JSON or values where either field is not a number are ignored. The
runner itself does **not** clamp, floor, or reject NaN/infinity explicitly;
normal daemon WS input has already clamped dimensions. For valid numeric
fields, it resizes the PTY if live, always resizes the runner's xterm mirror,
and mutates the in-memory metadata `cols`/`rows`. It does not rewrite the `.json`
metadata file, so on-disk values remain the startup dimensions.

### SNAPSHOT_REQ (`0x12`)

Payload is conventionally empty but is not validated. The runner replies with
one SNAPSHOT_RES containing its current serialized terminal.

### REPLAY_REQ (`0x13`)

The first four payload bytes are unsigned BE `afterSeq`; extra bytes are
ignored. A payload shorter than four bytes is silently ignored with no
REPLAY_DONE. Otherwise the runner sends every currently retained in-memory
event whose `seq > afterSeq`, oldest first, as OUTPUT frames, then one empty
REPLAY_DONE. If `afterSeq` predates the in-memory ring, the runner does not send
a gap frame: it simply sends all retained events. Gap detection exists only in
the daemon-to-browser WS layer.

### KILL (`0x14`)

Payload is conventionally empty but is not validated. The runner calls
`pty.kill()` and ignores an exception. PTY exit then drives the normal EXIT and
delayed cleanup path; KILL does not directly close the socket or send an ack.

## Daemon registration handshake

For both a newly created runner and startup discovery, sessionsd:

1. connects to the Unix socket and waits up to two seconds for HELLO;
2. accepts protocol versions 0 through 2 and rejects values outside that range;
3. creates a local 4 MiB EventLog and 5,000-row xterm mirror sized from HELLO;
4. installs OUTPUT/EXIT/disconnect listeners;
5. sends REPLAY_REQ with `afterSeq=0`;
6. waits for REPLAY_DONE, disconnect, or a 10-second timeout;
7. returns the registered session even when the replay wait timed out.

Replay OUTPUT frames retain the runner's exact sequence via `pushAt`. A clean
EXIT updates session state and disconnects. A socket close without EXIT emits a
daemon `runner-lost` event and marks the registered session unreachable without
ending or removing it. Conservative reattach attempts start after 1, 3, and 10
seconds and repeat the final delay; periodic discovery may reconnect sooner.
The recovered connection atomically replaces the unreachable one under the
same Sessions ID.

## `.events` on-disk records

The `.events` format is related to, but **not the same as**, socket framing. It
has no type byte:

```text
offset  width  encoding       value
0       4      uint32, BE     R = 4 + UTF-8 data byte length
4       4      uint32, BE     seq
8       R-4    UTF-8          terminal output chunk
```

The record length excludes its own four-byte prefix and includes the four-byte
sequence. Therefore a socket OUTPUT containing N data bytes occupies `9 + N`
bytes (`4 length + 1 type + 4 seq + N`), while the disk record occupies `8 + N`
bytes (`4 length + 4 seq + N`). There is no on-disk record type, timestamp,
protocol version, checksum, magic, file header, or trailer.

See `fixtures/events.hex.txt` for byte-exact examples.

### Restore and corruption behavior

The restore reader processes from byte zero while at least four bytes remain.
It stops, preserving earlier records, when:

- `R < 4`, or
- fewer than `R` bytes remain after the length field.

A trailing fragment shorter than four bytes is also ignored. There is no file
size or per-record maximum check in the restore parser beyond the bytes present
in the loaded file. Sequence ordering and uniqueness are not validated.

At runner startup, restored records are pushed into the in-memory EventLog and
xterm mirror. When at least one exists, the runner adds a visible replay banner
to memory/mirror with the next sequence number, but deliberately does not write
that banner back to `.events`. A missing-Claude-JSONL warning is likewise
memory-only.

### Append, cap, and trim

Each PTY chunk becomes one record. `append()` pre-encodes it and queues a
`setImmediate` flush. A flush concatenates all pending records and performs one
synchronous `writeSync` to an append/read file descriptor. `close()` synchronously
flushes pending records before closing. A failed batch write is dropped and
counted; it is not retried.

The soft cap is 16 MiB. After a flush makes the file larger than that, the
runner synchronously rewrites it at whole-record boundaries, retaining the
latest records totaling at most 8 MiB. If no individual record fits, it keeps
only the last record. The rewrite is `<events>.tmp` with mode 0600 followed by
an atomic POSIX rename and fd reopen. `open()` deletes a stale `.tmp` first.

### Lifecycle

- Normal PTY termination (including KILL) marks the session ended. After the
  exit hold, cleanup removes `.sock`, `.json`, and `.events`. One deliberate
  exception exists on a Claude respawn when its backing JSONL was not found:
  the runner leaves `sessionEnded=false`, so the failed resume's events remain
  available for inspection/recovery.
- SIGTERM cleanup leaves `.events` in place for launchd respawn but removes
  `.sock` and `.json`.
- SIGINT and SIGHUP are intentionally ignored by the runner.
- The runner code does not unlink its `.log` file in either cleanup flavor.
