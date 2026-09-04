# Sessions relay

`sessions-relay` is an optional fallback you operate. It is a long-running
Sessions service, not a Somewhere function. A daemon connects outbound to the
relay, so the host needs no inbound firewall rule; clients use
`https://relay.example/m/<machine-id>/api/...` and `/ws` only after LAN and
tailnet routes fail.

The relay multiplexes many client streams over one WebSocket per machine. A
machine signs a fresh challenge with its Ed25519 fleet key. The relay admits
that tunnel only when the public key matches either the owner's Somewhere
directory or a static allow-list. That decision permits a pipe; it does not
authorize a Sessions API call. The destination daemon still verifies the
client's normal revocable device credential on every HTTP request and
WebSocket connection.

The relay does not persist request bodies, terminal output, or transcripts and
does not log them. It logs connection events, machine IDs, methods, paths, and
errors. Frames are capped at 64 KiB, stream queues are bounded for backpressure,
and idle HTTP connections expire. Because TLS normally terminates at the relay,
an operator with control of that host can observe or alter relayed bytes. Treat
the relay host as infrastructure you trust; device authentication prevents it
from becoming API authority, but is not content encryption against that host.
See [Network security](NETWORK_SECURITY.md#hosted-relay-fallback).

## Run it

Put TLS on the process itself:

```sh
sessions-relay --listen :443 \
  --cert /etc/sessions/fullchain.pem \
  --key /etc/sessions/privkey.pem \
  --allow-file /etc/sessions/relay-allow.json
```

Or bind loopback and publish it with Tailscale Serve or Caddy:

```sh
sessions-relay --listen 127.0.0.1:8899 \
  --allow-file /etc/sessions/relay-allow.json
```

The same mode is available as `sessionsd --relay`. `/healthz` reports process
health and the current connected-machine count.

The static file is re-read on every machine authentication, so rotation applies
to the next connection without restarting the service:

```json
{
  "machines": {
    "machine-id-from-sessions-account-key": "unpadded-base64url-public-key"
  }
}
```

For an account-backed allow-list, use the owner-scoped directory and keep its
token in a mode-`0600` file:

```sh
sessions-relay --listen 127.0.0.1:8899 \
  --directory-url https://sessions-fleet.somewhere.site \
  --owner-token-file /etc/sessions/owner-token
```

Configure each host in Settings › Fleet, with `sessions relay set
https://relay.example`, or through the local daemon API's `PUT /api/relay` with
`{"url":"https://relay.example"}`. `sessions relay status` reports the selected
source and live connection; `sessions relay disable` clears the explicit
setting. A relay address
already registered for that account is also discovered from the machine's
directory row. The daemon advertises its `/m/<machine-id>` endpoint on the next
registration.

## macOS launchd

The CLI writes and bootstraps the equivalent LaunchAgent:

```sh
sessions relay install --listen 127.0.0.1:8899 \
  --allow-file "$HOME/.config/sessions/relay-allow.json"
```

The generated plist uses `tech.somewhere.sessions.relay`, keeps the relay alive
after crashes, and sends logs to `~/Library/Logs/sessions/`. A manual plist has
this shape:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>tech.somewhere.sessions.relay</string>
  <key>ProgramArguments</key><array>
    <string>/usr/local/bin/sessions-relay</string>
    <string>--listen</string><string>127.0.0.1:8899</string>
    <string>--allow-file</string><string>/Users/example/.config/sessions/relay-allow.json</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/Users/example/Library/Logs/sessions/relay.log</string>
  <key>StandardErrorPath</key><string>/Users/example/Library/Logs/sessions/relay.log</string>
</dict></plist>
```

## Docker

Build the checked-in [`Dockerfile`](../deploy/relay/Dockerfile) from the
repository root, mount an allow-list, and terminate public TLS at the platform
load balancer or reverse proxy:

```sh
docker build -f deploy/relay/Dockerfile -t sessions-relay .
docker run --read-only --tmpfs /tmp -p 8899:8899 \
  -v "$PWD/relay-allow.json:/etc/sessions/relay-allow.json:ro" \
  sessions-relay --listen :8899 --allow-file /etc/sessions/relay-allow.json
```
