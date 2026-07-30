# Product principles

Sessions is a local-first operations surface for durable Claude Code, Codex,
shell, and command sessions. These principles describe the public product
contract. Internal decision logs, launch notes, rejected options, and private
service plans are maintained outside this repository.

## Sessions are durable work

A window is not a process and a process is not a conversation. Closing a tab
must not silently end work. Ending a runtime must be explicit, and provider
history remains available for supported continuation and search.

## Users and agents are equal operators

Operational controls should have a stable CLI and JSON contract as well as a
clear native UI. An agent may inspect and operate Sessions with the user's
authority, but it does not bypass approvals, credentials, or destructive-action
boundaries.

## Local by default

The daemon listens on loopback by default. LAN, tailnet, backup, notification,
and hosted features are opt-in and must explain what leaves the machine.
Sessions makes no hidden model call and does not require a Sessions account for
local operation.

## Direct connections before relays

Native clients connect directly to a user-controlled Sessions host on localhost,
a trusted LAN, or a user-owned tailnet. A hosted service must be separately
identified and must not silently turn into a tunnel to a user's local machine.

## Calm, literal lifecycle language

Routine states such as finished, resumable, archived, or temporarily offline
are not emergencies. Red and blocking confirmation are reserved for meaningful
danger or irreversible loss. Labels such as Close tab, Set aside, End session, Continue,
Move, and Archive must describe their actual effect.

## Compatibility over forced lockstep

The app, daemon, CLI, and runners have separate lifetimes. Compatible versions
may cooperate; an update must not replace a live runner or discard an open
session. Protocol ranges, not display-version equality, determine compatibility.

## Open local runtime, explicit hosted boundary

The code that runs on a user's device, its local formats, and its versioned
protocols are buildable from this repository. Optional Somewhere account,
backup, relay, billing, abuse-control, and hosted-worker services may live in
private repositories and consume those public contracts. See
[`OPEN_SOURCE_BOUNDARY.md`](OPEN_SOURCE_BOUNDARY.md).
