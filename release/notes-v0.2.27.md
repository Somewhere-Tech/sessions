# Sessions 0.2.27

- Accepts the unique eight-character ids printed by session, access-request, device, and machine listings everywhere those ids are used; an ambiguous prefix names every candidate instead of guessing.
- Keeps `sessions wait`, multi-target joins, and `sessions fanout` waiting across a sessionsd restart, with one restart notice and bounded reconnect backoff inside the original timeout.
- Restores a Rich lane's exact working state and structured conversation as soon as sessionsd reconnects to its runner, instead of showing a still-running turn as completed or briefly showing an empty transcript.
- Keeps a Rich provider turn running when sessionsd disconnects or restarts; daemon-client cleanup is transport-only and cannot cancel runner-owned work, retries, or approvals.
- Merges saved pairings, Bonjour discovery, and the optional Somewhere directory in `sessions machines` and Fleet, showing every transport candidate and the route in use. Same-account devices use a five-minute signed, replay-protected challenge to receive an ordinary revocable device credential automatically; other accounts still require pairing or explicit approval.
- Lets client-only iOS and Android devices sign in beside the scan/manual connection choices, register an endpoint-free signing identity, and load the same-account fleet without pairing each machine.
- Adds optional Somewhere fleet sign-in without changing the local default: onboarding and Settings › Fleet can request and verify an emailed magic-link code, `sessions account login|logout|status|key` exposes the same daemon-owned contract, and skipping sign-in leaves LAN and Tailscale Sessions fully usable.
- Registers a signed-in daemon's computer name, known LAN/Tailscale/relay endpoint hints, Ed25519 public key, version, and last-seen time in an owner-scoped Somewhere directory. Token rotation is atomic, machine requests have five-minute Ed25519/replay protection, heartbeats run every five minutes with a cloud-side rate limit, and health/doctor report the last registration without sending session content.
- Adds consent-by-possession fleet pairing: `sessions pair` and Settings › Fleet show a ten-minute, single-use QR/link containing every LAN and Tailscale endpoint; phones scan it, Macs accept it with `sessions machines connect <link>`, and the new device receives an independently revocable credential without a request/accept round trip.
- Makes signed-in Tailscale Macs reachable by default, with an opt-out in Fleet settings, automatic startup/network/sign-in checks, LAN → Tailscale HTTPS → direct Tailscale-IP fallback for saved machines and relays, and health/doctor reporting. The direct HTTP fallback binds only the Tailscale `100.64.0.0/10` address and still requires a revocable Sessions device credential.
- Makes Mac fleet access work from agents without granting network access to each lane: sessionsd now owns Bonjour discovery, machine connection, and relayed `--machine`, grep, and move traffic; macOS Local Network failures name the exact System Settings fix across the CLI, API, Fleet, and `sessions doctor`, while `--direct` remains available for explicit diagnostics.
- Gives macOS a responsible process for that permission: signed Darwin runtime binaries embed the Local Network and `_sessions._tcp` declarations, the launch agent is associated with Sessions.app, and Fleet onboarding plus Settings › Fleet › **Allow local network** trigger the daemon's first browse while the user is looking.
- Automatically retries a failed Rich Claude or Codex turn when the provider is unavailable or rate-limited, with a bounded five-attempt backoff, one exhaustion notification, visible countdown state, `sessions retry <id> [--stop]`, and no hidden second prompt when new input arrives.
- Recognizes a provider control that a session stops at before its first turn — Claude Code's folder-trust dialog, a login prompt — as a needs-you state instead of a not-started one, so sending a message no longer presses Enter on the highlighted choice and ends the session.
- Answers those controls from the conversation view: the exact choices the terminal is showing appear as buttons, without switching to the raw terminal.
- Opens the Resume picker quickly on a large history by dropping message-count work it never displayed, lists your own conversations ahead of work started by other lanes, and closes on Escape.
- Keeps search fast while a conversation is live by indexing only the new messages appended to a transcript rather than re-reading the whole file.
- Preserves a Rich Claude or Codex conversation after its session ends; the session's own copy is no longer deleted with the runtime record.
- Attributes a Codex conversation in history by its recorded thread identity or first message rather than the nearest rollout in the same folder, and no longer reports a completed turn as failed because of a provider startup warning.
- Groups the inbox by project — a folder, a Git checkout together with its worktrees, or a Somewhere project — with the sessions waiting on you pinned above them, pinned projects ordered before the rest, and finished or not-connected sessions folded under each project. `sessions projects name <folder> <name>` claims a folder under a name of your choosing; a failed project write now leaves the in-memory store unchanged, and a failed suggestion is reported instead of replaced with a guess.
- Adds an inbox keyboard model: Up/Down or j/k moves between rows, Home/End jumps to the first or last row, Down from the filter enters its results, Enter opens the focused session, and ⌘J jumps back to the inbox from anywhere.
- Adds `sessions fanout -- <request>` to start one lane per installed provider with the same request, join them, and report each lane's last line; `--with` selects each named provider once, `--no-wait` leaves the join to you, provider-discovery failures are reported instead of guessed around, and generated lane names stay valid UTF-8 when shortened.
- Lets a manager see the lanes it delegated, and only those, with `sessions team` and the Lanes panel: each lane's state, last line of work, and branch, never its whole conversation. `sessions team --all` rolls every manager up into lane, working, and needs-you counts and names the lanes waiting across them.
- Reports a lane whose runner is proven gone as **lost**, never running: `sessions lanes` and `sessions doctor` show the exact `sessions kill <id>` or `sessions resume <id>` action that resolves the record, and JSON callers receive the same answer in `lane_status`.
- Completes the breakout loop in the app: a lane's own view has a breadcrumb back to its manager, and **Hand back** is available both there and in the Lanes panel. It posts the lane's result into the manager's conversation attributed to the lane, names its branch, and returns to the manager without ending the lane.
- Runs delegated work on its own by default: a lane created from inside another session gets full access and its own Sessions-owned worktree on its own branch, so several lanes never trample one checkout. `--no-worktree` shares the manager's checkout; onboarding still offers "inherit my permissions" as the opt-out.
- Replaces provider-shaped permissions in New Session with one three-way **Access** choice: **Full access**, **Ask me**, or **Plan**. For Codex, **Ask me** now uses its untrusted approval policy so ordinary workspace commands ask instead of proceeding under the on-request policy.
- Routes a non-autonomous Rich lane's permission requests through Sessions instead of accepting them silently (Codex) or refusing them outright (Claude): the lane waits, shows as needing you, and is answered with Allow / Allow for this session / Decline in the app, `sessions approve <id>`, or the API. The request and answer are recorded with who decided, and `sessions cat <id>` includes those approval audit records.
- Treats a Rich lane that ends its turn with a question as needing you, so a blocked worker is visible from the inbox and the manager's Lanes panel.
- Restores more after a reboot: pinned sessions first, then every session you spoke to in the last day, up to eight, so a restart no longer loses the work you were in the middle of.
- Wakes a session that stayed paused after a reboot on first contact: opening it, sending it a message, or reading it live restarts its runner in place with the same conversation and folder, so the restart cap only decides what comes back at login by itself, never what you can reach.
- Removes a safely cleaned Sessions worktree from the normal `sessions worktrees` listing while retaining its append-only cleanup record in `sessions worktrees --all`.
- Makes terminal Codex conversations readable through `sessions cat` by binding the session to the rollout that contains its initial request, and says when Codex has not published that transcript yet instead of reporting an empty conversation.
- Keeps inherited `sessions send` attribution local to the daemon that owns the source session, so targeting a scratch or remote daemon does not attach a foreign session identity or block a valid delivery.
- Gives Android and iOS a compact phone connection screen that scans a one-time pairing code for immediate consent, with a paste-link fallback when the camera is unavailable.
- Makes phones inherit the connected host's machine-level choices instead of presenting host onboarding: runtime, connection, provider, delegated-access, and AI settings are labelled as host-owned and read-only, while device-local choices remain editable.
- Introduces each daemon with the operating system's user-facing computer name while preserving its stable machine identity and any explicit Fleet nickname.
- Adds the client-only iOS simulator application, including Bonjour/local-network declarations, generated Xcode project, and the build and pairing guide in `docs/IOS.md`.
- Keeps an app update installed when a session ends during its readiness check; rollback remains reserved for a baseline session that disappears or is still unreachable without a recorded user-end boundary.
- Removes the retired `sessions deploy` notice-only stub and no-action Coming soon cards, so the CLI and app show only operations that exist.
- Removes the opt-in generated recap; the local Daily activity journal and usage facts stay available without a model call.
- Loads secondary app views only when opened, reducing the initial JavaScript bundle from 701,455 bytes to about 584,000 bytes while keeping the total bundle within its existing budget.

Existing sessions keep running across the update. No session is ended or re-adopted to install it.

## Reliability follow-ups — 2026-09-10

Everything below landed after the 2026-09-09 baseline and is not yet in an
installed release. Each line ends with the commit that carries it.

### Delivery truth

- Recovers a structured runner's acknowledgment that arrives after the request asking for it has gone, so a message the runner accepted stops reading as permanently unknown; the upgrade is one-directional and evidence-only, a late refusal stays unknown, and nothing new goes on the wire. (fa9c32c)
- Closes the race where a cancelled or timed-out message waiter and the runner's answer could retire together, losing the answer that was the whole point of retaining it. (5193b05)
- Answers a refused send with its receipt: reading a recorded receipt is 200 whatever it says, `sessions send` states that the message was not delivered and whether retrying is safe, and `sessions send-status` prints the receipt instead of wrapping it in an error. (5309ab6)

### Fleet and network

- Reports the network failure that actually happened instead of inferring a Local Network denial from one errno, attributes each failure to the endpoint that was dialled, and stops persisting that guess across restarts. (a59f6fc)
- Stops presenting a stored nearby-access success as a live connection: a check that reaches nothing says so, a failed check keeps its own message on screen, and an older host's denial is attributed to that host rather than restated as a current verdict. (8db86c6)
- Keeps one unusable saved machine from costing the whole fleet: the machine listing and every relay to a healthy peer keep working, and the unusable row is listed as unreachable with the reason. (fdb3632)
- Publishes only the addresses this host would actually dial, so a saved endpoint carrying credentials or query text is never handed to a paired device. (fdb14fb)
- Says why one inherited machine is unavailable, as a snapshot of what the host observed rather than a permanent fact about that machine. (75d7bcf)
- Gives Fleet its one-line failure without discarding the rest of a daemon's answer, so a recovery instruction meant for another caller — the exact command that resumes a paused session — is no longer dropped on the way. (f1abda9)
- Stops one absent machine from holding up the whole fleet's history: each machine is read inside its own budget in parallel, local results appear first, and the response says which machines answered, timed out, or could not be reached. (dbbb0b9)
- Lets a live route win while a preferred one is still silent, so a dead preferred address costs a short grace instead of the full per-machine budget. (4986284)
- Says a Mac whose daemon is restarting is restarting, retries on its own for a minute, and never reports that silence as an empty search result. (9169ae2)
- Counts only a refused connection as a restart: a daemon that answered with an error is running, and its own message is shown instead. (53049ee)

### History speed and memory

- Stops re-reading a Codex conversation once its listing card is finished, and stops re-reading any conversation that has not changed since it was last described. (5770276, ffe3840)
- Gives back the terminal screen of a session nobody is watching after two quiet minutes, rebuilding it from what it kept on the next read or write, and bounds retained scrollback by size as well as by line count. (a438d33)
- Stops building a terminal screen at all for the session kinds that are never read as one — lanes and the structured providers — which keep their raw output and event history unchanged. (6623127)
- Keeps what a history listing already learned across a restart, so the first listing after an install no longer recounts every conversation; an entry is used only when the file still matches the size and modification time it was computed at. (68008ff)

### Phones and WebViews

- Builds the per-machine read budget from parts every shipped WebView has, instead of two APIs that are newer than the iOS the app is built for. (ad2b473, 7a15027)
- Ships what the declared phones can actually run: the bundle is built for the iOS version the mobile projects name, and the three APIs used anywhere in the app that are newer than that baseline — including one that is simply absent on the plain-HTTP addresses phones reach a Mac on — go through the guarded helpers that exist for them. (d87315b)

### CLI and app truth

- Says Usage is loading while it is loading: a machine whose report has not arrived reads as loading by name, one that timed out says so with a Try again, an older host is described as one only after its answer arrives, and the headline states how many machines it covers. (47f6598)
- Gives `sessions status --json` the same facts `sessions ls --json` carries, field for field and name for name, so an agent inspecting one session before sending to it reads what an agent listing every session reads; the card states working, exited, the last turn's reason and summary, an unreachable runner and a provider failure in the same words. (41464f9)

### Tests, fixtures and measurements only

- Measures what a listing and a preview cost on long conversations and what a daemon holding 560 records retains after it answers; both are the evidence the memory and history work above was built from. (9d50bdf, b4c9a63)
- Pins delivery evidence that was asserted only in prose: the retention bound under overflow, and a settled receipt surviving a new store, a restarted daemon and a replacement runner. (ba01855)
- Pins that resume follows provider identity rather than a folder or a name, that a paused lane refuses a read instead of answering quietly, and that a closed Usage view stops combining reports. (f30333e, 5651ef9, aee215e)
- Corrects three fixtures that passed for the wrong reason: a traversal fixture planted at the path it claimed, workspace paths encoded as the product encodes them, and paused-read CLI fixtures isolated under the Windows home too. (de6b26c, 64f24e9, 8eec039)

### Proof boundaries

- **Verified on the installed machines.** The MacBook and the Mac mini both run the state of this branch as of 5309ab6, installed on 2026-09-10. Every live session survived every install; none was ended or re-adopted. On the Mini, daemon resident memory fell from 2.37 GB to 768 MB, and a warm history listing fell from about 11 seconds to about 1 second.
- **Verified only in fixtures.** Everything else: the fleet timing and route behaviour, the delivery receipt paths, the restart and refusal wording, the persisted listing cache, the per-kind terminal screens, and every measurement quoted above. These run against synthetic conversations, fake runners and in-process daemons on a development Mac. Fixture numbers are not machine numbers, and no figure here was taken from a production daemon except the two in the line above.
- **Not on the installed machines.** The `sessions status --json` parity change (41464f9) landed after that install and has no device exposure yet.
- **No device proof at all.** Physical iPhone and Android hardware: the WebView baseline work was verified by reading the shipped bundle and the declared build targets, not by running the app on a phone. Windows: fixtures only, including the ones corrected above; no Windows machine was involved.

Existing sessions keep running across these changes. No session is ended or re-adopted to install them.
