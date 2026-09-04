# Sessions relay tunnel contract

The optional relay is an HTTP(S) service with two public route families:

- `GET /healthz` returns `{"ok":true,"name":"sessions-relay","machines":N}`.
- `GET /connect` upgrades a daemon's outbound WebSocket.
- `/m/<machine-id>/api/*` and `/m/<machine-id>/ws` forward an ordinary client
  connection to that authenticated daemon tunnel.

## Machine authentication

Immediately after `/connect`, the relay sends one text JSON challenge with a
Unix-second `timestamp` and random base64url `nonce`. The daemon replies with
`machine_id`, its unpadded-base64url Ed25519 `public_key`, and an unpadded
base64url `signature` over these bytes:

```text
sessions-relay-v1 NUL machine_id NUL timestamp NUL nonce
```

The relay verifies the signature and resolves the exact machine/public-key
pair through its owner-scoped directory token or static allow-list. Success is
`{"ok":true}` as a final text frame. All later frames are binary. Failure closes
with WebSocket policy violation and registers no tunnel.

## Multiplexed frames

Each binary frame is a one-byte type, a big-endian uint32 stream ID, then at
most 65,536 payload bytes. Types are `1=open`, `2=data`, `3=end-of-input`, and
`4=close`. The relay allocates non-zero stream IDs; the daemon never opens a
stream. Duplicate opens and malformed or oversized frames close the tunnel.

Stream data is the HTTP/1.1 byte stream for one client request and response.
This includes the WebSocket upgrade and later WebSocket framing for `/ws`.
Stream queues are bounded and WebSocket writes are serialized, so a slow
consumer creates backpressure instead of unbounded memory growth. An idle
stream closes after two minutes by default. Reconnecting a machine replaces
its previous tunnel and closes the previous tunnel's streams.

The relay adds `X-Forwarded-For` and `X-Sessions-Relay-Forwarded: 1`. The daemon
connector independently strips proxy and Tailscale identity headers, replaces
`X-Forwarded-For` with a non-loopback relay marker, and rewrites `Host` to the
actual loopback listener. This does not trust the relay to preserve the remote
authentication boundary. The original Authorization header or WebSocket token
remains intact. The forwarded marker grants no authority; the destination
daemon must authenticate the device credential.
