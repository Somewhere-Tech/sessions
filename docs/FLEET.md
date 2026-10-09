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
session content and does not run the terminal or transcript relay. A signed-in
device lists the directory, signs a short-lived challenge with its registered
key, and presents it directly to the selected machine. That host fetches the
requester's public key with its own account token; only a key in the same
owner-scoped directory can receive a normal, independently revocable device
credential. A signature from another account never bypasses pairing.

`sessions machines` merges saved pairings, live Bonjour results, and account
directory rows by stable machine identity and endpoint. Each row includes all
known `lan`, `tailnet`, `tailnet-ip`, and `relay` candidates plus the
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

Sessions machines find and trust each other directly. The no-account tier needs
no relay, broker, or credential exchange: a client connects to a daemon over a
trusted LAN or the user's own Tailscale network. People who want a fallback
without an account may run `sessions-relay` with a static machine-key
allow-list.

### How machines are found

A host can publish up to four endpoint kinds. Every discovery record, pairing
link, saved machine, and fleet relay keeps them distinct and tries them in this
order:

1. `lan` — HTTP on a private Wi-Fi or Ethernet network explicitly enabled in
   Settings › Fleet.
2. `tailnet` — HTTPS through Tailscale Serve and the machine's MagicDNS name.
3. `tailnet-ip` — HTTP on the machine's `100.64.0.0/10` address. Tailscale still
   authenticates and encrypts this traffic; this route exists when MagicDNS is
   unavailable.
4. `relay` — HTTPS through a Sessions relay the owner operates. The relay is a
   fallback only; the destination daemon still verifies the device credential.

See [Sessions relay](RELAY.md) for deployment, configuration, and the relay
threat model.

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

## Accounts

A subscription is not a machine. One computer can hold several Claude or
ChatGPT logins, each in its own provider home with its own history, and a
session picks one when it starts. Sessions calls those accounts; the on-disk
name is a profile, and `--profile` is unchanged.

**Choosing one.** New Session has an **Account** control beside Agent and
Computer: **Default**, every account on the selected computer, and **Add an
account…**. The list follows the computer, because an account exists on the
machine that holds its login. A project's last choice is remembered per
computer, so a folder that belongs to the work plan keeps using it. A delegated
lane starts on its manager's account, shown as *from this session*, and can be
changed before it starts. Sessions itself makes that choice, so a Claude or Codex
lane an agent starts with `sessions new` also keeps its manager's account unless
it passes `--profile NAME` or `--default-profile`; an account is never carried
to the other provider.

**Per computer.** Each machine card in Fleet lists the accounts that computer
has — the label its owner typed, the provider, and the same login-file fact —
and an account another computer has that this one lacks appears there as **Log
in here too**. That runs the same guided login on the computer whose card it is
under: the home is registered there, the provider's sign-in opens there, and the
instruction to check the account in the browser is the same one.

**Accounts page.** Accounts lists accounts rather than computers. A row names
the account, its provider-reported email and plan, its usage, and the computers
it is **Connected on**, each with its own sign-in state and its own **Check
account**, **Rename** and **Remove**. **Add on <computer>** runs the same guided
login on a computer that lacks the account; nothing is copied between
computers. Homes on different computers become one row only when the provider
reported the same account ID for both. No supported provider reports such an
ID today, so the same subscription signed in on two computers currently shows
as two rows: an email alone does not prove that two homes share a workspace or
organization, so those rows stay separate with a note that the same email
appears elsewhere. **Add account** has a **Computer** choice,
so a second subscription can be set up on the machine that needs it without
pointing the whole app at that machine first.

**Usage.** For ChatGPT accounts, Sessions asks Codex itself, in that account's
private home, for the account's rate limits (`account/rateLimits/read`), with no
model turn. Each limit is its own meter with its used percentage and reset time,
and limits are never added together. When several computers read the same
account, the freshest reading is shown with the computer and time it came from.
A computer that fails or does not answer is never shown as signed out or zero:
it keeps the last reading, marked stale, when the provider still reports the
same stable account ID, and otherwise says usage is unknown. With today's
email-only Codex identity, a failed refresh therefore stays unknown. A computer that could not
be reached is named at the top of the page. Claude usage is not connected in
Sessions yet, so Claude accounts say so and point to Claude for current limits
rather than showing a guess. An older Codex without the method asks to be
updated.

**Adding one.** Settings › Accounts → **Add account** asks for a provider, a
short name for the home, and a label for you to recognise it by. Sessions
creates the home, then opens a session on that computer running the provider
inside it so the login happens in front of you: Claude through `/login`, Codex
through **Sign in with ChatGPT**. Sessions prints what the provider prints and
never handles the credential, the link, or the code. **Check which account you
are signing in as in the browser before confirming** — the provider's page, not
Sessions, is what knows. The account then shows its **login file** as present,
which means the provider wrote its own sign-in file into that home. That is the
only thing Sessions inspects, and it is a weak fact on purpose: Sessions never
opens the file, so a present file is not proof the login still works, an absent
one is not proof it does not — a provider that keeps its credential in the
system keychain shows none — and neither says which account it belongs to.
Subscription logins are the supported form; there is no API-key path.

**Seeing it.** A session that is not on the default account carries its account
name in the session header, the inbox row, and the Lanes panel. Resuming a
conversation keeps the account it was started on. Continuing a conversation on
the other provider asks which account to use, because it is a different
subscription.

**Removing one.** **Remove** takes the account off this computer's list and
leaves the provider home in place, naming the directory so you can review or
delete it yourself. Adding the same name again re-registers it with its login
intact. Sessions has no command that deletes a provider home.

**From the CLI.** `sessions accounts` lists them with labels and a LOGIN-FILE
column carrying the same weak fact,
`sessions accounts add <name> --tool claude|codex [--label TEXT] [--machine
NAME]` performs the same guided login headlessly — with `--machine` it is
relayed to that approved computer, and nothing local to the caller is sent with
it — `sessions accounts usage [--refresh] [--json]` prints each account's
limits, when they were read and whether they are stale, and
`sessions accounts forget <name> --tool claude|codex` unregisters one.
`sessions profiles` remains the same listing.
