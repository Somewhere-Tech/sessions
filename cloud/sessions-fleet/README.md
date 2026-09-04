# sessions-fleet

This is the optional Somewhere account and machine-registration service for
Sessions. It stores account-owned directory metadata only: machine name,
reachable endpoint hints, the machine Ed25519 public key, daemon version, and
last-seen time. It never receives session content or provider credentials.

## Deploy

The project owner performs the first link and deploy from this directory:

```sh
cd cloud/sessions-fleet
somewhere init --name sessions-fleet
somewhere deploy
```

For an existing project, use `somewhere link`, review `somewhere status`, then
run `somewhere deploy`. A deploy applies `db/schema.ts`, including the
`owner()` scopes on `machines` and the nonce-replay table. Do not replace the
managed schema with raw migrations.

Sessions defaults to `https://sessions-fleet.somewhere.site`. A local or
staging daemon may point at a different origin with `SESSIONS_FLEET_URL`.

## Local smoke

In one terminal, start the platform compiler/runtime without deploying:

```sh
cd cloud/sessions-fleet
somewhere dev
```

In another terminal, use a disposable account email and the localhost URL
printed by `somewhere dev`:

```sh
SESSIONS_FLEET_SMOKE_URL=http://127.0.0.1:5173 \
SESSIONS_FLEET_SMOKE_EMAIL=you@example.com npm run smoke
```

Paste the emailed code or link token when prompted. The smoke signs in,
registers an ephemeral Ed25519 machine, heartbeats, lists and reads it, removes
it, and logs out. `SESSIONS_FLEET_SMOKE_CODE` can supply the code in a test
harness. Local database access from `somewhere dev` depends on the owner's
Somewhere plan; the CLI reports that limitation at startup.

## Signed machine requests

Machine routes require both the app-user access/refresh pair and these headers:

- `X-Sessions-Machine-ID`
- `X-Sessions-Timestamp` (Unix seconds, within five minutes)
- `X-Sessions-Nonce` (single use)
- `X-Sessions-Signature` (unpadded base64url Ed25519 signature)

The signed bytes are the UTF-8 concatenation of:

```text
machine_id + timestamp + nonce + method + URL pathname + hex(sha256(exact body bytes))
```

The registration route verifies a new machine with the public key in its body;
an existing row must verify with its stored key. Every accepted nonce is stored
in the caller's owner-scoped replay table. Heartbeat additionally allows twelve
requests per machine per minute, leaving room for bounded retries around the
normal five-minute daemon interval.
