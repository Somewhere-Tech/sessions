# Sessions-owned network and outbound security

Status: **product security policy.** Sessions is local by default. This policy
applies to network traffic initiated by Sessions.app, `sessionsd`, and the
`sessions` CLI. It does not claim that Claude, Codex, a shell, or an agent's own
tools are offline; those subprocesses retain the permissions the user gave
them.

## Default

- Starting, viewing, searching, tagging, and measuring local sessions requires
  no Somewhere account and sends no transcript, prompt, terminal output, usage
  event, or telemetry to Somewhere.
- A new Sessions-owned outbound data path must be visible and attributable. Its
  implementation and UI must name the destination, trigger, payload class,
  credential source, retention expectation, timeout/retry behavior, and how the
  user disables or revokes it.
- Data-bearing outbound features are opt-in. An update check may fetch public
  release metadata automatically, but it must not attach local session data or
  a durable machine/user identifier.
- Sessions does not add third-party analytics, advertising SDKs, or silent crash
  uploads. Local diagnostics stay local until the user previews and explicitly
  sends them.
- LAN, Tailscale reachability, pairing, the optional Somewhere fleet account,
  a future cloud worker, backup, and support access are separate capabilities.
  Tailscale reachability is on by
  default when this Mac is already signed in to Tailscale, and has its own
  opt-out; it never enables the trusted-LAN listener or creates a
  general-purpose tunnel.

## Optional Somewhere fleet account

The account tier is opt-in during host onboarding and in Settings › Fleet; the
CLI equivalent is `sessions account login`. Skipping it is a complete setup and
does not disable loopback, trusted-LAN, Tailscale, discovery, pairing, or local
session work. Sign out revokes the Somewhere auth session and removes this
machine's directory row; it does not disable either local transport or end a
running session.

sessionsd sends the email address only to the `sessions-fleet` authentication
functions to request and verify a Somewhere magic link. It stores the returned
access, refresh, and logout-session tokens together in
`fleet-account.json` under the daemon state root. The file is replaced
atomically with mode `0600`. Every authenticated request sends the access token
and the refresh token. sessionsd adopts a rotated pair only when the same
response contains both `X-New-Access-Token` and `X-New-Refresh-Token`; a lone
header is ignored, requests are serialized around rotation, and a network
error never clears the stored login.

The machine's Ed25519 private/public key pair is stored separately in
`fleet-machine-key.json`, also atomically written with mode `0600`. This first
account slice deliberately uses a private state file; moving the private key to
the OS keychain is later hardening. `sessions account key` exposes only the
unpadded-base64url public key.

After sign-in, the platform sees only the app-user identity and owner-scoped
machine directory metadata: the computer name, stable machine ID, enabled LAN,
tailnet, and relay endpoint hints, machine public key, daemon version, and
last-seen time. It never receives provider credentials, prompts, responses,
transcripts, terminal bytes, session names, paths, tags, search indexes, usage
events, logs, or the machine private key. The daemon registers after login and
startup and sends a heartbeat every five minutes.

Every machine write carries the Somewhere app-user access/refresh pair and an
Ed25519 signature over the machine ID, Unix timestamp, nonce, method, path, and
SHA-256 of the exact request body. The platform rejects timestamps outside five
minutes, stores accepted nonces in an owner-scoped replay table, and rate-limits
heartbeat retries. The registration public key may establish a new row; once a
row exists, updates must verify with its stored key. These requests update
directory presence only. A same-account connection is a separate direct
exchange: the requesting device signs the target machine ID, its registered
device ID, timestamp, nonce, method/path, and SHA-256 of the unsigned claim.
The host uses its own Somewhere token to fetch the requester from the same
owner-scoped directory, rejects signatures from absent or different-account
keys, timestamps outside five minutes, and replayed nonces, then issues the
same two-minute-pending, independently revocable device credential as an
accepted access request. The daemon audit line names the device and `via
account`. Somewhere sees the directory lookup but never the issued credential
or later session traffic. The account directory is not the relay service; it
stores only the relay endpoint hint.

Client-only iOS and Android builds have no local daemon state directory. Their
optional account token pair and Ed25519 key live in the application's private
WebView storage; the phone registers an endpoint-free directory identity so a
host can verify its key, and endpoint-free client rows are not presented as
host machines. Signing out removes that identity. The per-host credentials
minted afterward remain in the existing native client machine registry and can
be revoked separately.

## Claude Remote Control consent

Claude Remote Control is an Anthropic capability on Claude's native
interactive CLI. Sessions can make that same locally running conversation
available on claude.ai and the Claude mobile app, using the user's existing
Claude subscription. The connection goes directly from Claude Code to
Anthropic; Somewhere is not a transcript relay.

Sessions recommends the feature during first-run onboarding on a daemon host
but does not enable it until the user explicitly chooses **Enable Remote
Control**. Choosing
**Keep sessions local** is equally complete onboarding. The choice is stored per
Sessions machine and can be changed later in Settings. Fresh installs, upgraded
installs, missing state, old `inherit` values, and old `on` values all remain
local until this current consent record exists. Sessions also passes
`remoteControlAtStartup: false` for a local-only managed launch.

A client-only phone inherits this machine-level choice from its connected host.
It does not present host onboarding or write the setting; host-owned controls
are labelled and read-only on the phone.

The daemon enforces this below the UI: general Claude settings, per-session
overrides, continuations, delegates, and direct `--remote-control` launch
arguments cannot enable the feature without the recorded choice. The
authenticated `sessions onboarding` command exposes the state to people and
agents but deliberately has no command that can grant consent. The user-facing
write endpoint requires an explicit consent header; that is a product-surface
guard, not a claim that another process already authorized with the local
daemon token is cryptographically distinguishable from the app.

Changing this setting affects only newly launched Claude processes. Sessions
does not restart, terminate, or alter existing sessions.

## Delegated task access

The same host onboarding/Settings surface asks whether agent-created children
should receive autonomous full access or inherit a manager's exact
permission mode. Autonomous access is the default; inheritance is the explicit
narrower choice. The daemon resolves the choice below
the UI and CLI, rejects child self-escalation, and applies it only to newly
created children. The authenticated CLI exposes the current state read-only;
an agent cannot grant autonomous consent through the normal command surface.

Autonomous delegated access does not create a Sessions-owned network path. It
changes the provider sandbox or approval mode for the child process, so that
process and its tools may use whatever network access the provider and operating
system allow. The product labels that authority explicitly and never treats
automatic prompt acceptance as equivalent consent. A constrained child waiting
for approval is reported as `needs-input` with the prompt reason; Sessions does
not send Enter on its behalf.

## Cross-machine continuation

Moving a Claude or Codex conversation between approved computers is
client-mediated. The native app or CLI authenticates to source and destination
independently using each saved per-device credential. It never puts either
credential in argv, the webview, the provider transcript, or a request to the
other daemon. The source exports only the selected provider history, safe resume
recipe, bounded workspace metadata, and lineage identifiers. The destination
validates and receives that data before starting one new runtime; source
completion is recorded only after target creation succeeds.

The source provider file is preserved. Sessions does not transfer isolated
profile credentials, environment variables, arbitrary attachments, PTY bytes,
usage databases, or the full ledger. Transport security remains the selected
machine connection's responsibility: prefer Tailscale Serve HTTPS for remote or
untrusted networks, use the direct Tailscale IP fallback when tailnet DNS is
unavailable, and use plain LAN HTTP only on a private network the user trusts.

## Hosted relay fallback

`sessions-relay` is an optional Sessions service the owner hosts, independently
of the Somewhere directory. Each daemon opens one outbound WebSocket and signs
a relay-generated nonce and timestamp with its Ed25519 machine key. The relay
admits that tunnel only when the public key matches the owner's directory or a
static allow-list. Duplicate machine connections replace the old tunnel, many
client streams are multiplexed per machine, each frame is limited to 64 KiB,
and bounded queues apply backpressure. The daemon reconnects with bounded
exponential backoff. `/healthz` exposes only service health and a connected
machine count.

Clients try LAN, Tailscale Serve, the direct tailnet address, and then the
machine-specific relay endpoint. The relay forwards `/api/*` and `/ws` bytes
but grants no Sessions authority. It preserves the presented device credential,
marks the request as forwarded so the destination cannot mistake the relay's
loopback connection for local trust, and the destination daemon performs its
normal credential and Origin checks. A relay-admitted machine key cannot read,
create, send to, or end a session. Revoking a client device on the destination
therefore ends that client's relay access exactly as it ends direct access.

The service stores no request bodies, terminal bytes, or transcripts and logs
only connection events, machine IDs, methods, paths, and errors. This is a
storage and authority boundary, not end-to-end content encryption against the
relay operator: when HTTPS terminates at the relay, a compromised relay host
can observe, delay, drop, replay, or alter relayed bytes. The destination's
device authentication detects no general content tampering. Run the relay only
on infrastructure you trust, protect its TLS key and directory token or static
allow-list, and prefer direct LAN/tailnet routes. Compromise of the directory
token can admit registered machine tunnels but still does not mint a device
credential or bypass the destination daemon.

See [`RELAY.md`](RELAY.md) for deployment and configuration.

## Paired-client fleet relay

A phone is a viewer that inherits the approved fleet of the machine it pairs
with. After that phone authenticates to host A with its own revocable device
credential, A may list only the machines in A's saved `sessions machines`
registry and relay an `/api/*` or `/ws` request to one of those exact endpoints.
A removes the phone credential and supplies A's separate saved credential for
host B. B therefore authorizes and audits A as the paired device it already
approved, and revoking A on B immediately ends the relay path. Forgetting B on
A removes it from the relay allowlist; revoking the phone on A ends all of that
phone's inherited access. No machine accepts a new machine or device because of
the relay.

The destination and payload are visible and attributable: a relay request is
triggered only by a local or paired-device call naming a saved machine; it may
carry the same API body, event response, or WebSocket frames as a direct client.
A logs the method, path, destination machine, and calling device ID, never the
body or either credential. The fleet-list reachability check sends only an
authenticated machine-identity probe and keeps offline machines visible. Each
connection tries the saved LAN origin first, then Tailscale Serve HTTPS, then
the direct Tailscale IP origin. Relayed streams have no background retry queue;
the phone's existing reconnect behavior starts a new request.

This is a user's own machine relaying to that same user's independently
approved machines. It is distinct from the optional `sessions-relay` transport:
host A holds B's credential and substitutes it, while the hosted fallback pipes
the original client's credential to B. Somewhere still operates neither path.

**What we borrowed from T3 Code, and what we deliberately did not.** T3 Code's
public remote model keeps each environment as one intact server/runtime behind
an HTTP/WebSocket connection, while LAN, Tailscale, HTTPS, and desktop-managed
SSH forwarding are connection choices. Sessions uses the same useful UI
property: a relayed machine is still an ordinary server base, so the existing
Fleet, inbox, and conversation paths do not split the runtime. We did not adopt
client-owned SSH launch or direct per-environment credentials for this inherited
phone path: the phone learns no credential for B and gets no new tunnel
authority; A stays the only ingress and may reach B only with B's prior
approval. See T3 Code's
[remote architecture](https://github.com/pingdotgg/t3code/blob/main/docs/internals/remote.md)
and [remote-access guide](https://github.com/pingdotgg/t3code/blob/main/docs/user/remote-access.md).

## Review checklist for an outbound feature

1. Is the destination allowlisted and is TLS/authentication fail-closed?
2. Can the payload be metadata instead of transcript or terminal content?
3. Does the UI show the action before the first data-bearing request?
4. Are credentials referenced from their owner rather than copied into prompts,
   workspaces, runner environments, or logs?
5. Are requests bounded, cancelable, rate-limited where appropriate, and free
   of unbounded retry queues?
6. Can the user turn it off and revoke server-side capability without
   reinstalling Sessions?
7. Do tests prove local-only operation still works with the network unavailable?

## Automatic tailnet reachability

When the Tailscale CLI is installed and reports a signed-in backend,
`sessionsd` makes the machine reachable without a separate Sessions action. It
configures Tailscale Serve HTTPS for the daemon's loopback origin and listens on
the daemon port at the exact IPv4 address in Tailscale's `100.64.0.0/10` range.
It checks on startup, after a network-interface change, and periodically so a
later Tailscale sign-in is picked up. A missing or signed-out Tailscale install
is routine and produces no prompt or alarming error. Settings › Fleet ›
**Reachable over Tailscale automatically** is on by default; turning it off
stops the Tailscale-IP listener and removes the Sessions-owned Serve root.

The HTTPS name is the preferred remote transport. The direct
`http://100.x.y.z:8787` origin exists for peers whose Tailscale tunnel works but
whose MagicDNS resolver is not applied. Plain HTTP is acceptable on that exact
interface because Tailscale authenticates the peer and encrypts packets before
they traverse the physical network. Sessions additionally requires the same
revocable device credential for protected API and WebSocket routes. Bootstrap
still requires either explicit request approval or possession of a one-time
pairing ticket.

The direct listener is never wildcard-bound and never listens on Wi-Fi,
Ethernet, public, or arbitrary private addresses: both Tailscale status parsing
and listener creation independently require `100.64.0.0/10`. The daemon
publishes LAN, Tailscale HTTPS, direct Tailscale-IP, and optional relay origins
as distinct endpoint kinds. Clients try `lan`, `tailnet`, `tailnet-ip`, then
`relay`; a macOS Local Network denial falls through silently and is logged once
with the transport that was selected.

## One-time pairing tickets

Settings › Fleet › **Pair a device** and `sessions pair [--ttl 10m]` mint the
same daemon-owned ticket. Its secret is 32 random bytes encoded as base64url,
exists only in memory, is single use, and expires after ten minutes by default.
The caller may shorten but not extend that lifetime. An unused ticket can be
revoked locally, and every daemon restart revokes all outstanding tickets.
Used, expired, revoked, malformed, and unknown values deliberately produce the
same `410 Gone` sentence.

The QR and `sessions://pair` link record all enabled `lan`, `tailnet`, and
`tailnet-ip` endpoints in that order. The plain `/pair/<ticket>` fallback uses
Tailscale HTTPS when available and otherwise the trusted-LAN HTTP origin. A
native claimant validates every origin, probes endpoints in recorded order,
follows no redirect, and posts the ticket in a JSON body to
`/api/lan/access/claim`; it never puts the daemon master token on the wire. The
daemon-served browser fallback may make that one same-origin claim. Other
browser origins and ordinary request/accept claim secrets remain rejected.

Possession is explicit host consent and immediately mints a separate
host-administrator device credential—there is no second accept action. Failed
ticket guesses are rate-limited per source with a bounded global backstop.
Successful claims log `paired via ticket <short> from <device name>` in the
same audit stream as accepted or denied discovery requests, without logging the
ticket, device token, or request body. Durable device storage contains only a
SHA-256 token hash. Settings › Fleet **Forget** and `sessions devices revoke`
invalidate that credential immediately.

## Nearby Bonjour discovery and LAN access

Bonjour is a low-sensitivity discovery hint, not authentication. `sessionsd`
advertises `_sessions._tcp` only while the user-enabled LAN listener is active
and names only that selected private IPv4 address as its target. On macOS the
daemon registers the proxy record through Apple's system Bonjour responder;
other platforms use the same record contract through the bundled mDNS
implementation. The record carries the friendly instance name, LAN IP/port,
the current Tailscale HTTPS and direct-IP origins when available, protocol
marker, HTTP transport marker, and “approval required.” It carries no
credential, account, full machine ID, session metadata, workspace, usage, or
filesystem path. Any peer on a local link where the operating system publishes
Bonjour can observe or spoof such a record, even when the selected target
address is unreachable from that link.

The host app and CLI ask their loopback `sessionsd` to browse and verify
`/api/health` before presenting a candidate. Selecting a discovery result uses
the separate request/accept/claim flow; a pairing link instead uses the
possession flow above. The daemon also owns the outbound peer connection
for `sessions machines connect`, `sessions --machine`, cross-machine grep, and
conversation moves. A lane therefore talks only to its local daemon and needs
no local-network permission. The global `--direct` flag is an explicit
diagnostic escape hatch that restores client-side browsing and peer dials.
Nearby bootstrap routes:

- exist on the dedicated LAN listener, plus the main listener only for a true
  loopback peer that already has local-user authority;
- require a private IPv4 network peer or local loopback and `application/json`;
- reject every browser `Origin`, except the same-origin one-time ticket claim;
- bind the short-lived request secret to the observed source address;
- are bounded by the shared pending-request limits; and
- issue only a per-device revocable token after authenticated local host
  approval.

An approved machine is not a passive viewer. Its revocable per-device token is
a host-administrator credential used by the native client and CLI to create,
send to, and end sessions. Those actions run with the local Sessions user's
authority and can therefore execute commands on that computer. Approve only a
device you control, prefer a Tailscale transport outside a private LAN, and
revoke a lost device. Sessions does not currently issue a read-only pairing
token.

LAN traffic is plain HTTP. Credentials and later session traffic are therefore
not confidential against a hostile observer on shared Wi-Fi even though API
authorization is required. The product labels this “trusted private network,”
does not recommend it for hotels, cafés, or other shared networks, and keeps
Tailscale Serve HTTPS as the preferred remote/untrusted-network path. A
Bonjour failure does not disable the listener; disabling LAN access also stops
the advertisement.

The agent surface has the same boundary. `sessions machines connect` accepts
only private or loopback IPv4 HTTP origins, `.ts.net` HTTPS origins, or HTTP
origins in `100.64.0.0/10`; it follows no redirects, never sends the local
daemon token to a candidate, and stores an issued token in a separate mode-0600
file. The metadata registry contains no credential. `sessions --machine` reads
that file through the local daemon's fleet relay, while `sessions access`
exposes the same pending host decisions as the native inbox. The low-level
global `--host` flag uses the local daemon token only for a loopback target; a
non-loopback raw host receives no local credential.

On macOS 15, Local Network privacy applies to launchd agents as well as apps.
Darwin release binaries embed an Info.plist section with their stable bundle
identifier, `NSLocalNetworkUsageDescription`, and
`NSBonjourServices = [_sessions._tcp]`; the app-installed launch agent is
associated with the signed Sessions app bundle so macOS attributes sessionsd's
request to the visible app, following
[Apple TN3179](https://developer.apple.com/documentation/technotes/tn3179-understanding-local-network-privacy).
First-run Fleet onboarding and Settings › Fleet ›
**Allow local network** start a daemon-owned Bonjour browse while the person is
looking at that surface. macOS 14 accepts the same binary metadata but does not
enforce the macOS 15 Local Network gate.

Apple provides no supported API to preflight this permission or force its
prompt. `sessions doctor` therefore reports the daemon's last observed state:
`granted`, `denied`, or `not-yet-asked` (`not-required` on other platforms).
Successful nearby discovery or connection records `granted`; a private or
link-local Darwin dial failing with `EHOSTUNREACH`, or an empty Bonjour browse
while this daemon is itself advertising, records `denied`. The API, CLI,
doctor, and Fleet banner replace the misleading route error with:
“macOS has not allowed Sessions to use the local network. System Settings ›
Privacy & Security › Local Network › turn on Sessions.” Tailnet addresses are
outside this classification and remain exempt.

## Native update traffic

Automatic update awareness and `sessions update --check` send only an HTTPS
GET with a non-identifying updater user agent to the fixed public release route:
`sessions.somewhere.tech` redirects to the allowlisted deployed host
`sessions.somewhere.site`. They send no token, cookie, account credential,
machine ID, session ID, usage, transcript, prompt, terminal content, or
telemetry. Installation is explicit.

`sessions update` then accepts only the exact immutable GitHub release path for
the announced version; GitHub's asset redirect is restricted to
`release-assets.githubusercontent.com`. Redirect count and response sizes are
bounded. Even a compromised mutable manifest cannot authorize executable
bytes: the archive must verify with the public key compiled into the CLI, then
the app must pass Developer ID/team/bundle/version and Gatekeeper checks before
the atomic swap. There is no URL, key, proxy credential, or app-path option in
the command surface. The system HTTPS proxy setting may be honored, but TLS
validation and the artifact signature remain mandatory.

## Claude and Codex version controls

Sessions checks the installed Claude Code and Codex versions by resolving the
local executables and running their local `--version` commands. Codex update
awareness additionally reads its bounded local version cache. The automatic
check does not call a package registry, provider API, or Somewhere service
(`runtime/internal/api/providers_handlers.go`).

Installing a provider update is a separate explicit action in Settings or
`sessions providers update claude|codex`. That action invokes only the
resolved provider executable with its own fixed `update` subcommand, so the
provider updater may access the internet and replace that provider's local
binary. The mutating endpoint accepts a loopback client, the master token, or
an explicitly paired device; anonymous open-access clients may inspect status
but cannot trigger installation. Remote clients name the destination machine
before invoking the action. Sessions accepts no provider path, URL, package,
or shell fragment from the caller. Already-running provider processes continue
unchanged; sessions created later resolve the updated executable
(`runtime/internal/api/providers_handlers.go`,
`runtime/cmd/sessions/providers.go`).

## Feedback and support tickets

The source implementation provides two user-controlled entry points:

- Sessions.app Settings → Help & feedback accepts a local draft, can generate
  an optional diagnostic preview, copies only the reviewed draft to the
  clipboard, and opens a fixed public GitHub feedback or bug form. It never
  submits the form.
- `sessions support` prints the same public-ticket and private-security-report
  destinations. `sessions support --diagnostics` previews the small diagnostic
  object locally and never uploads it. `sessions support --bundle PATH` writes
  that exact redacted object to a new owner-readable file and refuses to
  overwrite an existing path. Agents use
  `sessions --json support --diagnostics`; the returned contract tells them to
  capture only the sanitized shape of the failing Sessions command/action, exit
  code, sanitized exact error, expected/actual behavior, and repeatability, then require user approval
  before opening or submitting a report.
- After that approval, and only after it, an agent or user can run
  `sessions support --attach --ticket tsk_ID --project somewhere-project`.
  Sessions invokes the installed Somewhere CLI once, using its existing local
  login, without exposing project environment variables. A temporary
  owner-readable script carries only the redacted diagnostic object, writes one
  private `/support/<ticket>/...json` project file, and appends that path to the
  exact named ticket. The temporary script is removed locally. A failed ticket
  update deletes the uploaded file; a deterministic content path makes a
  repeated identical request idempotent.

The diagnostic schema contains only generation time, Sessions CLI/daemon
versions, OS/architecture, daemon reachable/ready/discovery state, and a
session count. It deliberately excludes transcripts, terminal output, prompts,
responses, titles, tags, commands, session/process IDs, usernames, hostnames,
paths, tokens, credentials, environment variables, provider configuration,
logs, and crash files. The command succeeds with an explicit unreachable state
when the daemon is down, so support never depends on the broken component.
The attach command makes one attempt, caps the local operation at 45 seconds
and the remote script at 10 seconds, and never retries in the background. The
private bundle remains in the Somewhere project and on the ticket until the
account owner or project team removes the attachment and file. No public or
signed download URL is created.

The native shell accepts only the compiled-in GitHub feedback, bug, chooser,
and private security-advisory destinations; the webview cannot supply an
arbitrary URL. Public ticket forms repeat the privacy warning and require the
reporter to confirm review before submission. They also identify whether the
failure or feedback came from an agent workflow, direct use, or both; agent
origin does not weaken the approval or privacy boundary.

Temporary live support access remains unimplemented. If it is ever justified,
the user must authenticate through the Somewhere CLI and select an exact
ticket; the grant must be separately confirmed, read-only by default, scoped
to named machines/sessions/capabilities, short-lived, revocable, and audited.
It must never expose unrelated sessions, provider credentials, environment
variables, arbitrary filesystem paths, or a master daemon token, and it must
never create an unattended listener or permanent reverse tunnel.
