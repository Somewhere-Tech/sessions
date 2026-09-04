# Fleet

Sessions has two additive fleet tiers. The default needs no account: machines
find each other over a trusted LAN or the user's own tailnet, and pair with a
one-time code. An optional Somewhere sign-in registers this machine in the
user's account so the same fleet directory can appear on every signed-in
device. Signing in does not move sessions, provider credentials, transcripts,
terminal output, search indexes, or usage records off the machine.

## Optional Somewhere account

First-run onboarding offers **Sign in to Somewhere** and an equally complete
**Skip — Sessions works on this network without it** path. The same choice is
available later in Settings › Fleet or from the CLI:

```sh
sessions account login
sessions account status
sessions account key
sessions account logout
```

Login asks for an email, sends a single-use magic-link code, and waits for that
code. The daemon stores the returned access/refresh pair in its private state
directory and creates one Ed25519 machine key there; both files are written
atomically with mode `0600`. The private key never leaves the machine. A later
release may move it into the OS keychain without changing its public identity.

After login, sessionsd registers the computer name, currently known LAN,
tailnet, and relay endpoint hints, machine public key, daemon version, and
last-seen time. Registration requests are signed by the machine key and carry a
single-use nonce with a five-minute timestamp window. sessionsd heartbeats every
five minutes. `sessions doctor` and `/api/health` report whether the daemon is
signed in and its last registration result.

Somewhere hosts sign-in and this owner-scoped directory. It does not receive
session content and does not become a terminal or transcript relay. A signed-in
device lists the directory, signs a short-lived challenge with its registered
key, and presents it directly to the selected machine. That host fetches the
requester's public key with its own account token; only a key in the same
owner-scoped directory can receive a normal, independently revocable device
credential. A signature from another account never bypasses pairing.

`sessions machines` merges saved pairings, live Bonjour results, and account
directory rows by stable machine identity and endpoint. Each row includes all
known `lan`, `tailnet`, `tailnet-ip`, and future `relay` candidates plus the
transport currently in use. The Fleet view uses the same shape: an offline
directory machine stays visible, while a reachable same-account machine is
credentialed automatically without an accept prompt.

On iOS and Android, **Sign in** appears beside **Scan a pairing code** and the
manual address path. The phone stores its account token pair and signing key in
its app-private WebView storage, registers only a client identity (no session
metadata), and then loads the fleet without pairing each host. Signing out
removes that directory identity; already saved device credentials remain
individually revocable on their hosts.

## No-account tier

Sessions machines find and trust each other directly. The no-account tier has
no Somewhere relay, broker, or credential exchange: a client connects to a
daemon over a trusted LAN or the user's own Tailscale network, and session data
stays on that path.

### How machines are found

A host can publish up to three endpoint kinds. Every discovery record, pairing
link, saved machine, and fleet relay keeps them distinct and tries them in this
order:

1. `lan` — HTTP on a private Wi-Fi or Ethernet network explicitly enabled in
   Settings › Fleet.
2. `tailnet` — HTTPS through Tailscale Serve and the machine's MagicDNS name.
3. `tailnet-ip` — HTTP on the machine's `100.64.0.0/10` address. Tailscale still
   authenticates and encrypts this traffic; this route exists when MagicDNS is
   unavailable.

Bonjour advertises the endpoint hints but no credential or session metadata.
Clients verify `/api/health` before presenting or selecting a candidate. A
saved machine retains all known routes so a network change does not require
pairing again.

### Pair by possession

On the host, choose Settings › Fleet › **Pair a device**, or run:

```sh
sessions pair
sessions pair --ttl 5m --name "My phone"
```

Sessions shows a large QR code, a `sessions://pair?...` application link, and a
plain `/pair/<ticket>` browser fallback. The application link records every
endpoint kind available on the host in connection order. The default and
maximum lifetime is ten minutes.

Scanning or opening the link is the consent step. A native client probes the
recorded endpoints in order, sends the ticket only to the selected host, and
immediately receives its own device credential. There is no second
`sessions access accept` step. On a Mac, the equivalent command is:

```sh
sessions machines connect '<pairing-link>'
```

On iOS and Android, **Scan a pairing code** opens the system camera permission;
the paste-link field remains available when scanning is inconvenient. A plain
browser fallback is served by the daemon itself and claims only against that
same origin.

Treat an unclaimed pairing link like a temporary administrator password. It is
single use, disappears on daemon restart, and can be revoked before use from
the countdown card. Used, expired, revoked, and unknown tickets all return one
instructional `410 Gone` response without revealing which condition applied.

### Trust after pairing

Each paired device receives a separate bearer credential, not the daemon's
master token. It is still a host-administrator credential: the device can view,
create, send to, and end sessions with the local user's authority. Settings ›
Fleet lists paired devices; **Forget** revokes one immediately. The CLI
equivalents are:

```sh
sessions devices
sessions devices revoke <id-or-prefix>
```

The host records ticket pairing and manual access decisions in the same daemon
audit log. Pairing does not create an account or copy provider credentials.
For the full threat model and transport rules, see
[NETWORK_SECURITY.md](NETWORK_SECURITY.md).

### Discovery requests remain available

Choosing a Bonjour-discovered machine without a ticket uses the older
request/accept flow. The host sees the observed LAN address or verified
Tailscale identity and decides with the Fleet inbox or `sessions access`.
Pairing codes are the faster path when both devices are in front of the user;
requests remain useful when a link cannot be transferred.
